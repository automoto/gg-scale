package rbac

import (
	"context"
	"fmt"
	"strings"

	"github.com/casbin/casbin/v3/model"
	"github.com/casbin/casbin/v3/persist"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ggscale/ggscale/internal/db"
)

type adapter struct {
	pool *db.Pool
}

func newAdapter(pool *db.Pool) *adapter {
	return &adapter{pool: pool}
}

func (a *adapter) LoadPolicy(m model.Model) error {
	return a.pool.BootstrapQ(context.Background(), func(tx pgx.Tx) error {
		rows, err := tx.Query(context.Background(), `
SELECT ptype, v0, v1, v2, v3, v4, v5
FROM casbin_rule
ORDER BY id`)
		if err != nil {
			return fmt.Errorf("rbac: load policy query: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var (
				ptype string
				v     [6]pgtype.Text
			)
			if err := rows.Scan(&ptype, &v[0], &v[1], &v[2], &v[3], &v[4], &v[5]); err != nil {
				return fmt.Errorf("rbac: load policy scan: %w", err)
			}
			rule := []string{ptype}
			for _, value := range v {
				if !value.Valid {
					break
				}
				rule = append(rule, value.String)
			}
			if err := persist.LoadPolicyArray(rule, m); err != nil {
				return fmt.Errorf("rbac: load policy line %v: %w", rule, err)
			}
		}
		return rows.Err()
	})
}

func (a *adapter) SavePolicy(m model.Model) error {
	return a.pool.BootstrapQ(context.Background(), func(tx pgx.Tx) error {
		if _, err := tx.Exec(context.Background(), `DELETE FROM casbin_rule`); err != nil {
			return fmt.Errorf("rbac: clear policy: %w", err)
		}
		for ptype, ast := range m["p"] {
			for _, rule := range ast.Policy {
				if err := insertRule(context.Background(), tx, ptype, rule); err != nil {
					return err
				}
			}
		}
		for ptype, ast := range m["g"] {
			for _, rule := range ast.Policy {
				if err := insertRule(context.Background(), tx, ptype, rule); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func (a *adapter) AddPolicy(_ string, ptype string, rule []string) error {
	return a.pool.BootstrapQ(context.Background(), func(tx pgx.Tx) error {
		return insertRule(context.Background(), tx, ptype, rule)
	})
}

func (a *adapter) RemovePolicy(_ string, ptype string, rule []string) error {
	values := ruleValues(rule)
	return a.pool.BootstrapQ(context.Background(), func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `
DELETE FROM casbin_rule
WHERE ptype = $1
  AND v0 IS NOT DISTINCT FROM $2
  AND v1 IS NOT DISTINCT FROM $3
  AND v2 IS NOT DISTINCT FROM $4
  AND v3 IS NOT DISTINCT FROM $5
  AND v4 IS NOT DISTINCT FROM $6
  AND v5 IS NOT DISTINCT FROM $7`,
			ptype, values[0], values[1], values[2], values[3], values[4], values[5])
		if err != nil {
			return fmt.Errorf("rbac: remove policy: %w", err)
		}
		return nil
	})
}

func (a *adapter) RemoveFilteredPolicy(_ string, ptype string, fieldIndex int, fieldValues ...string) error {
	if fieldIndex < 0 || fieldIndex+len(fieldValues) > 6 {
		return fmt.Errorf("rbac: invalid filtered policy index %d with %d values", fieldIndex, len(fieldValues))
	}
	var b strings.Builder
	b.WriteString("DELETE FROM casbin_rule WHERE ptype = $1")
	args := []any{ptype}
	for i, value := range fieldValues {
		if value == "" {
			continue
		}
		args = append(args, value)
		fmt.Fprintf(&b, " AND v%d IS NOT DISTINCT FROM $%d", fieldIndex+i, len(args))
	}
	return a.pool.BootstrapQ(context.Background(), func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), b.String(), args...)
		if err != nil {
			return fmt.Errorf("rbac: remove filtered policy: %w", err)
		}
		return nil
	})
}

func insertRule(ctx context.Context, tx pgx.Tx, ptype string, rule []string) error {
	values := ruleValues(rule)
	_, err := tx.Exec(ctx, `
INSERT INTO casbin_rule (ptype, v0, v1, v2, v3, v4, v5)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT DO NOTHING`,
		ptype, values[0], values[1], values[2], values[3], values[4], values[5])
	if err != nil {
		return fmt.Errorf("rbac: insert policy %s %v: %w", ptype, rule, err)
	}
	return nil
}

func ruleValues(rule []string) [6]any {
	var out [6]any
	for i := 0; i < len(rule) && i < len(out); i++ {
		out[i] = rule[i]
	}
	return out
}
