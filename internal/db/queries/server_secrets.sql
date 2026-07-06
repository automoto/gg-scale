-- name: GetServerSecret :one
SELECT value
FROM server_secrets
WHERE name = sqlc.arg(name);

-- name: InsertServerSecret :execrows
-- ON CONFLICT DO NOTHING makes first-boot generation race-safe: concurrent
-- instances all insert, one wins, and everyone reads the winner back.
INSERT INTO server_secrets (name, value)
VALUES (sqlc.arg(name), sqlc.arg(value))
ON CONFLICT (name) DO NOTHING;
