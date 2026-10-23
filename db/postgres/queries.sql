-- name: GetUserByEmail :one
SELECT id, email, password_hash, disabled, created_at FROM users WHERE email = sqlc.arg(email);
-- name: GetUserByID :one
SELECT id, email, password_hash, disabled, created_at FROM users WHERE id = sqlc.arg(id);
-- name: LockUserByID :one
SELECT id, email, password_hash, disabled, created_at FROM users WHERE id = sqlc.arg(id) FOR UPDATE;
-- name: CreateUser :exec
INSERT INTO users (id,email,password_hash,disabled,created_at) VALUES ($1,$2,$3,0,$4);
-- name: ResetPassword :exec
UPDATE users SET password_hash = $1 WHERE id = $2;
-- name: DisableUser :exec
UPDATE users SET disabled = 1 WHERE id = $1;
-- name: CreateSession :exec
INSERT INTO sessions (id,user_id,expires_at,created_at) VALUES ($1,$2,$3,$4);
-- name: GetSession :one
SELECT id, user_id, expires_at, revoked_at, created_at FROM sessions WHERE id = sqlc.arg(id);
-- name: GetActiveSession :one
SELECT id, user_id, expires_at, revoked_at, created_at FROM sessions
WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id) AND revoked_at IS NULL AND expires_at > sqlc.arg(now);
-- name: RevokeSession :exec
UPDATE sessions SET revoked_at = $1 WHERE id = $2 AND revoked_at IS NULL;
-- name: RevokeUserSessions :exec
UPDATE sessions SET revoked_at = $1 WHERE user_id = $2 AND revoked_at IS NULL;
-- name: CreateRefresh :exec
INSERT INTO refresh_tokens (hash,session_id,created_at) VALUES ($1,$2,$3);
-- name: GetRefresh :one
SELECT hash, session_id, consumed_at, created_at FROM refresh_tokens WHERE hash = sqlc.arg(hash);
-- name: ConsumeRefresh :execrows
UPDATE refresh_tokens SET consumed_at = $1 WHERE hash = $2 AND consumed_at IS NULL;
-- name: CreateActionItem :exec
INSERT INTO action_items (id,user_id,content,status,note,due_at,refs,version,created_at,updated_at)
VALUES (sqlc.arg(id),sqlc.arg(user_id),sqlc.arg(content),sqlc.arg(status),sqlc.arg(note),sqlc.arg(due_at),sqlc.arg(refs),sqlc.arg(version),sqlc.arg(created_at),sqlc.arg(updated_at));
-- name: GetActionItem :one
SELECT id, user_id, content, status, note, due_at, refs, version, created_at, updated_at FROM action_items
WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id);
-- name: LockActionItem :one
SELECT id, user_id, content, status, note, due_at, refs, version, created_at, updated_at FROM action_items
WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id) FOR UPDATE;
-- name: ListActionItems :many
SELECT id, user_id, content, status, note, due_at, refs, version, created_at, updated_at FROM action_items
WHERE user_id = sqlc.arg(user_id) AND (created_at < sqlc.arg(before_created_at) OR (created_at = sqlc.arg(tie_created_at) AND id < sqlc.arg(before_id)))
ORDER BY created_at DESC, id DESC LIMIT sqlc.arg(page_size);
-- name: ListActionItemsByStatus :many
SELECT id, user_id, content, status, note, due_at, refs, version, created_at, updated_at FROM action_items
WHERE user_id = sqlc.arg(user_id) AND status = sqlc.arg(status) AND (created_at < sqlc.arg(before_created_at) OR (created_at = sqlc.arg(tie_created_at) AND id < sqlc.arg(before_id)))
ORDER BY created_at DESC, id DESC LIMIT sqlc.arg(page_size);
-- name: UpdateActionItem :execrows
UPDATE action_items SET content = sqlc.arg(content), status = sqlc.arg(status), note = sqlc.arg(note), due_at = sqlc.arg(due_at), refs = sqlc.arg(refs), version = sqlc.arg(new_version), updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id) AND version = sqlc.arg(current_version);
