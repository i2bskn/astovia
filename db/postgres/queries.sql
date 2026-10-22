-- name: GetUserByName :one
SELECT * FROM users WHERE username = $1;
-- name: GetUserByID :one
SELECT * FROM users WHERE id = $1;
-- name: LockUserByID :one
SELECT * FROM users WHERE id = $1 FOR UPDATE;
-- name: CreateUser :exec
INSERT INTO users (id,username,password_hash,disabled,created_at) VALUES ($1,$2,$3,0,$4);
-- name: ResetPassword :exec
UPDATE users SET password_hash = $1 WHERE id = $2;
-- name: DisableUser :exec
UPDATE users SET disabled = 1 WHERE id = $1;
-- name: CreateSession :exec
INSERT INTO sessions (id,user_id,expires_at,created_at) VALUES ($1,$2,$3,$4);
-- name: GetSession :one
SELECT * FROM sessions WHERE id = $1;
-- name: RevokeSession :exec
UPDATE sessions SET revoked_at = $1 WHERE id = $2 AND revoked_at IS NULL;
-- name: RevokeUserSessions :exec
UPDATE sessions SET revoked_at = $1 WHERE user_id = $2 AND revoked_at IS NULL;
-- name: CreateRefresh :exec
INSERT INTO refresh_tokens (hash,session_id,created_at) VALUES ($1,$2,$3);
-- name: GetRefresh :one
SELECT * FROM refresh_tokens WHERE hash = $1;
-- name: ConsumeRefresh :execrows
UPDATE refresh_tokens SET consumed_at = $1 WHERE hash = $2 AND consumed_at IS NULL;

-- name: GetAuthenticated :one
SELECT u.* FROM users u JOIN sessions s ON s.user_id=u.id
WHERE u.id=$1 AND s.id=$2 AND u.disabled=0 AND s.revoked_at IS NULL AND s.expires_at>$3;
