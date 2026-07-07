package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/playerauth"
	"github.com/ggscale/ggscale/internal/webutil"
)

const (
	gameSessionTTL            = 4 * time.Hour
	joinCodeAlphabet          = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // no I O 0 1 (ambiguous)
	joinCodeLen               = 6
	joinCodeMaxAttempts       = 5
	maxOpenSessionsPerProject = 100
	maxPlayersLimit           = 64
)

var (
	errGameSessionEnded     = errors.New("game session: ended")
	errGameSessionExpired   = errors.New("game session: expired")
	errGameSessionFull      = errors.New("game session: full")
	errGameSessionCapped    = errors.New("game session: project cap reached")
	errGameSessionForbidden = errors.New("game session: caller not a member")
)

func newJoinCode() (string, error) {
	b := make([]byte, joinCodeLen)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(joinCodeAlphabet))))
		if err != nil {
			return "", err
		}
		b[i] = joinCodeAlphabet[n.Int64()]
	}
	return string(b), nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

type gameSessionAddr struct {
	IP   string `json:"ip"`
	Port int    `json:"port"`
}

func (a gameSessionAddr) valid() bool {
	return a.Port >= 1 && a.Port <= 65535
}

type gameSessionCreateRequest struct {
	TitleID    string          `json:"title_id"`
	PublicAddr gameSessionAddr `json:"public_addr"`
	Props      json.RawMessage `json:"props"`
	MaxPlayers int             `json:"max_players"`
	Private    bool            `json:"private"`
}

type gameSessionJoinRequest struct {
	PublicAddr gameSessionAddr `json:"public_addr"`
}

type gameSessionHeartbeatRequest struct {
	QoS *json.RawMessage `json:"qos"`
}

type peerEntry struct {
	PlayerID int64           `json:"player_id"`
	XUID     string          `json:"xuid,omitempty"`
	Addr     gameSessionAddr `json:"addr"`
	Relay    any             `json:"relay"`
}

type gameSessionResponse struct {
	SessionID string      `json:"session_id"`
	JoinCode  string      `json:"join_code"`
	State     string      `json:"state"`
	Peers     []peerEntry `json:"peers"`
}

func buildPeerEntries(rows []sqlcgen.ListGameSessionPeersRow) []peerEntry {
	out := make([]peerEntry, 0, len(rows))
	for _, r := range rows {
		p := peerEntry{PlayerID: r.PlayerID}
		if r.Xuid != nil {
			p.XUID = *r.Xuid
		}
		if r.Ip != nil {
			p.Addr.IP = *r.Ip
		}
		if r.Port != nil {
			p.Addr.Port = int(*r.Port)
		}
		out = append(out, p)
	}
	return out
}

// POST /v1/game-session
func gameSessionCreateHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req gameSessionCreateRequest
		if !decodeJSON(w, r, &req) {
			return
		}

		ctx := r.Context()
		projectID, ok := db.ProjectFromContext(ctx)
		if !ok {
			http.Error(w, "api key has no project pin", http.StatusBadRequest)
			return
		}
		hostUserID, ok := playerauth.IDFromContext(ctx)
		if !ok {
			http.Error(w, "no player", http.StatusUnauthorized)
			return
		}
		if !req.PublicAddr.valid() {
			http.Error(w, "public_addr.port out of range", http.StatusBadRequest)
			return
		}
		maxPlayers := req.MaxPlayers
		if maxPlayers <= 0 {
			maxPlayers = 2
		}
		if maxPlayers > maxPlayersLimit {
			http.Error(w, "max_players exceeds limit", http.StatusBadRequest)
			return
		}
		props := req.Props
		if len(props) == 0 {
			props = json.RawMessage("{}")
		}

		sessionID, err := webutil.RandomHex("gs_", 16)
		if err != nil {
			webutil.InternalError(w, "game session create: rand id", err)
			return
		}
		now := time.Now()
		ip := req.PublicAddr.IP
		port := int32(req.PublicAddr.Port) //nolint:gosec // validated: 1–65535

		var (
			sess  sqlcgen.CreateGameSessionRow
			peers []sqlcgen.ListGameSessionPeersRow
		)
		// Retry on the (astronomically rare) join-code unique collision.
		for attempt := 0; ; attempt++ {
			joinCode, jerr := newJoinCode()
			if jerr != nil {
				webutil.InternalError(w, "game session create: join code", jerr)
				return
			}
			err = d.Pool.Q(ctx, func(tx pgx.Tx) error {
				q := sqlcgen.New(tx)
				// Serialize creation per project so the open-session cap can't
				// be raced past, then count + insert in the same transaction.
				if qerr := q.LockProjectForGameSessionCreate(ctx, projectID); qerr != nil {
					return qerr
				}
				openCount, qerr := q.CountOpenGameSessionsForProject(ctx, projectID)
				if qerr != nil {
					return qerr
				}
				if openCount >= maxOpenSessionsPerProject {
					return errGameSessionCapped
				}
				sess, qerr = q.CreateGameSession(ctx, sqlcgen.CreateGameSessionParams{
					ID:           sessionID,
					JoinCode:     joinCode,
					ProjectID:    projectID,
					TitleID:      req.TitleID,
					HostPlayerID: hostUserID,
					Props:        []byte(props),
					MaxPlayers:   int32(maxPlayers), //nolint:gosec // validated: ≤64
					Private:      req.Private,
					ExpiresAt:    pgtype.Timestamptz{Time: now.Add(gameSessionTTL), Valid: true},
				})
				if qerr != nil {
					return qerr
				}
				if qerr := q.UpsertGameSessionPeer(ctx, sqlcgen.UpsertGameSessionPeerParams{
					SessionID: sessionID,
					PlayerID:  hostUserID,
					Ip:        &ip,
					Port:      &port,
					Qos:       []byte("{}"),
				}); qerr != nil {
					return qerr
				}
				peers, qerr = q.ListGameSessionPeers(ctx, sessionID)
				return qerr
			})
			if err != nil && isUniqueViolation(err) && attempt < joinCodeMaxAttempts {
				continue
			}
			break
		}
		switch {
		case errors.Is(err, errGameSessionCapped):
			http.Error(w, "session limit reached for this project", http.StatusTooManyRequests)
			return
		case err != nil:
			webutil.InternalError(w, "game session create: tx", err)
			return
		}

		writeJSONStatus(w, http.StatusCreated, gameSessionResponse{
			SessionID: sess.ID,
			JoinCode:  sess.JoinCode,
			State:     sess.State,
			Peers:     buildPeerEntries(peers),
		})
	}
}

// canAccessPrivateSession reports whether callerID may see or join a private
// session: the host, an existing peer, or the holder of an unexpired invite.
func canAccessPrivateSession(ctx context.Context, q *sqlcgen.Queries, sessionID string, hostPlayerID, callerID int64) (bool, error) {
	if callerID == hostPlayerID {
		return true, nil
	}
	member, err := q.IsGameSessionMember(ctx, sqlcgen.IsGameSessionMemberParams{
		SessionID: sessionID, PlayerID: callerID,
	})
	if err != nil {
		return false, err
	}
	if member {
		return true, nil
	}
	invites, err := q.CountPendingGameInviteForSessionPlayer(ctx, sqlcgen.CountPendingGameInviteForSessionPlayerParams{
		SessionID: sessionID, ToPlayerID: callerID,
	})
	if err != nil {
		return false, err
	}
	return invites > 0, nil
}

// GET /v1/game-session/{id}
func gameSessionGetHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		sessionID := chi.URLParam(r, "id")
		callerID, ok := playerauth.IDFromContext(ctx)
		if !ok {
			http.Error(w, "no player", http.StatusUnauthorized)
			return
		}
		projectID, ok := db.ProjectFromContext(ctx)
		if !ok {
			http.Error(w, "api key has no project pin", http.StatusBadRequest)
			return
		}

		var (
			sess  sqlcgen.GetGameSessionRow
			peers []sqlcgen.ListGameSessionPeersRow
		)
		err := d.Pool.Q(ctx, func(tx pgx.Tx) error {
			q := sqlcgen.New(tx)
			var qerr error
			sess, qerr = q.GetGameSession(ctx, sqlcgen.GetGameSessionParams{ProjectID: projectID, ID: sessionID})
			if qerr != nil {
				return qerr
			}
			// The roster (peer public IP:port) is member-only — the host or an
			// existing peer. A non-member gets 404 so neither the roster nor
			// the session's existence leaks (mirrors the heartbeat handler).
			member, qerr := canAccessPrivateSession(ctx, q, sessionID, sess.HostPlayerID, callerID)
			if qerr != nil {
				return qerr
			}
			if !member {
				return errGameSessionForbidden
			}
			peers, qerr = q.ListGameSessionPeers(ctx, sessionID)
			return qerr
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, errGameSessionForbidden) {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			webutil.InternalError(w, "game session get: tx", err)
			return
		}

		writeJSON(w, gameSessionResponse{
			SessionID: sess.ID,
			JoinCode:  sess.JoinCode,
			State:     sess.State,
			Peers:     buildPeerEntries(peers),
		})
	}
}

// GET /v1/game-session?joinCode=<code>
func gameSessionResolveHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		joinCode := r.URL.Query().Get("joinCode")
		if joinCode == "" {
			http.Error(w, "joinCode required", http.StatusBadRequest)
			return
		}
		callerID, ok := playerauth.IDFromContext(ctx)
		if !ok {
			http.Error(w, "no player", http.StatusUnauthorized)
			return
		}
		projectID, ok := db.ProjectFromContext(ctx)
		if !ok {
			http.Error(w, "api key has no project pin", http.StatusBadRequest)
			return
		}

		var row sqlcgen.GetGameSessionByJoinCodeRow
		err := d.Pool.Q(ctx, func(tx pgx.Tx) error {
			q := sqlcgen.New(tx)
			var qerr error
			row, qerr = q.GetGameSessionByJoinCode(ctx, sqlcgen.GetGameSessionByJoinCodeParams{ProjectID: projectID, JoinCode: joinCode})
			if qerr != nil {
				return qerr
			}
			// A private session is not discoverable by join code alone: only
			// the host, an existing member, or an invitee may resolve it.
			if row.Private {
				allowed, aerr := canAccessPrivateSession(ctx, q, row.ID, row.HostPlayerID, callerID)
				if aerr != nil {
					return aerr
				}
				if !allowed {
					return errGameSessionForbidden
				}
			}
			return nil
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, errGameSessionForbidden) {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			webutil.InternalError(w, "game session resolve: tx", err)
			return
		}

		writeJSON(w, map[string]string{"session_id": row.ID})
	}
}

// POST /v1/game-session/{id}/join
func gameSessionJoinHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req gameSessionJoinRequest
		if !decodeJSON(w, r, &req) {
			return
		}

		ctx := r.Context()
		sessionID := chi.URLParam(r, "id")
		joinerID, ok := playerauth.IDFromContext(ctx)
		if !ok {
			http.Error(w, "no player", http.StatusUnauthorized)
			return
		}
		projectID, ok := db.ProjectFromContext(ctx)
		if !ok {
			http.Error(w, "api key has no project pin", http.StatusBadRequest)
			return
		}
		if !req.PublicAddr.valid() {
			http.Error(w, "public_addr.port out of range", http.StatusBadRequest)
			return
		}

		ip := req.PublicAddr.IP
		port := int32(req.PublicAddr.Port) //nolint:gosec // validated: 1–65535

		var (
			sess  sqlcgen.GetGameSessionForUpdateRow
			peers []sqlcgen.ListGameSessionPeersRow
		)
		err := d.Pool.Q(ctx, func(tx pgx.Tx) error {
			q := sqlcgen.New(tx)
			var qerr error
			// FOR UPDATE serializes concurrent joins for this session so the
			// capacity check below can't be raced.
			sess, qerr = q.GetGameSessionForUpdate(ctx, sqlcgen.GetGameSessionForUpdateParams{ProjectID: projectID, ID: sessionID})
			if qerr != nil {
				return qerr
			}
			// A private session admits only the host, an existing member, or an
			// invitee — even a caller who somehow learned the session id.
			if sess.Private {
				allowed, aerr := canAccessPrivateSession(ctx, q, sessionID, sess.HostPlayerID, joinerID)
				if aerr != nil {
					return aerr
				}
				if !allowed {
					return errGameSessionForbidden
				}
			}
			if sess.State == "ended" {
				return errGameSessionEnded
			}
			if sess.ExpiresAt.Time.Before(time.Now()) {
				return errGameSessionExpired
			}
			active, qerr := q.CountActiveGameSessionPeers(ctx, sqlcgen.CountActiveGameSessionPeersParams{
				SessionID:     sessionID,
				ExcludeUserID: joinerID,
			})
			if qerr != nil {
				return qerr
			}
			if active >= int64(sess.MaxPlayers) {
				return errGameSessionFull
			}
			if qerr := q.UpsertGameSessionPeer(ctx, sqlcgen.UpsertGameSessionPeerParams{
				SessionID: sessionID,
				PlayerID:  joinerID,
				Ip:        &ip,
				Port:      &port,
				Qos:       []byte("{}"),
			}); qerr != nil {
				return qerr
			}
			peers, qerr = q.ListGameSessionPeers(ctx, sessionID)
			return qerr
		})
		switch {
		case errors.Is(err, pgx.ErrNoRows), errors.Is(err, errGameSessionForbidden):
			http.Error(w, "not found", http.StatusNotFound)
			return
		case errors.Is(err, errGameSessionEnded), errors.Is(err, errGameSessionExpired):
			http.Error(w, "session no longer joinable", http.StatusGone)
			return
		case errors.Is(err, errGameSessionFull):
			http.Error(w, "session is full", http.StatusConflict)
			return
		case err != nil:
			webutil.InternalError(w, "game session join: tx", err)
			return
		}

		writeJSON(w, gameSessionResponse{
			SessionID: sess.ID,
			JoinCode:  sess.JoinCode,
			State:     sess.State,
			Peers:     buildPeerEntries(peers),
		})
	}
}

// POST /v1/game-session/{id}/heartbeat
func gameSessionHeartbeatHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req gameSessionHeartbeatRequest
		if !decodeJSON(w, r, &req) {
			return
		}

		ctx := r.Context()
		sessionID := chi.URLParam(r, "id")
		callerID, ok := playerauth.IDFromContext(ctx)
		if !ok {
			http.Error(w, "no player", http.StatusUnauthorized)
			return
		}

		// nil qos preserves the stored value (see TouchGameSessionPeer).
		var qos []byte
		if req.QoS != nil {
			qos = []byte(*req.QoS)
		}

		var (
			peers    []sqlcgen.ListGameSessionPeersRow
			isMember bool
		)
		err := d.Pool.Q(ctx, func(tx pgx.Tx) error {
			q := sqlcgen.New(tx)
			affected, qerr := q.TouchGameSessionPeer(ctx, sqlcgen.TouchGameSessionPeerParams{
				Qos:       qos,
				SessionID: sessionID,
				PlayerID:  callerID,
			})
			if qerr != nil {
				return qerr
			}
			// Zero rows touched → the caller isn't a member of this session.
			// Stop before listing peers so a non-member can't poll the roster.
			if affected == 0 {
				return nil
			}
			isMember = true
			if _, qerr := q.PruneStaleGameSessionPeers(ctx, sessionID); qerr != nil {
				return qerr
			}
			peers, qerr = q.ListGameSessionPeers(ctx, sessionID)
			return qerr
		})
		if err != nil {
			webutil.InternalError(w, "game session heartbeat: tx", err)
			return
		}
		if !isMember {
			http.Error(w, "not a member of this session", http.StatusNotFound)
			return
		}

		writeJSON(w, map[string]any{"ok": true, "peers": buildPeerEntries(peers)})
	}
}

// DELETE /v1/game-session/{id}
func gameSessionLeaveHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		sessionID := chi.URLParam(r, "id")
		callerID, ok := playerauth.IDFromContext(ctx)
		if !ok {
			http.Error(w, "no player", http.StatusUnauthorized)
			return
		}
		projectID, ok := db.ProjectFromContext(ctx)
		if !ok {
			http.Error(w, "api key has no project pin", http.StatusBadRequest)
			return
		}

		err := d.Pool.Q(ctx, func(tx pgx.Tx) error {
			q := sqlcgen.New(tx)
			sess, qerr := q.GetGameSession(ctx, sqlcgen.GetGameSessionParams{ProjectID: projectID, ID: sessionID})
			if qerr != nil {
				return qerr
			}
			if sess.HostPlayerID == callerID {
				// Host leaving → end the session and clear its roster so peer
				// rows don't linger until GC.
				if qerr := q.UpdateGameSessionState(ctx, sqlcgen.UpdateGameSessionStateParams{
					State:     "ended",
					ProjectID: projectID,
					ID:        sessionID,
				}); qerr != nil {
					return qerr
				}
				return q.DeleteAllGameSessionPeers(ctx, sessionID)
			}
			// Joiner leaving → remove only their peer row.
			return q.DeleteGameSessionPeer(ctx, sqlcgen.DeleteGameSessionPeerParams{
				SessionID: sessionID,
				PlayerID:  callerID,
			})
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			webutil.InternalError(w, "game session leave: tx", err)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}
