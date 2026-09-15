package party

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/automoto/gg-scale/internal/db"
	"github.com/automoto/gg-scale/internal/webutil"
	"github.com/jackc/pgx/v5"
)

// Store serializes all party changes with the party row lock.
type Store struct{ pool *db.Pool }

// NewStore uses the primary database for recovery and mutations.
func NewStore(pool *db.Pool) *Store { return &Store{pool: pool} }

func load(ctx context.Context, tx pgx.Tx, project, id int64) (*Party, error) {
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT to_jsonb(p) FROM parties p WHERE project_id=$1 AND id=$2 FOR UPDATE`, project, id).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var p Party
	if err = json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	err = tx.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(m)||jsonb_build_object('ticket_id',COALESCE((SELECT t.id FROM matchmaking_tickets t WHERE t.party_id=m.party_id AND t.player_id=m.player_id ORDER BY t.created_at DESC,t.id DESC LIMIT 1),0)) ORDER BY joined_at,player_id),'[]') FROM party_members m WHERE party_id=$1`, id).Scan(&raw)
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(raw, &p.Members); err != nil {
		return nil, err
	}
	return &p, nil
}

func memberIndex(p *Party, player int64) int {
	return slices.IndexFunc(p.Members, func(m Member) bool { return m.PlayerID == player })
}

func save(ctx context.Context, tx pgx.Tx, p *Party) error {
	settings, err := json.Marshal(p.Settings)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE parties SET leader_id=$2,state=$3,version=$4,roster_version=$5,settings=$6,current_queue_entry_id=$7,last_match_id=$8,updated_at=now(),closed_at=CASE WHEN $3='closed' THEN now() ELSE NULL END WHERE id=$1`, p.ID, p.LeaderID, p.State, p.Version, p.RosterVersion, settings, p.CurrentQueueEntryID, p.LastMatchID)
	if err != nil {
		return err
	}
	ids := make([]int64, 0, len(p.Members))
	for _, m := range p.Members {
		ids = append(ids, m.PlayerID)
		raw, err := json.Marshal(m)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO party_members(tenant_id,project_id,party_id,player_id,ready_version,string_properties,numeric_properties,attributes,joined_at,last_seen_at,disconnect_deadline)
   SELECT tenant_id,project_id,id,$2,$3,COALESCE($4::jsonb->'string_properties','{}'),COALESCE($4::jsonb->'numeric_properties','{}'),COALESCE($4::jsonb->'attributes','{}'),$5,$6,$7 FROM parties WHERE id=$1
   ON CONFLICT(party_id,player_id) DO UPDATE SET ready_version=EXCLUDED.ready_version,string_properties=EXCLUDED.string_properties,numeric_properties=EXCLUDED.numeric_properties,attributes=EXCLUDED.attributes,last_seen_at=EXCLUDED.last_seen_at,disconnect_deadline=EXCLUDED.disconnect_deadline`, p.ID, m.PlayerID, m.ReadyVersion, raw, m.JoinedAt, m.LastSeenAt, m.DisconnectDeadline)
		if err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `DELETE FROM party_members WHERE party_id=$1 AND NOT(player_id=ANY($2::bigint[]))`, p.ID, ids)
	return err
}

func checkPlayer(ctx context.Context, tx pgx.Tx, project, player int64) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('party_member:' || $1::bigint::text,$2))`, project, player); err != nil {
		return err
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM project_players WHERE project_id=$1 AND id=$2 AND deleted_at IS NULL AND disabled_at IS NULL)`, project, player).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrNotFound
	}
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM party_members WHERE project_id=$1 AND player_id=$2)`, project, player).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return ErrMembership
	}
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM matchmaking_tickets WHERE project_id=$1 AND player_id=$2 AND status='queued')`, project, player).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return ErrActive
	}
	return nil
}

// Create starts a party with the caller as its unready leader.
func (s *Store) Create(ctx context.Context, project, player int64, settings Settings) (*Party, error) {
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	if settings.Query == "" {
		settings.Query = "*"
	}
	var out *Party
	err := s.pool.Q(ctx, func(tx pgx.Tx) error {
		if err := checkPlayer(ctx, tx, project, player); err != nil {
			return err
		}
		raw, err := json.Marshal(settings)
		if err != nil {
			return err
		}
		var id int64
		err = tx.QueryRow(ctx, `INSERT INTO parties(tenant_id,project_id,leader_id,settings) VALUES(current_setting('app.tenant_id')::bigint,$1,$2,$3) RETURNING id`, project, player, raw).Scan(&id)
		if err != nil {
			return err
		}
		out, err = load(ctx, tx, project, id)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		out.Members = []Member{{PlayerID: player, JoinedAt: now, LastSeenAt: now, DisconnectDeadline: now.Add(30 * time.Second)}}
		return save(ctx, tx, out)
	})
	return out, err
}

// Get recovers a party for a member. Zero id means the caller's current party.
func (s *Store) Get(ctx context.Context, project, id, player int64) (*Party, error) {
	var out *Party
	err := s.pool.Q(ctx, func(tx pgx.Tx) error {
		if id == 0 {
			if err := tx.QueryRow(ctx, `SELECT party_id FROM party_members WHERE project_id=$1 AND player_id=$2`, project, player).Scan(&id); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return ErrNotFound
				}
				return err
			}
		}
		var err error
		out, err = load(ctx, tx, project, id)
		if err != nil {
			return err
		}
		if memberIndex(out, player) < 0 {
			return ErrNotFound
		}
		return nil
	})
	return out, err
}

func (s *Store) mutate(ctx context.Context, project, id, player, version int64, leader bool, fn func(pgx.Tx, *Party) error) (*Party, error) {
	var out *Party
	err := s.pool.Q(ctx, func(tx pgx.Tx) error {
		var err error
		out, err = load(ctx, tx, project, id)
		if err != nil {
			return err
		}
		if memberIndex(out, player) < 0 {
			return ErrNotFound
		}
		if leader && out.LeaderID != player {
			return ErrNotLeader
		}
		if out.Version != version {
			return ErrStale
		}
		if err = fn(tx, out); err != nil {
			return err
		}
		return save(ctx, tx, out)
	})
	if webutil.IsUniqueViolation(err) {
		err = ErrMembership
	}
	return out, err
}

// Update changes idle queue settings and resets all readiness.
func (s *Store) Update(ctx context.Context, project, id, player, version int64, settings Settings) (*Party, error) {
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	if settings.Query == "" {
		settings.Query = "*"
	}
	return s.mutate(ctx, project, id, player, version, true, func(_ pgx.Tx, p *Party) error {
		if p.State != "idle" {
			return ErrBusy
		}
		p.Settings = settings
		p.rosterChanged()
		return nil
	})
}

// Ready changes only the caller's readiness and player properties.
func (s *Store) Ready(ctx context.Context, project, id, player, version int64, ready bool, properties Member) (*Party, error) {
	return s.mutate(ctx, project, id, player, version, false, func(_ pgx.Tx, p *Party) error {
		if p.State != "idle" && p.State != "matched" {
			return ErrBusy
		}
		m := &p.Members[memberIndex(p, player)]
		m.ReadyVersion = 0
		if ready {
			m.ReadyVersion = p.RosterVersion
		}
		m.StringProperties = properties.StringProperties
		m.NumericProperties = properties.NumericProperties
		m.Attributes = properties.Attributes
		p.Version++
		return nil
	})
}

// Heartbeat extends only presence. It never resets readiness.
func (s *Store) Heartbeat(ctx context.Context, project, id, player, version int64) (*Party, error) {
	return s.mutate(ctx, project, id, player, version, false, func(_ pgx.Tx, p *Party) error {
		m := &p.Members[memberIndex(p, player)]
		now := time.Now().UTC()
		if !m.DisconnectDeadline.After(now) {
			return ErrNotFound
		}
		m.LastSeenAt = now
		m.DisconnectDeadline = now.Add(30 * time.Second)
		return nil
	})
}

func cancel(ctx context.Context, tx pgx.Tx, p *Party) error {
	if p.CurrentQueueEntryID == nil {
		if p.State == "matched" {
			p.State = "idle"
		}
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE matchmaking_entries SET status='cancelled' WHERE id=$1 AND status='queued'`, *p.CurrentQueueEntryID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE matchmaking_tickets SET status='cancelled',claim_id=NULL,claimed_at=NULL,claim_expires_at=NULL WHERE entry_id=$1 AND status='queued'`, *p.CurrentQueueEntryID)
	p.CurrentQueueEntryID = nil
	p.State = "idle"
	return err
}

// Remove leaves, kicks, or disbands without altering the active game session.
func (s *Store) Remove(ctx context.Context, project, id, player, version, target int64, disband bool) (*Party, error) {
	return s.mutate(ctx, project, id, player, version, disband || target != player, func(tx pgx.Tx, p *Party) error {
		if err := cancel(ctx, tx, p); err != nil {
			return err
		}
		if disband {
			p.Members = nil
		} else {
			index := memberIndex(p, target)
			if index < 0 {
				return ErrNotFound
			}
			p.Members = slices.Delete(p.Members, index, index+1)
		}
		if len(p.Members) == 0 {
			p.State = "closed"
		} else if memberIndex(p, p.LeaderID) < 0 {
			p.promote(time.Now())
		}
		if p.State == "closed" {
			p.Members = nil
		}
		p.rosterChanged()
		return nil
	})
}

// Cancel stops queued tickets or returns a matched party to idle.
func (s *Store) Cancel(ctx context.Context, project, id, player, version int64) (*Party, error) {
	return s.mutate(ctx, project, id, player, version, true, func(tx pgx.Tx, p *Party) error {
		if p.State != "queued" && p.State != "matched" {
			return ErrBusy
		}
		if err := cancel(ctx, tx, p); err != nil {
			return err
		}
		p.rosterChanged()
		return nil
	})
}
