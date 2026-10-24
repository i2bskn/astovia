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
-- name: CreateAPIToken :exec
INSERT INTO api_tokens (id,user_id,name,token_hash,created_at,last_used_at)
VALUES (sqlc.arg(id),sqlc.arg(user_id),sqlc.arg(name),sqlc.arg(token_hash),sqlc.arg(created_at),NULL);
-- name: CountAPITokens :one
SELECT COUNT(*) FROM api_tokens WHERE user_id = sqlc.arg(user_id);
-- name: ListAPITokens :many
SELECT id, user_id, name, created_at, last_used_at FROM api_tokens
WHERE user_id = sqlc.arg(user_id) ORDER BY created_at DESC, id DESC;
-- name: GetAPITokenByHash :one
SELECT id, user_id, last_used_at FROM api_tokens WHERE token_hash = sqlc.arg(token_hash);
-- name: DeleteAPIToken :execrows
DELETE FROM api_tokens WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id);
-- name: TouchAPIToken :exec
UPDATE api_tokens SET last_used_at = sqlc.arg(last_used_at)
WHERE id = sqlc.arg(id) AND (last_used_at IS NULL OR last_used_at < sqlc.arg(touched_before));
-- name: DeleteUserAPITokens :exec
DELETE FROM api_tokens WHERE user_id = sqlc.arg(user_id);
-- name: GetOAuthClient :one
SELECT client_id, kind, name, redirect_uris, auth_method, secret_hash, metadata_expires_at, created_at, updated_at FROM oauth_clients
WHERE client_id = sqlc.arg(client_id);
-- name: CreateOAuthClient :exec
INSERT INTO oauth_clients (client_id,kind,name,redirect_uris,auth_method,secret_hash,metadata_expires_at,created_at,updated_at)
VALUES (sqlc.arg(client_id),sqlc.arg(kind),sqlc.arg(name),sqlc.arg(redirect_uris),sqlc.arg(auth_method),sqlc.arg(secret_hash),sqlc.arg(metadata_expires_at),sqlc.arg(created_at),sqlc.arg(updated_at));
-- name: UpsertCIMDClient :exec
INSERT INTO oauth_clients (client_id,kind,name,redirect_uris,auth_method,secret_hash,metadata_expires_at,created_at,updated_at)
VALUES (sqlc.arg(client_id),'cimd',sqlc.arg(name),sqlc.arg(redirect_uris),'none',NULL,sqlc.arg(metadata_expires_at),sqlc.arg(created_at),sqlc.arg(updated_at))
ON CONFLICT (client_id) DO UPDATE SET name = excluded.name, redirect_uris = excluded.redirect_uris, metadata_expires_at = excluded.metadata_expires_at, updated_at = excluded.updated_at;
-- name: CreateOAuthGrant :exec
INSERT INTO oauth_grants (id,user_id,client_id,client_name,scope,resource,redirect_uri,code_hash,code_challenge,code_expires_at,code_used_at,created_at,expires_at,last_used_at)
VALUES (sqlc.arg(id),sqlc.arg(user_id),sqlc.arg(client_id),sqlc.arg(client_name),sqlc.arg(scope),sqlc.arg(resource),sqlc.arg(redirect_uri),sqlc.arg(code_hash),sqlc.arg(code_challenge),sqlc.arg(code_expires_at),NULL,sqlc.arg(created_at),sqlc.arg(expires_at),NULL);
-- name: GetOAuthGrantByCode :one
SELECT id, user_id, client_id, client_name, scope, resource, redirect_uri, code_hash, code_challenge, code_expires_at, code_used_at, created_at, expires_at, last_used_at FROM oauth_grants
WHERE code_hash = sqlc.arg(code_hash);
-- name: LockOAuthGrantByCode :one
SELECT id, user_id, client_id, client_name, scope, resource, redirect_uri, code_hash, code_challenge, code_expires_at, code_used_at, created_at, expires_at, last_used_at FROM oauth_grants
WHERE code_hash = sqlc.arg(code_hash) AND user_id = sqlc.arg(user_id) FOR UPDATE;
-- name: GetOAuthGrant :one
SELECT id, user_id, client_id, client_name, scope, resource, redirect_uri, code_hash, code_challenge, code_expires_at, code_used_at, created_at, expires_at, last_used_at FROM oauth_grants
WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id);
-- name: MarkOAuthCodeUsed :execrows
UPDATE oauth_grants SET code_used_at = sqlc.arg(code_used_at)
WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id) AND code_used_at IS NULL;
-- name: ExtendOAuthGrant :exec
UPDATE oauth_grants SET expires_at = sqlc.arg(expires_at) WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id);
-- name: ListOAuthGrants :many
SELECT id, user_id, client_id, client_name, scope, resource, redirect_uri, code_hash, code_challenge, code_expires_at, code_used_at, created_at, expires_at, last_used_at FROM oauth_grants
WHERE user_id = sqlc.arg(user_id) AND code_used_at IS NOT NULL AND expires_at > sqlc.arg(now)
ORDER BY created_at DESC, id DESC;
-- name: DeleteOAuthGrant :execrows
DELETE FROM oauth_grants WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id);
-- name: TouchOAuthGrant :exec
UPDATE oauth_grants SET last_used_at = sqlc.arg(last_used_at)
WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id) AND (last_used_at IS NULL OR last_used_at < sqlc.arg(touched_before));
-- name: DeleteExpiredOAuthGrants :exec
DELETE FROM oauth_grants WHERE user_id = sqlc.arg(user_id) AND expires_at <= sqlc.arg(now);
-- name: DeleteUserOAuthGrants :exec
DELETE FROM oauth_grants WHERE user_id = sqlc.arg(user_id);
-- name: CreateOAuthToken :exec
INSERT INTO oauth_tokens (token_hash,kind,grant_id,user_id,created_at,expires_at)
VALUES (sqlc.arg(token_hash),sqlc.arg(kind),sqlc.arg(grant_id),sqlc.arg(user_id),sqlc.arg(created_at),sqlc.arg(expires_at));
-- name: GetOAuthTokenByHash :one
SELECT token_hash, kind, grant_id, user_id, created_at, expires_at FROM oauth_tokens WHERE token_hash = sqlc.arg(token_hash);
-- name: LockOAuthTokenByHash :one
SELECT token_hash, kind, grant_id, user_id, created_at, expires_at FROM oauth_tokens
WHERE token_hash = sqlc.arg(token_hash) AND user_id = sqlc.arg(user_id) FOR UPDATE;
-- name: DeleteOAuthToken :execrows
DELETE FROM oauth_tokens WHERE token_hash = sqlc.arg(token_hash) AND user_id = sqlc.arg(user_id);
-- name: DeleteGrantOAuthTokens :exec
DELETE FROM oauth_tokens WHERE grant_id = sqlc.arg(grant_id) AND user_id = sqlc.arg(user_id);
-- name: DeleteExpiredOAuthTokens :exec
DELETE FROM oauth_tokens WHERE user_id = sqlc.arg(user_id) AND expires_at <= sqlc.arg(now);
-- name: DeleteUserOAuthTokens :exec
DELETE FROM oauth_tokens WHERE user_id = sqlc.arg(user_id);
