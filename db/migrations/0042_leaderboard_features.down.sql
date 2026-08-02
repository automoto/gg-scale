-- Reverses the leaderboard feature columns and the periods table. The entry
-- collapse in the up migration is one-way: superseded submission-log rows are
-- gone, so this restores the shape (columns and indexes) only.
DROP TABLE leaderboard_periods;

DROP INDEX leaderboard_entries_period_score_idx;
DROP INDEX leaderboard_entries_player_period_uniq;
CREATE INDEX leaderboard_entries_best_score_asc_idx
    ON leaderboard_entries (tenant_id, leaderboard_id, player_id, score);
CREATE INDEX leaderboard_entries_best_score_idx
    ON leaderboard_entries (tenant_id, leaderboard_id, player_id, score DESC);
CREATE INDEX leaderboard_entries_top_idx
    ON leaderboard_entries (tenant_id, leaderboard_id, score DESC, recorded_at);
CREATE INDEX leaderboard_entries_player_idx
    ON leaderboard_entries (tenant_id, leaderboard_id, player_id);

ALTER TABLE leaderboard_entries
DROP COLUMN period,
DROP COLUMN attempts,
DROP COLUMN metadata,
DROP COLUMN updated_at;

ALTER TABLE leaderboards
DROP COLUMN score_operator,
DROP COLUMN metadata,
DROP COLUMN client_submissions,
DROP COLUMN score_min,
DROP COLUMN score_max,
DROP COLUMN reset_schedule,
DROP COLUMN attempt_cap,
DROP COLUMN current_period,
DROP COLUMN period_started_at,
DROP COLUMN next_reset_at;
