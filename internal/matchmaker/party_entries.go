package matchmaker

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Lock order matches party mutations: party, entry, then tickets. A nil ID
// list is used by the cross-tenant expiry sweep.
func lockPartyEntries(ctx context.Context, tx pgx.Tx, ids []int64) error {
	for _, statement := range []string{
		`SELECT p.id FROM parties p WHERE p.id IN (SELECT party_id FROM matchmaking_tickets WHERE ($1::bigint[] IS NULL AND status='queued' AND (claim_expires_at<now() OR expires_at<=now())) OR id=ANY($1)) ORDER BY p.id FOR UPDATE`,
		`SELECT e.id FROM matchmaking_entries e WHERE e.id IN (SELECT entry_id FROM matchmaking_tickets WHERE ($1::bigint[] IS NULL AND status='queued' AND (claim_expires_at<now() OR expires_at<=now())) OR id=ANY($1)) ORDER BY e.id FOR UPDATE`,
	} {
		rows, err := tx.Query(ctx, statement, ids)
		if err != nil {
			return err
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return err
		}
	}
	return nil
}

func requireWholeEntries(ctx context.Context, tx pgx.Tx, ids []int64) error {
	var partial bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM matchmaking_tickets WHERE entry_id IN (SELECT entry_id FROM matchmaking_tickets WHERE id=ANY($1::bigint[])) AND NOT(id=ANY($1::bigint[])))`, ids).Scan(&partial)
	if err != nil {
		return err
	}
	if partial {
		return ErrShortCommit
	}
	return nil
}

// Terminal entry state and party state change in the ticket transaction.
func settleEntries(ctx context.Context, tx pgx.Tx, ids []int64, matchID string) error {
	_, err := tx.Exec(ctx, `UPDATE matchmaking_entries e SET status=t.status FROM matchmaking_tickets t WHERE t.entry_id=e.id AND e.status='queued' AND t.status<>'queued' AND ($1::bigint[] IS NULL OR t.id=ANY($1))`, ids)
	if err != nil {
		return err
	}
	// An entry's failure applies to its entire snapshot, including members with
	// different expiry times left by an interrupted or administrative update.
	_, err = tx.Exec(ctx, `UPDATE matchmaking_tickets t SET status=e.status,failure_reason=CASE WHEN e.status='failed' THEN COALESCE((SELECT failure_reason FROM matchmaking_tickets x WHERE x.entry_id=e.id AND x.failure_reason IS NOT NULL LIMIT 1),'expired') ELSE t.failure_reason END,claim_id=NULL,claimed_at=NULL,claim_expires_at=NULL FROM matchmaking_entries e WHERE t.entry_id=e.id AND t.status='queued' AND e.status IN ('failed','cancelled') AND ($1::bigint[] IS NULL OR e.id IN (SELECT entry_id FROM matchmaking_tickets WHERE id=ANY($1)))`, ids)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `WITH changed AS (
 UPDATE parties p SET state=CASE WHEN e.status='matched' THEN 'matched' ELSE 'idle' END,
 last_match_id=CASE WHEN e.status='matched' THEN $2 ELSE p.last_match_id END,
 current_queue_entry_id=NULL,version=version+1,roster_version=roster_version+1,updated_at=now()
 FROM matchmaking_entries e WHERE p.current_queue_entry_id=e.id AND e.status<>'queued'
 AND ($1::bigint[] IS NULL OR e.id IN (SELECT entry_id FROM matchmaking_tickets WHERE id=ANY($1))) RETURNING p.id)
 UPDATE party_members SET ready_version=0 WHERE party_id IN (SELECT id FROM changed)`, ids, matchID)
	return err
}
