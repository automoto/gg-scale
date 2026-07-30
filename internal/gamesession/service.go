// Package gamesession owns game-session creation so both the player-facing
// HTTP handler and the matchmaker worker can mint sessions through one code
// path (join-code generation, per-project open-session cap, peer seeding).
package gamesession

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/quota"
	"github.com/ggscale/ggscale/internal/webutil"
)

const (
	// DefaultTTL is a session's lifetime window. Member heartbeats slide it
	// forward (see HeartbeatExtendThreshold), so an active session lives as
	// long as its match while an idle or abandoned one frees its cap slot
	// within the hour. Hosts should still DELETE the session when the match
	// ends — that frees the slot immediately.
	DefaultTTL = time.Hour
	// HeartbeatExtendThreshold gates the sliding extension: a heartbeat
	// pushes expires_at out to now+DefaultTTL only when less than this
	// remains, so steady heartbeats don't rewrite the row on every call.
	HeartbeatExtendThreshold = 30 * time.Minute
	// MatchPendingTTL is the initial lifetime of a matchmade session. It
	// covers the window between match formation and the first join; the
	// first join extends the session to DefaultTTL. Keeps sessions nobody
	// ever joins from holding an open-session cap slot for hours.
	MatchPendingTTL     = 10 * time.Minute
	joinCodeAlphabet    = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // no I O 0 1 (ambiguous)
	joinCodeLen         = 6
	joinCodeMaxAttempts = 5
	// SessionsHardCap is the absolute per-project live-session ceiling for
	// every tenant, enforced or not — it protects the server, not the
	// business model. Enforced tenants additionally sit under the (lower)
	// per-class quota.Limits.OpenSessionsPerProject.
	SessionsHardCap = 10_000
	// MaxPlayersLimit caps a single session's roster size.
	MaxPlayersLimit = 64
)

// ErrProjectCapped is returned by Create when the project is at its
// open-session limit — the hard cap, or the class quota for enforced
// tenants. The quota case additionally wraps *quota.ErrQuotaExceeded so
// callers can label the rejection metric.
var ErrProjectCapped = errors.New("game session: project cap reached")

// Addr is a peer's announced public endpoint.
type Addr struct {
	IP   string
	Port int32
}

// Member is a player seeded onto the session roster at creation. Addr is
// optional: matchmade members have no endpoint until they join/heartbeat.
type Member struct {
	PlayerID int64
	Addr     *Addr
}

// CreateParams describes a session to create. HostPlayerID must be present
// in Members.
type CreateParams struct {
	ProjectID    int64
	HostPlayerID int64
	TitleID      string
	Props        json.RawMessage
	MaxPlayers   int
	Private      bool
	Members      []Member
	TTL          time.Duration
}

// Created is the persisted session view.
type Created struct {
	SessionID string
	JoinCode  string
	State     string
	ExpiresAt time.Time
	Peers     []sqlcgen.ListGameSessionPeersRow
}

// Service creates game sessions inside the caller's tenant scope.
type Service struct {
	pool *db.Pool
}

// NewService returns a Service bound to the app pool.
func NewService(pool *db.Pool) *Service {
	return &Service{pool: pool}
}

// Create mints a session with a unique join code, enforcing the per-project
// open-session cap, and seeds the given members onto the roster. The caller
// must supply a tenant-scoped ctx.
func (s *Service) Create(ctx context.Context, p CreateParams) (*Created, error) {
	if p.MaxPlayers <= 0 {
		p.MaxPlayers = 2
	}
	if p.MaxPlayers > MaxPlayersLimit {
		return nil, fmt.Errorf("game session: max_players %d exceeds limit %d", p.MaxPlayers, MaxPlayersLimit)
	}
	if p.TTL <= 0 {
		p.TTL = DefaultTTL
	}
	props := p.Props
	if len(props) == 0 {
		props = json.RawMessage("{}")
	}

	sessionID, err := webutil.RandomHex("gs_", 16)
	if err != nil {
		return nil, fmt.Errorf("game session: rand id: %w", err)
	}
	expires := pgtype.Timestamptz{Time: time.Now().Add(p.TTL), Valid: true}

	var (
		sess  sqlcgen.CreateGameSessionRow
		peers []sqlcgen.ListGameSessionPeersRow
	)
	// Retry on the (astronomically rare) join-code unique collision.
	for attempt := 0; ; attempt++ {
		joinCode, jerr := newJoinCode()
		if jerr != nil {
			return nil, fmt.Errorf("game session: join code: %w", jerr)
		}
		err = s.pool.Q(ctx, func(tx pgx.Tx) error {
			q := sqlcgen.New(tx)
			// Serialize creation per project so the open-session cap can't
			// be raced past, then count + insert in the same transaction.
			if qerr := q.LockProjectForGameSessionCreate(ctx, p.ProjectID); qerr != nil {
				return qerr
			}
			openCount, qerr := q.CountOpenGameSessionsForProject(ctx, p.ProjectID)
			if qerr != nil {
				return qerr
			}
			if qerr := checkOpenSessionLimit(ctx, q, openCount); qerr != nil {
				return qerr
			}
			sess, qerr = q.CreateGameSession(ctx, sqlcgen.CreateGameSessionParams{
				ID:           sessionID,
				JoinCode:     joinCode,
				ProjectID:    p.ProjectID,
				TitleID:      p.TitleID,
				HostPlayerID: p.HostPlayerID,
				Props:        []byte(props),
				MaxPlayers:   int32(p.MaxPlayers), //nolint:gosec // validated ≤ MaxPlayersLimit above
				Private:      p.Private,
				ExpiresAt:    expires,
			})
			if qerr != nil {
				return qerr
			}
			for _, m := range p.Members {
				var ip *string
				var port *int32
				if m.Addr != nil {
					ip, port = &m.Addr.IP, &m.Addr.Port
				}
				if qerr := q.UpsertGameSessionPeer(ctx, sqlcgen.UpsertGameSessionPeerParams{
					SessionID: sessionID,
					PlayerID:  m.PlayerID,
					Ip:        ip,
					Port:      port,
					Qos:       []byte("{}"),
				}); qerr != nil {
					return qerr
				}
			}
			peers, qerr = q.ListGameSessionPeers(ctx, sessionID)
			return qerr
		})
		if err != nil && webutil.IsUniqueViolation(err) && attempt < joinCodeMaxAttempts {
			continue
		}
		break
	}
	if err != nil {
		return nil, err
	}
	return &Created{SessionID: sess.ID, JoinCode: sess.JoinCode, State: sess.State, ExpiresAt: expires.Time, Peers: peers}, nil
}

// checkOpenSessionLimit gates session creation on the project's open-session
// count: the absolute hard cap applies to everyone; enforced tenants also sit
// under their class quota. Runs inside the create transaction, after the
// per-project lock, so the check-then-insert cannot be raced past. Both
// rejections satisfy errors.Is(err, ErrProjectCapped); the quota one also
// carries *quota.ErrQuotaExceeded for the rejection metric.
func checkOpenSessionLimit(ctx context.Context, q *sqlcgen.Queries, openCount int64) error {
	if openCount >= SessionsHardCap {
		return ErrProjectCapped
	}
	qc, err := q.GetTenantQuotaContext(ctx)
	if err != nil {
		return fmt.Errorf("game session: tenant quota context: %w", err)
	}
	if !qc.EnforceQuotas {
		return nil
	}
	limits, err := quota.ResolveSnapshot(int(qc.Tier), qc.Overrides)
	if err != nil {
		return fmt.Errorf("game session: %w", err)
	}
	if err := limits.CheckOpenSessions(openCount); err != nil {
		return fmt.Errorf("%w: %w", ErrProjectCapped, err)
	}
	return nil
}

// EffectiveState is the state a client should see. A session past its expiry
// must never read as live, even while the row lingers before GC removes it.
func EffectiveState(state string, expiresAt time.Time) string {
	if state != "ended" && !expiresAt.After(time.Now()) {
		return "expired"
	}
	return state
}

// IsMatchmade reports whether the session props carry the matchmade flag the
// matchmaker adapter sets. Unparsable props count as not matchmade.
func IsMatchmade(props []byte) bool {
	var p struct {
		Matchmade bool `json:"matchmade"`
	}
	if err := json.Unmarshal(props, &p); err != nil {
		return false
	}
	return p.Matchmade
}

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
