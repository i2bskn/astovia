-- name: GetUserByName :one
SELECT * FROM users WHERE username = ?;
-- name: GetUserByID :one
SELECT * FROM users WHERE id = ?;
-- name: LockUserByID :one
SELECT * FROM users WHERE id = ?;
-- name: CreateUser :exec
INSERT INTO users (id,username,password_hash,disabled,created_at) VALUES (?,?,?,0,?);
-- name: ResetPassword :exec
UPDATE users SET password_hash = ? WHERE id = ?;
-- name: DisableUser :exec
UPDATE users SET disabled = 1 WHERE id = ?;
-- name: CreateSession :exec
INSERT INTO sessions (id,user_id,expires_at,created_at) VALUES (?,?,?,?);
-- name: GetSession :one
SELECT * FROM sessions WHERE id = ?;
-- name: RevokeSession :exec
UPDATE sessions SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL;
-- name: RevokeUserSessions :exec
UPDATE sessions SET revoked_at = ? WHERE user_id = ? AND revoked_at IS NULL;
-- name: CreateRefresh :exec
INSERT INTO refresh_tokens (hash,session_id,created_at) VALUES (?,?,?);
-- name: GetRefresh :one
SELECT * FROM refresh_tokens WHERE hash = ?;
-- name: ConsumeRefresh :execrows
UPDATE refresh_tokens SET consumed_at = ? WHERE hash = ? AND consumed_at IS NULL;

-- name: GetAuthenticated :one
SELECT u.* FROM users u JOIN sessions s ON s.user_id=u.id
WHERE u.id=? AND s.id=? AND u.disabled=0 AND s.revoked_at IS NULL AND s.expires_at>?;
