-- Leaderboard entries hold ONE row per (leaderboard, player, period). The
-- board's score operator is applied at write time by the SubmitScore upsert,
-- so every read is a plain ORDER BY score. Scheduled boards bump
-- leaderboards.current_period on reset; old entries stay in place under
-- their period number.

-- name: GetLeaderboard :one
SELECT id, sort_order, score_operator, client_submissions, score_min,
       score_max, attempt_cap, reset_schedule, current_period,
       period_started_at, next_reset_at
FROM leaderboards
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND id = sqlc.arg(id)
  AND project_id = sqlc.arg(project_id)
  AND deleted_at IS NULL;

-- name: GetLeaderboardForSubmit :one
-- The submit-path twin of GetLeaderboard. FOR KEY SHARE conflicts with the
-- reset job's FOR UPDATE (ListDueLeaderboardResets), so a submission blocks
-- on an in-flight reset and re-reads the advanced period instead of writing
-- into the just-archived one. Concurrent submissions do not block each other.
SELECT id, sort_order, score_operator, client_submissions, score_min,
       score_max, attempt_cap, reset_schedule, current_period,
       period_started_at, next_reset_at
FROM leaderboards
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND id = sqlc.arg(id)
  AND project_id = sqlc.arg(project_id)
  AND deleted_at IS NULL
FOR KEY SHARE;

-- name: CreateLeaderboard :one
INSERT INTO leaderboards (
    tenant_id, project_id, name, sort_order, score_operator, metadata,
    client_submissions, score_min, score_max, reset_schedule, attempt_cap,
    period_started_at, next_reset_at
)
VALUES (
    current_setting('app.tenant_id', true)::bigint,
    sqlc.arg(project_id), sqlc.arg(name), sqlc.arg(sort_order),
    sqlc.arg(score_operator), sqlc.narg(metadata),
    sqlc.arg(client_submissions), sqlc.narg(score_min), sqlc.narg(score_max),
    sqlc.arg(reset_schedule), sqlc.narg(attempt_cap),
    sqlc.narg(period_started_at), sqlc.narg(next_reset_at)
)
RETURNING id;

-- name: SubmitScore :execrows
-- Operator-aware upsert. 'best' keeps the better score by sort order, 'set'
-- replaces, 'incr' adds. clamp_min/clamp_max bound the ACCUMULATED 'incr'
-- total (the request-level bounds check only sees the delta, so without this
-- a client could stack in-bounds deltas past score_max); callers pass NULL to
-- leave the total unclamped. Metadata follows the standing score on 'best'
-- (a worse run must not overwrite the record run's metadata) and the latest
-- submission otherwise. Zero rows means the attempt cap blocked the write —
-- the arbiter matched but the DO UPDATE WHERE refused it.
INSERT INTO leaderboard_entries (
    tenant_id, leaderboard_id, player_id, period, score, attempts, metadata,
    recorded_at, updated_at
)
VALUES (
    current_setting('app.tenant_id', true)::bigint,
    sqlc.arg(leaderboard_id), sqlc.arg(player_id), sqlc.arg(period),
    sqlc.arg(score), 1, sqlc.narg(metadata), now(), now()
)
ON CONFLICT (leaderboard_id, player_id, period) DO UPDATE SET
    score = CASE
        WHEN sqlc.arg(score_operator)::text = 'set' THEN EXCLUDED.score
        -- LEAST/GREATEST ignore NULL args, so an absent bound simply drops
        -- out; wrapping COALESCE around either bound would instead feed the
        -- accumulated score back in and cancel the other bound.
        WHEN sqlc.arg(score_operator)::text = 'incr' THEN GREATEST(
            sqlc.narg(clamp_min)::bigint,
            LEAST(
                sqlc.narg(clamp_max)::bigint,
                leaderboard_entries.score + EXCLUDED.score
            )
        )
        WHEN sqlc.arg(sort_order)::text = 'asc' THEN LEAST(leaderboard_entries.score, EXCLUDED.score)
        ELSE GREATEST(leaderboard_entries.score, EXCLUDED.score)
    END,
    metadata = CASE
        WHEN sqlc.arg(score_operator)::text <> 'best' THEN EXCLUDED.metadata
        WHEN sqlc.arg(sort_order)::text = 'asc' AND EXCLUDED.score < leaderboard_entries.score THEN EXCLUDED.metadata
        WHEN sqlc.arg(sort_order)::text <> 'asc' AND EXCLUDED.score > leaderboard_entries.score THEN EXCLUDED.metadata
        ELSE leaderboard_entries.metadata
    END,
    attempts = leaderboard_entries.attempts + 1,
    updated_at = now()
WHERE sqlc.narg(attempt_cap)::integer IS NULL
   OR leaderboard_entries.attempts < sqlc.narg(attempt_cap)::integer;

-- name: TopN :many
-- display_name rides along from the entry's linked global account (NULL for
-- anonymous players); joining pp/a cannot fan out because pp.id is unique.
SELECT le.player_id, le.score, le.metadata, a.display_name
FROM leaderboard_entries le
JOIN leaderboards l ON l.id = le.leaderboard_id AND l.deleted_at IS NULL
LEFT JOIN project_players pp ON pp.id = le.player_id AND pp.deleted_at IS NULL
LEFT JOIN player_accounts a ON a.id = pp.player_account_id
WHERE le.tenant_id = current_setting('app.tenant_id', true)::bigint
  AND le.leaderboard_id = sqlc.arg(leaderboard_id)
  AND le.period = sqlc.arg(period)
  AND l.tenant_id = le.tenant_id
  AND l.project_id = sqlc.arg(project_id)
ORDER BY
  CASE WHEN l.sort_order = 'asc' THEN le.score END ASC,
  CASE WHEN l.sort_order <> 'asc' THEN le.score END DESC,
  le.player_id ASC
LIMIT sqlc.arg(row_limit);

-- name: LeaderboardUserRank :one
WITH ranked AS (
    SELECT le.player_id,
           RANK() OVER (
             ORDER BY
               CASE WHEN l.sort_order = 'asc' THEN le.score END ASC,
               CASE WHEN l.sort_order <> 'asc' THEN le.score END DESC,
               le.player_id ASC
           ) AS r
    FROM leaderboard_entries le
    JOIN leaderboards l ON l.id = le.leaderboard_id AND l.deleted_at IS NULL
    WHERE le.tenant_id = current_setting('app.tenant_id', true)::bigint
      AND le.leaderboard_id = sqlc.arg(leaderboard_id)
      AND le.period = sqlc.arg(period)
      AND l.tenant_id = le.tenant_id
      AND l.project_id = sqlc.arg(project_id)
)
SELECT r::bigint AS rank
FROM ranked
WHERE player_id = sqlc.arg(player_id);

-- name: LeaderboardRangeByRank :many
WITH ranked AS (
    SELECT le.player_id, le.score, le.metadata, a.display_name,
           RANK() OVER (
             ORDER BY
               CASE WHEN l.sort_order = 'asc' THEN le.score END ASC,
               CASE WHEN l.sort_order <> 'asc' THEN le.score END DESC,
               le.player_id ASC
           ) AS r
    FROM leaderboard_entries le
    JOIN leaderboards l ON l.id = le.leaderboard_id AND l.deleted_at IS NULL
    LEFT JOIN project_players pp ON pp.id = le.player_id AND pp.deleted_at IS NULL
    LEFT JOIN player_accounts a ON a.id = pp.player_account_id
    WHERE le.tenant_id = current_setting('app.tenant_id', true)::bigint
      AND le.leaderboard_id = sqlc.arg(leaderboard_id)
      AND le.period = sqlc.arg(period)
      AND l.tenant_id = le.tenant_id
      AND l.project_id = sqlc.arg(project_id)
)
SELECT player_id, score, metadata, display_name, r::bigint AS rank
FROM ranked
WHERE r BETWEEN sqlc.arg(rank_low)::bigint AND sqlc.arg(rank_high)::bigint
ORDER BY r;

-- name: LeaderboardEntriesForPlayers :many
-- Friends view: current-period entries for an explicit player set, in rank
-- order. The caller re-ranks 0-based within the returned set.
SELECT le.player_id, le.score, le.metadata, a.display_name
FROM leaderboard_entries le
JOIN leaderboards l ON l.id = le.leaderboard_id AND l.deleted_at IS NULL
LEFT JOIN project_players pp ON pp.id = le.player_id AND pp.deleted_at IS NULL
LEFT JOIN player_accounts a ON a.id = pp.player_account_id
WHERE le.tenant_id = current_setting('app.tenant_id', true)::bigint
  AND le.leaderboard_id = sqlc.arg(leaderboard_id)
  AND le.period = sqlc.arg(period)
  AND l.tenant_id = le.tenant_id
  AND l.project_id = sqlc.arg(project_id)
  AND le.player_id = ANY(sqlc.arg(player_ids)::bigint[])
ORDER BY
  CASE WHEN l.sort_order = 'asc' THEN le.score END ASC,
  CASE WHEN l.sort_order <> 'asc' THEN le.score END DESC,
  le.player_id ASC
LIMIT sqlc.arg(row_limit);

-- name: ListLeaderboardsForProject :many
SELECT id, name, sort_order, score_operator, metadata, client_submissions,
       score_min, score_max, reset_schedule, attempt_cap, current_period,
       period_started_at, next_reset_at, created_at
FROM leaderboards
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND project_id = sqlc.arg(project_id)
  AND deleted_at IS NULL
ORDER BY name;

-- name: GetLeaderboardForControlPanel :one
SELECT id, project_id, name, sort_order, score_operator, metadata,
       client_submissions, score_min, score_max, reset_schedule, attempt_cap,
       current_period, period_started_at, next_reset_at, created_at
FROM leaderboards
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND project_id = sqlc.arg(project_id)
  AND id = sqlc.arg(id)
  AND deleted_at IS NULL;

-- name: UpdateLeaderboard :execrows
-- score_operator and current_period are deliberately absent: the operator is
-- fixed at creation and the period only moves via AdvanceLeaderboardPeriod.
UPDATE leaderboards
SET name = sqlc.arg(name),
    sort_order = sqlc.arg(sort_order),
    metadata = sqlc.narg(metadata),
    client_submissions = sqlc.arg(client_submissions),
    score_min = sqlc.narg(score_min),
    score_max = sqlc.narg(score_max),
    reset_schedule = sqlc.arg(reset_schedule),
    attempt_cap = sqlc.narg(attempt_cap),
    period_started_at = sqlc.narg(period_started_at),
    next_reset_at = sqlc.narg(next_reset_at)
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND project_id = sqlc.arg(project_id)
  AND id = sqlc.arg(id)
  AND deleted_at IS NULL;

-- name: LeaderboardHasEntries :one
-- Any period counts: past-period entries are ranked under the board's
-- current sort order too, so history alone locks it.
SELECT EXISTS (
    SELECT 1 FROM leaderboard_entries
    WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
      AND leaderboard_id = sqlc.arg(leaderboard_id)
)::boolean AS has_entries;

-- name: SoftDeleteLeaderboard :execrows
UPDATE leaderboards
SET deleted_at = now()
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND project_id = sqlc.arg(project_id)
  AND id = sqlc.arg(id)
  AND deleted_at IS NULL;

-- name: ListLeaderboardPeriods :many
-- Finished periods, newest first, keyset-paginated on the period number.
SELECT s.period, s.started_at, s.ended_at
FROM leaderboard_periods s
JOIN leaderboards l ON l.id = s.leaderboard_id AND l.deleted_at IS NULL
WHERE s.tenant_id = current_setting('app.tenant_id', true)::bigint
  AND s.leaderboard_id = sqlc.arg(leaderboard_id)
  AND l.tenant_id = s.tenant_id
  AND l.project_id = sqlc.arg(project_id)
  AND s.period < sqlc.arg(before_period)::integer
ORDER BY s.period DESC
LIMIT sqlc.arg(row_limit);

-- name: ListDueLeaderboardResets :many
-- Boards whose scheduled reset boundary has passed, locked for the reset
-- transaction so two job runs can never double-archive a period. as_of is the
-- job's clock — the same instant it computes the next boundary from, so the
-- due filter and the new boundary can never disagree.
SELECT id, current_period, period_started_at, next_reset_at, reset_schedule, created_at
FROM leaderboards
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND deleted_at IS NULL
  AND reset_schedule <> 'none'
  AND next_reset_at IS NOT NULL
  AND next_reset_at <= sqlc.arg(as_of)::timestamptz
FOR UPDATE;

-- name: ArchiveLeaderboardPeriod :exec
INSERT INTO leaderboard_periods (tenant_id, leaderboard_id, period, started_at, ended_at)
VALUES (
    current_setting('app.tenant_id', true)::bigint,
    sqlc.arg(leaderboard_id), sqlc.arg(period), sqlc.arg(started_at), sqlc.arg(ended_at)
)
ON CONFLICT (leaderboard_id, period) DO NOTHING;

-- name: AdvanceLeaderboardPeriod :execrows
UPDATE leaderboards
SET current_period = current_period + 1,
    period_started_at = sqlc.arg(period_started_at),
    next_reset_at = sqlc.arg(next_reset_at)
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND id = sqlc.arg(id)
  AND deleted_at IS NULL;
