package party

import (
	"context"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// Sweep drops members whose persisted grace deadline has elapsed. A bounded
// batch skips locked parties so concurrent workers can make progress.
func (s *Store) Sweep(ctx context.Context) error {
	return s.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT p.project_id,p.id FROM parties p WHERE p.state<>'closed' AND EXISTS(SELECT 1 FROM party_members m WHERE m.party_id=p.id AND m.disconnect_deadline<=now()) ORDER BY p.id LIMIT 100 FOR UPDATE SKIP LOCKED`)
		if err != nil {
			return err
		}
		type key struct{ project, id int64 }
		var keys []key
		for rows.Next() {
			var k key
			if err = rows.Scan(&k.project, &k.id); err != nil {
				rows.Close()
				return err
			}
			keys = append(keys, k)
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return err
		}
		now := time.Now().UTC()
		for _, k := range keys {
			p, err := load(ctx, tx, k.project, k.id)
			if err != nil {
				return err
			}
			if err = cancel(ctx, tx, p); err != nil {
				return err
			}
			p.Members = slices.DeleteFunc(p.Members, func(m Member) bool { return !m.DisconnectDeadline.After(now) })
			if len(p.Members) == 0 {
				p.State = "closed"
			} else if memberIndex(p, p.LeaderID) < 0 {
				p.promote(now)
			}
			p.rosterChanged()
			if err = save(ctx, tx, p); err != nil {
				return err
			}
		}
		return nil
	})
}
