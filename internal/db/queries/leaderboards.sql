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
    tenant_id, leaderboard_id, player_id, score, recorded_at
)
VALUES (
    current_setting('app.tenant_id', true)::bigint,
    $1, $2, $3, now()
)
RETURNING id, recorded_at;

-- name: TopN :many
SELECT le.player_id,
       CASE WHEN max(l.sort_order) = 'asc' THEN MIN(le.score) ELSE MAX(le.score) END::bigint AS best_score,
       MIN(le.recorded_at)::timestamptz AS first_seen
FROM leaderboard_entries le
JOIN leaderboards l ON l.id = le.leaderboard_id
WHERE le.tenant_id = current_setting('app.tenant_id', true)::bigint
  AND le.leaderboard_id = $1
  AND l.tenant_id = le.tenant_id
  AND l.deleted_at IS NULL
GROUP BY le.player_id
ORDER BY
  CASE WHEN max(l.sort_order) = 'asc' THEN MIN(le.score) END ASC,
  CASE WHEN max(l.sort_order) <> 'asc' THEN MAX(le.score) END DESC,
  le.player_id ASC
LIMIT $2;

-- name: CountEntries :one
SELECT COUNT(*)::bigint
FROM leaderboard_entries
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND leaderboard_id = $1;

-- name: LeaderboardUserRank :one
WITH ranked AS (
    SELECT player_id,
           RANK() OVER (
             ORDER BY
               CASE WHEN max(l.sort_order) = 'asc' THEN MIN(le.score) END ASC,
               CASE WHEN max(l.sort_order) <> 'asc' THEN MAX(le.score) END DESC,
               player_id ASC
           ) AS r
    FROM leaderboard_entries le
    JOIN leaderboards l ON l.id = le.leaderboard_id
    WHERE le.tenant_id = current_setting('app.tenant_id', true)::bigint
      AND le.leaderboard_id = $1
      AND l.tenant_id = le.tenant_id
      AND l.deleted_at IS NULL
    GROUP BY player_id
)
SELECT r::bigint AS rank
FROM ranked
WHERE player_id = $2;

-- name: LeaderboardRangeByRank :many
WITH ranked AS (
    SELECT player_id,
           CASE WHEN max(l.sort_order) = 'asc' THEN MIN(le.score) ELSE MAX(le.score) END::bigint AS best_score,
           RANK() OVER (
             ORDER BY
               CASE WHEN max(l.sort_order) = 'asc' THEN MIN(le.score) END ASC,
               CASE WHEN max(l.sort_order) <> 'asc' THEN MAX(le.score) END DESC,
               player_id ASC
           ) AS r
    FROM leaderboard_entries le
    JOIN leaderboards l ON l.id = le.leaderboard_id
    WHERE le.tenant_id = current_setting('app.tenant_id', true)::bigint
      AND le.leaderboard_id = $1
      AND l.tenant_id = le.tenant_id
      AND l.deleted_at IS NULL
    GROUP BY player_id
)
SELECT player_id, best_score, r::bigint AS rank
FROM ranked
WHERE r BETWEEN sqlc.arg(rank_low)::bigint AND sqlc.arg(rank_high)::bigint
ORDER BY r;

-- name: ListLeaderboardsForProject :many
SELECT id, name, sort_order, created_at
FROM leaderboards
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND project_id = sqlc.arg(project_id)
  AND deleted_at IS NULL
ORDER BY name;

-- name: GetLeaderboardForControlPanel :one
SELECT id, project_id, name, sort_order, created_at
FROM leaderboards
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND project_id = sqlc.arg(project_id)
  AND id = sqlc.arg(id)
  AND deleted_at IS NULL;

-- name: UpdateLeaderboard :execrows
UPDATE leaderboards
SET name = sqlc.arg(name),
    sort_order = sqlc.arg(sort_order)
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND project_id = sqlc.arg(project_id)
  AND id = sqlc.arg(id)
  AND deleted_at IS NULL;

-- name: SoftDeleteLeaderboard :execrows
UPDATE leaderboards
SET deleted_at = now()
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND project_id = sqlc.arg(project_id)
  AND id = sqlc.arg(id)
  AND deleted_at IS NULL;
