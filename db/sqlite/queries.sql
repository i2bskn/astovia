-- name: GetUserByName :one
SELECT id, username, password_hash, disabled, created_at FROM users WHERE username = sqlc.arg(username);
-- name: GetUserByID :one
SELECT id, username, password_hash, disabled, created_at FROM users WHERE id = sqlc.arg(id);
-- name: LockUserByID :one
SELECT id, username, password_hash, disabled, created_at FROM users WHERE id = sqlc.arg(id);
-- name: CreateUser :exec
INSERT INTO users (id,username,password_hash,disabled,created_at) VALUES (?,?,?,0,?);
-- name: ResetPassword :exec
UPDATE users SET password_hash = ? WHERE id = ?;
-- name: DisableUser :exec
UPDATE users SET disabled = 1 WHERE id = ?;
-- name: CreateSession :exec
INSERT INTO sessions (id,user_id,expires_at,created_at) VALUES (?,?,?,?);
-- name: GetSession :one
SELECT id, user_id, expires_at, revoked_at, created_at FROM sessions WHERE id = sqlc.arg(id);
-- name: GetActiveSession :one
SELECT id, user_id, expires_at, revoked_at, created_at FROM sessions
WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id) AND revoked_at IS NULL AND expires_at > sqlc.arg(now);
-- name: RevokeSession :exec
UPDATE sessions SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL;
-- name: RevokeUserSessions :exec
UPDATE sessions SET revoked_at = ? WHERE user_id = ? AND revoked_at IS NULL;
-- name: CreateRefresh :exec
INSERT INTO refresh_tokens (hash,session_id,created_at) VALUES (?,?,?);
-- name: GetRefresh :one
SELECT hash, session_id, consumed_at, created_at FROM refresh_tokens WHERE hash = sqlc.arg(hash);
-- name: ConsumeRefresh :execrows
UPDATE refresh_tokens SET consumed_at = ? WHERE hash = ? AND consumed_at IS NULL;
