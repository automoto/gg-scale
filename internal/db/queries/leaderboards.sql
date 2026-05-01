-- name: GetLeaderboard :one
SELECT id, sort_order
FROM leaderboards
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND id = $1
  AND deleted_at IS NULL;

-- name: CreateLeaderboard :one
INSERT INTO leaderboards (tenant_id, project_id, name, sort_order)
VALUES (
    current_setting('app.tenant_id', true)::bigint,
    $1, $2, $3
)
RETURNING id;

-- name: SubmitScore :one
INSERT INTO leaderboard_entries (
    tenant_id, leaderboard_id, end_user_id, score, recorded_at
)
VALUES (
    current_setting('app.tenant_id', true)::bigint,
    $1, $2, $3, now()
)
RETURNING id, recorded_at;

-- name: TopN :many
SELECT le.end_user_id,
       MAX(le.score)::bigint AS best_score,
       MIN(le.recorded_at)::timestamptz AS first_seen
FROM leaderboard_entries le
WHERE le.tenant_id = current_setting('app.tenant_id', true)::bigint
  AND le.leaderboard_id = $1
GROUP BY le.end_user_id
ORDER BY best_score DESC
LIMIT $2;

-- name: CountEntries :one
SELECT COUNT(*)::bigint
FROM leaderboard_entries
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND leaderboard_id = $1;

-- name: LeaderboardUserRank :one
WITH ranked AS (
    SELECT end_user_id,
           RANK() OVER (ORDER BY MAX(score) DESC, end_user_id ASC) AS r
    FROM leaderboard_entries
    WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
      AND leaderboard_id = $1
    GROUP BY end_user_id
)
SELECT r::bigint AS rank
FROM ranked
WHERE end_user_id = $2;

-- name: LeaderboardRangeByRank :many
WITH ranked AS (
    SELECT end_user_id,
           MAX(score)::bigint AS best_score,
           RANK() OVER (ORDER BY MAX(score) DESC, end_user_id ASC) AS r
    FROM leaderboard_entries
    WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
      AND leaderboard_id = $1
    GROUP BY end_user_id
)
SELECT end_user_id, best_score, r::bigint AS rank
FROM ranked
WHERE r BETWEEN sqlc.arg(rank_low)::bigint AND sqlc.arg(rank_high)::bigint
ORDER BY r;
