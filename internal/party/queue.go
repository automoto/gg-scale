package party

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Queue snapshots a ready roster. A repeated key returns the same entry only while it is queued.
// previousMatch is required for rematch and must name the party's last match.
func (s *Store) Queue(ctx context.Context, project, id, player, version int64, key, previousMatch string, ttl time.Duration) (*Party, error) {
	if key == "" || len(key) > 128 {
		return nil, ErrStale
	}
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
		if out.LeaderID != player {
			return ErrNotLeader
		}
		var entry int64
		var previous, status string
		err = tx.QueryRow(ctx, `SELECT id,previous_match_id,status::text FROM matchmaking_entries WHERE party_id=$1 AND idempotency_key=$2`, id, key).Scan(&entry, &previous, &status)
		if err == nil {
			if status != "queued" || out.State != "queued" || out.CurrentQueueEntryID == nil || *out.CurrentQueueEntryID != entry || previous != previousMatch {
				return ErrStale
			}
			out.CurrentQueueEntryID = &entry
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if out.Version != version {
			return ErrStale
		}
		if out.State == "matched" && (previousMatch == "" || previousMatch != out.LastMatchID) {
			return ErrStale
		}
		if previousMatch != "" && previousMatch != out.LastMatchID {
			return ErrStale
		}
		if err = out.canQueue(player, time.Now()); err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `INSERT INTO matchmaking_entries(tenant_id,project_id,party_id,idempotency_key,previous_match_id) VALUES(current_setting('app.tenant_id')::bigint,$1,$2,$3,$4) RETURNING id`, project, id, key, previousMatch).Scan(&entry)
		if err != nil {
			return err
		}
		var expiry *time.Time
		if ttl > 0 {
			v := time.Now().Add(ttl)
			expiry = &v
		}
		cfg := out.Settings
		for i := range out.Members {
			m := &out.Members[i]
			var active bool
			if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM matchmaking_tickets WHERE project_id=$1 AND player_id=$2 AND status='queued')`, project, m.PlayerID).Scan(&active); err != nil {
				return err
			}
			if active {
				return ErrActive
			}
			properties, err := json.Marshal(m)
			if err != nil {
				return err
			}
			err = tx.QueryRow(ctx, `INSERT INTO matchmaking_tickets(tenant_id,project_id,player_id,entry_id,party_id,fleet_id,mode,region,game_mode,min_count,max_count,count_multiple,allow_cross_region,query,string_properties,numeric_properties,attributes,expires_at)
    VALUES(current_setting('app.tenant_id')::bigint,$1,$2,$3,$4,NULLIF($5,0),$6,$7,$8,$9,$10,$11,$12,$13,COALESCE($14::jsonb->'string_properties','{}'),COALESCE($14::jsonb->'numeric_properties','{}'),COALESCE($14::jsonb->'attributes','{}'),$15) RETURNING id`, project, m.PlayerID, entry, id, cfg.FleetID, cfg.Mode, cfg.Region, cfg.GameMode, cfg.MinCount, cfg.MaxCount, cfg.CountMultiple, cfg.AllowCrossRegion, cfg.Query, properties, expiry).Scan(&m.TicketID)
			if err != nil {
				return err
			}
		}
		out.State = "queued"
		out.CurrentQueueEntryID = &entry
		out.Version++
		if err := save(ctx, tx, out); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `SELECT pg_notify('matchmaker_ticket',jsonb_build_object('tenant_id',current_setting('app.tenant_id')::bigint,'project_id',$1::bigint,'mode',$2::text,'fleet_id',NULLIF($3::bigint,0),'region',$4::text,'game_mode',$5::text)::text)`, project, cfg.Mode, cfg.FleetID, cfg.Region, cfg.GameMode)
		return err
	})
	return out, err
}
