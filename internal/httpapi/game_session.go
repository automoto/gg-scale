package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"

	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/gamesession"
	"github.com/ggscale/ggscale/internal/playerauth"
)

var (
	errGameSessionEnded     = errors.New("game session: ended")
	errGameSessionExpired   = errors.New("game session: expired")
	errGameSessionFull      = errors.New("game session: full")
	errGameSessionForbidden = errors.New("game session: caller not a member")
)

type gameSessionAddr struct {
	IP   string `json:"ip"`
	Port int    `json:"port"`
}

func (a gameSessionAddr) valid() bool {
	return a.Port >= 1 && a.Port <= 65535
}

// Request fields are schema-optional so the handlers keep ownership of their
// (cross-field) validation → 400, matching the pre-migration wire.
type gameSessionCreateRequest struct {
	TitleID    string          `json:"title_id,omitempty"`
	PublicAddr gameSessionAddr `json:"public_addr,omitempty"`
	Props      json.RawMessage `json:"props,omitempty"`
	MaxPlayers int             `json:"max_players,omitempty"`
	Private    bool            `json:"private,omitempty"`
}

type gameSessionJoinRequest struct {
	PublicAddr gameSessionAddr `json:"public_addr,omitempty"`
}

type gameSessionHeartbeatRequest struct {
	QoS *json.RawMessage `json:"qos,omitempty"`
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

type gameSessionOutput struct {
	Body gameSessionResponse
}

type gameSessionCreateInput struct {
	Body gameSessionCreateRequest
}

type gameSessionIDInput struct {
	ID string `path:"id"`
}

type gameSessionJoinInput struct {
	ID   string `path:"id"`
	Body gameSessionJoinRequest
}

type gameSessionHeartbeatInput struct {
	ID   string `path:"id"`
	Body gameSessionHeartbeatRequest
}

type gameSessionResolveInput struct {
	JoinCode string `query:"joinCode"`
}

type gameSessionResolveResult struct {
	SessionID string `json:"session_id"`
}

type gameSessionResolveOutput struct {
	Body gameSessionResolveResult
}

type gameSessionHeartbeatResult struct {
	OK    bool        `json:"ok"`
	Peers []peerEntry `json:"peers"`
}

type gameSessionHeartbeatOutput struct {
	Body gameSessionHeartbeatResult
}

func registerGameSessionRoutes(api huma.API, d Deps) {
	huma.Register(api, huma.Operation{
		OperationID:   "createGameSession",
		Method:        http.MethodPost,
		Path:          "/v1/game-session",
		Summary:       "Create a game session",
		Tags:          []string{"/v1"},
		Security:      playerSecurity,
		DefaultStatus: http.StatusCreated,
	}, gameSessionCreate(d))

	huma.Register(api, huma.Operation{
		OperationID: "resolveGameSession",
		Method:      http.MethodGet,
		Path:        "/v1/game-session",
		Summary:     "Resolve a game session by join code",
		Tags:        []string{"/v1"},
		Security:    playerSecurity,
	}, gameSessionResolve(d))

	huma.Register(api, huma.Operation{
		OperationID: "getGameSession",
		Method:      http.MethodGet,
		Path:        "/v1/game-session/{id}",
		Summary:     "Get a game session",
		Tags:        []string{"/v1"},
		Security:    playerSecurity,
	}, gameSessionGet(d))

	huma.Register(api, huma.Operation{
		OperationID: "joinGameSession",
		Method:      http.MethodPost,
		Path:        "/v1/game-session/{id}/join",
		Summary:     "Join a game session",
		Tags:        []string{"/v1"},
		Security:    playerSecurity,
	}, gameSessionJoin(d))

	huma.Register(api, huma.Operation{
		OperationID: "heartbeatGameSession",
		Method:      http.MethodPost,
		Path:        "/v1/game-session/{id}/heartbeat",
		Summary:     "Heartbeat a game session peer",
		Tags:        []string{"/v1"},
		Security:    playerSecurity,
	}, gameSessionHeartbeat(d))

	huma.Register(api, huma.Operation{
		OperationID:   "leaveGameSession",
		Method:        http.MethodDelete,
		Path:          "/v1/game-session/{id}",
		Summary:       "Leave (or, for the host, end) a game session",
		Tags:          []string{"/v1"},
		Security:      playerSecurity,
		DefaultStatus: http.StatusNoContent,
	}, gameSessionLeave(d))
}

func gameSessionCreate(d Deps) func(context.Context, *gameSessionCreateInput) (*gameSessionOutput, error) {
	return func(ctx context.Context, in *gameSessionCreateInput) (*gameSessionOutput, error) {
		req := in.Body
		projectID, ok := db.ProjectFromContext(ctx)
		if !ok {
			return nil, huma.Error400BadRequest("api key has no project pin")
		}
		hostUserID, ok := playerauth.IDFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("no player")
		}
		if !req.PublicAddr.valid() {
			return nil, huma.Error400BadRequest("public_addr.port out of range")
		}
		if req.MaxPlayers > gamesession.MaxPlayersLimit {
			return nil, huma.Error400BadRequest("max_players exceeds limit")
		}
		if d.GameSessions == nil {
			return nil, huma.Error503ServiceUnavailable("game sessions unavailable")
		}

		created, err := d.GameSessions.Create(ctx, gamesession.CreateParams{
			ProjectID:    projectID,
			HostPlayerID: hostUserID,
			TitleID:      req.TitleID,
			Props:        req.Props,
			MaxPlayers:   req.MaxPlayers,
			Private:      req.Private,
			Members: []gamesession.Member{{
				PlayerID: hostUserID,
				Addr: &gamesession.Addr{
					IP:   req.PublicAddr.IP,
					Port: int32(req.PublicAddr.Port), //nolint:gosec // validated: 1–65535
				},
			}},
		})
		switch {
		case errors.Is(err, gamesession.ErrProjectCapped):
			return nil, huma.Error429TooManyRequests("session limit reached for this project")
		case err != nil:
			return nil, serverError(ctx, "game session create", err)
		}

		return &gameSessionOutput{Body: gameSessionResponse{
			SessionID: created.SessionID,
			JoinCode:  created.JoinCode,
			State:     created.State,
			Peers:     buildPeerEntries(created.Peers),
		}}, nil
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

func gameSessionGet(d Deps) func(context.Context, *gameSessionIDInput) (*gameSessionOutput, error) {
	return func(ctx context.Context, in *gameSessionIDInput) (*gameSessionOutput, error) {
		sessionID := in.ID
		callerID, ok := playerauth.IDFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("no player")
		}
		projectID, ok := db.ProjectFromContext(ctx)
		if !ok {
			return nil, huma.Error400BadRequest("api key has no project pin")
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
				return nil, huma.Error404NotFound("not found")
			}
			return nil, serverError(ctx, "game session get: tx", err)
		}

		return &gameSessionOutput{Body: gameSessionResponse{
			SessionID: sess.ID,
			JoinCode:  sess.JoinCode,
			State:     sess.State,
			Peers:     buildPeerEntries(peers),
		}}, nil
	}
}

func gameSessionResolve(d Deps) func(context.Context, *gameSessionResolveInput) (*gameSessionResolveOutput, error) {
	return func(ctx context.Context, in *gameSessionResolveInput) (*gameSessionResolveOutput, error) {
		joinCode := in.JoinCode
		if joinCode == "" {
			return nil, huma.Error400BadRequest("joinCode required")
		}
		callerID, ok := playerauth.IDFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("no player")
		}
		projectID, ok := db.ProjectFromContext(ctx)
		if !ok {
			return nil, huma.Error400BadRequest("api key has no project pin")
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
				return nil, huma.Error404NotFound("not found")
			}
			return nil, serverError(ctx, "game session resolve: tx", err)
		}

		return &gameSessionResolveOutput{Body: gameSessionResolveResult{SessionID: row.ID}}, nil
	}
}

func gameSessionJoin(d Deps) func(context.Context, *gameSessionJoinInput) (*gameSessionOutput, error) {
	return func(ctx context.Context, in *gameSessionJoinInput) (*gameSessionOutput, error) {
		req := in.Body
		sessionID := in.ID
		joinerID, ok := playerauth.IDFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("no player")
		}
		projectID, ok := db.ProjectFromContext(ctx)
		if !ok {
			return nil, huma.Error400BadRequest("api key has no project pin")
		}
		if !req.PublicAddr.valid() {
			return nil, huma.Error400BadRequest("public_addr.port out of range")
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
			return nil, huma.Error404NotFound("not found")
		case errors.Is(err, errGameSessionEnded), errors.Is(err, errGameSessionExpired):
			return nil, huma.Error410Gone("session no longer joinable")
		case errors.Is(err, errGameSessionFull):
			return nil, huma.Error409Conflict("session is full")
		case err != nil:
			return nil, serverError(ctx, "game session join: tx", err)
		}

		return &gameSessionOutput{Body: gameSessionResponse{
			SessionID: sess.ID,
			JoinCode:  sess.JoinCode,
			State:     sess.State,
			Peers:     buildPeerEntries(peers),
		}}, nil
	}
}

func gameSessionHeartbeat(d Deps) func(context.Context, *gameSessionHeartbeatInput) (*gameSessionHeartbeatOutput, error) {
	return func(ctx context.Context, in *gameSessionHeartbeatInput) (*gameSessionHeartbeatOutput, error) {
		req := in.Body
		sessionID := in.ID
		callerID, ok := playerauth.IDFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("no player")
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
			return nil, serverError(ctx, "game session heartbeat: tx", err)
		}
		if !isMember {
			return nil, huma.Error404NotFound("not a member of this session")
		}

		return &gameSessionHeartbeatOutput{Body: gameSessionHeartbeatResult{OK: true, Peers: buildPeerEntries(peers)}}, nil
	}
}

func gameSessionLeave(d Deps) func(context.Context, *gameSessionIDInput) (*struct{}, error) {
	return func(ctx context.Context, in *gameSessionIDInput) (*struct{}, error) {
		sessionID := in.ID
		callerID, ok := playerauth.IDFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("no player")
		}
		projectID, ok := db.ProjectFromContext(ctx)
		if !ok {
			return nil, huma.Error400BadRequest("api key has no project pin")
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
				return nil, huma.Error404NotFound("not found")
			}
			return nil, serverError(ctx, "game session leave: tx", err)
		}

		return nil, nil
	}
}
