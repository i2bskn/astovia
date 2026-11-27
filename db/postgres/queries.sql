-- name: GetUserByEmail :one
SELECT id, email, password_hash, disabled, created_at, role, oidc_issuer, oidc_subject, oidc_link_approved_until FROM users WHERE email = sqlc.arg(email);
-- name: GetUserByID :one
SELECT id, email, password_hash, disabled, created_at, role, oidc_issuer, oidc_subject, oidc_link_approved_until FROM users WHERE id = sqlc.arg(id);
-- name: LockUserByID :one
SELECT id, email, password_hash, disabled, created_at, role, oidc_issuer, oidc_subject, oidc_link_approved_until FROM users WHERE id = sqlc.arg(id) FOR UPDATE;
-- name: CreateUser :exec
INSERT INTO users (id,email,password_hash,disabled,created_at,role,oidc_issuer,oidc_subject)
VALUES (sqlc.arg(id),sqlc.arg(email),sqlc.arg(password_hash),0,sqlc.arg(created_at),sqlc.arg(role),sqlc.arg(oidc_issuer),sqlc.arg(oidc_subject));
-- name: GetUserByOIDC :one
SELECT id, email, password_hash, disabled, created_at, role, oidc_issuer, oidc_subject, oidc_link_approved_until FROM users
WHERE oidc_issuer = sqlc.arg(oidc_issuer) AND oidc_subject = sqlc.arg(oidc_subject);
-- name: SetUserOIDC :exec
UPDATE users SET oidc_issuer = sqlc.arg(oidc_issuer), oidc_subject = sqlc.arg(oidc_subject), oidc_link_approved_until = NULL WHERE id = sqlc.arg(id);
-- name: ClearUserOIDC :exec
UPDATE users SET oidc_issuer = NULL, oidc_subject = NULL WHERE id = sqlc.arg(id);
-- name: ListOIDCLinkedUserIDs :many
SELECT id FROM users WHERE oidc_issuer IS NOT NULL OR oidc_subject IS NOT NULL ORDER BY id;
-- name: ResetPassword :exec
UPDATE users SET password_hash = $1 WHERE id = $2;
-- name: DisableUser :exec
UPDATE users SET disabled = 1, oidc_link_approved_until = NULL WHERE id = $1;
-- name: EnableUser :exec
UPDATE users SET disabled = 0 WHERE id = sqlc.arg(id);
-- name: SetUserRole :exec
UPDATE users SET role = sqlc.arg(role) WHERE id = sqlc.arg(id);
-- name: ListUsers :many
SELECT id, email, password_hash, disabled, created_at, role, oidc_issuer, oidc_subject, oidc_link_approved_until FROM users ORDER BY created_at, id LIMIT sqlc.arg(page_size);
-- name: GetUsersByIDs :many
SELECT id, email, password_hash, disabled, created_at, role, oidc_issuer, oidc_subject, oidc_link_approved_until FROM users WHERE strpos(',' || sqlc.arg(ids)::text || ',', ',' || id || ',') > 0;
-- name: LockUsersForOwnerChange :many
SELECT id, email, password_hash, disabled, created_at, role, oidc_issuer, oidc_subject, oidc_link_approved_until FROM users
WHERE id = sqlc.arg(operator_id) OR id = sqlc.arg(target_id) OR (role = 'owner' AND disabled = 0)
ORDER BY id FOR UPDATE;
-- name: CreateActionItem :exec
INSERT INTO action_items (id,user_id,content,status,note,due_at,refs,version,created_at,updated_at,source,source_key,tracking,auto_reason,auto_ref,auto_at,detected_at,tracked_at,kind,event_title,event_start_at,event_end_at,parent_id,importance)
VALUES (sqlc.arg(id),sqlc.arg(user_id),sqlc.arg(content),sqlc.arg(status),sqlc.arg(note),sqlc.arg(due_at),sqlc.arg(refs),sqlc.arg(version),sqlc.arg(created_at),sqlc.arg(updated_at),sqlc.arg(source),sqlc.arg(source_key),sqlc.arg(tracking),sqlc.arg(auto_reason),sqlc.arg(auto_ref),sqlc.arg(auto_at),sqlc.arg(detected_at),sqlc.arg(tracked_at),sqlc.arg(kind),sqlc.arg(event_title),sqlc.arg(event_start_at),sqlc.arg(event_end_at),sqlc.arg(parent_id),sqlc.arg(importance));
-- name: InsertSourcedActionItem :execrows
INSERT INTO action_items (id,user_id,content,status,note,due_at,refs,version,created_at,updated_at,source,source_key,tracking,auto_reason,auto_ref,auto_at,detected_at,tracked_at,kind,event_title,event_start_at,event_end_at,parent_id,importance)
VALUES (sqlc.arg(id),sqlc.arg(user_id),sqlc.arg(content),sqlc.arg(status),sqlc.arg(note),sqlc.arg(due_at),sqlc.arg(refs),sqlc.arg(version),sqlc.arg(created_at),sqlc.arg(updated_at),sqlc.arg(source),sqlc.arg(source_key),sqlc.arg(tracking),sqlc.arg(auto_reason),sqlc.arg(auto_ref),sqlc.arg(auto_at),sqlc.arg(detected_at),sqlc.arg(tracked_at),sqlc.arg(kind),sqlc.arg(event_title),sqlc.arg(event_start_at),sqlc.arg(event_end_at),sqlc.arg(parent_id),sqlc.arg(importance))
ON CONFLICT (user_id, source_key) DO NOTHING;
-- name: GetActionItem :one
SELECT id, user_id, content, status, note, due_at, refs, version, created_at, updated_at, source, source_key, tracking, auto_reason, auto_ref, auto_at, detected_at, tracked_at, kind, event_title, event_start_at, event_end_at, parent_id, importance FROM action_items
WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id);
-- name: LockActionItem :one
SELECT id, user_id, content, status, note, due_at, refs, version, created_at, updated_at, source, source_key, tracking, auto_reason, auto_ref, auto_at, detected_at, tracked_at, kind, event_title, event_start_at, event_end_at, parent_id, importance FROM action_items
WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id) FOR UPDATE;
-- name: LockActionItemBySourceKey :one
SELECT id, user_id, content, status, note, due_at, refs, version, created_at, updated_at, source, source_key, tracking, auto_reason, auto_ref, auto_at, detected_at, tracked_at, kind, event_title, event_start_at, event_end_at, parent_id, importance FROM action_items
WHERE user_id = sqlc.arg(user_id) AND source_key = sqlc.arg(source_key) FOR UPDATE;
-- name: ListActionItems :many
SELECT id, user_id, content, status, note, due_at, refs, version, created_at, updated_at, source, source_key, tracking, auto_reason, auto_ref, auto_at, detected_at, tracked_at, kind, event_title, event_start_at, event_end_at, parent_id, importance FROM action_items
WHERE user_id = sqlc.arg(user_id) AND (created_at < sqlc.arg(before_created_at) OR (created_at = sqlc.arg(tie_created_at) AND id < sqlc.arg(before_id)))
ORDER BY created_at DESC, id DESC LIMIT sqlc.arg(page_size);
-- name: ListActionItemsByStatus :many
SELECT id, user_id, content, status, note, due_at, refs, version, created_at, updated_at, source, source_key, tracking, auto_reason, auto_ref, auto_at, detected_at, tracked_at, kind, event_title, event_start_at, event_end_at, parent_id, importance FROM action_items
WHERE user_id = sqlc.arg(user_id) AND status = sqlc.arg(status) AND (created_at < sqlc.arg(before_created_at) OR (created_at = sqlc.arg(tie_created_at) AND id < sqlc.arg(before_id)))
ORDER BY created_at DESC, id DESC LIMIT sqlc.arg(page_size);
-- name: ListTopActionItems :many
SELECT id, user_id, content, status, note, due_at, refs, version, created_at, updated_at, source, source_key, tracking, auto_reason, auto_ref, auto_at, detected_at, tracked_at, kind, event_title, event_start_at, event_end_at, parent_id, importance FROM action_items
WHERE user_id = sqlc.arg(user_id) AND parent_id IS NULL AND (created_at < sqlc.arg(before_created_at) OR (created_at = sqlc.arg(tie_created_at) AND id < sqlc.arg(before_id)))
ORDER BY created_at DESC, id DESC LIMIT sqlc.arg(page_size);
-- name: ListTopActionItemsByStatus :many
SELECT action_items.id, action_items.user_id, action_items.content, action_items.status, action_items.note, action_items.due_at, action_items.refs, action_items.version, action_items.created_at, action_items.updated_at, action_items.source, action_items.source_key, action_items.tracking, action_items.auto_reason, action_items.auto_ref, action_items.auto_at, action_items.detected_at, action_items.tracked_at, action_items.kind, action_items.event_title, action_items.event_start_at, action_items.event_end_at, action_items.parent_id, action_items.importance FROM action_items
WHERE action_items.user_id = sqlc.arg(user_id) AND action_items.status = sqlc.arg(status)
AND (action_items.parent_id IS NULL OR NOT EXISTS (SELECT 1 FROM action_items p WHERE p.user_id = action_items.user_id AND p.id = action_items.parent_id AND p.status = sqlc.arg(parent_status)))
AND (action_items.created_at < sqlc.arg(before_created_at) OR (action_items.created_at = sqlc.arg(tie_created_at) AND action_items.id < sqlc.arg(before_id)))
ORDER BY action_items.created_at DESC, action_items.id DESC LIMIT sqlc.arg(page_size);
-- name: ListAutoActionItems :many
SELECT id, user_id, content, status, note, due_at, refs, version, created_at, updated_at, source, source_key, tracking, auto_reason, auto_ref, auto_at, detected_at, tracked_at, kind, event_title, event_start_at, event_end_at, parent_id, importance FROM action_items
WHERE user_id = sqlc.arg(user_id) AND auto_reason IS NOT NULL AND (auto_at < sqlc.arg(before_auto_at) OR (auto_at = sqlc.arg(tie_auto_at) AND id < sqlc.arg(before_id)))
ORDER BY auto_at DESC, id DESC LIMIT sqlc.arg(page_size);
-- name: ListAutoActionItemsByStatus :many
SELECT id, user_id, content, status, note, due_at, refs, version, created_at, updated_at, source, source_key, tracking, auto_reason, auto_ref, auto_at, detected_at, tracked_at, kind, event_title, event_start_at, event_end_at, parent_id, importance FROM action_items
WHERE user_id = sqlc.arg(user_id) AND status = sqlc.arg(status) AND auto_reason IS NOT NULL AND (auto_at < sqlc.arg(before_auto_at) OR (auto_at = sqlc.arg(tie_auto_at) AND id < sqlc.arg(before_id)))
ORDER BY auto_at DESC, id DESC LIMIT sqlc.arg(page_size);
-- name: ListTrackedActionItems :many
SELECT id, user_id, content, status, note, due_at, refs, version, created_at, updated_at, source, source_key, tracking, auto_reason, auto_ref, auto_at, detected_at, tracked_at, kind, event_title, event_start_at, event_end_at, parent_id, importance FROM action_items
WHERE user_id = sqlc.arg(user_id) AND source = sqlc.arg(source) AND tracking = 1 AND status IN ('open','candidate')
ORDER BY COALESCE(tracked_at, 0), id LIMIT sqlc.arg(page_size);
-- name: ListStaleActionItems :many
SELECT id, user_id, content, status, note, due_at, refs, version, created_at, updated_at, source, source_key, tracking, auto_reason, auto_ref, auto_at, detected_at, tracked_at, kind, event_title, event_start_at, event_end_at, parent_id, importance FROM action_items
WHERE user_id = sqlc.arg(user_id) AND status = 'open' AND created_at <= sqlc.arg(created_before)
ORDER BY created_at, id LIMIT sqlc.arg(page_size);
-- name: ListDueActionItems :many
SELECT id, user_id, content, status, note, due_at, refs, version, created_at, updated_at, source, source_key, tracking, auto_reason, auto_ref, auto_at, detected_at, tracked_at, kind, event_title, event_start_at, event_end_at, parent_id, importance FROM action_items
WHERE user_id = sqlc.arg(user_id) AND status = 'open' AND kind = 'task' AND due_at IS NOT NULL AND due_at <= sqlc.arg(due_before)
ORDER BY due_at, id LIMIT sqlc.arg(page_size);
-- name: CountActionItemsByStatus :one
SELECT COUNT(*) FROM action_items WHERE user_id = sqlc.arg(user_id) AND status = sqlc.arg(status);
-- name: CountAutoActionItemsSince :one
SELECT COUNT(*) FROM action_items WHERE user_id = sqlc.arg(user_id) AND status = 'done' AND auto_reason IS NOT NULL AND auto_at > sqlc.arg(since);
-- name: UpdateActionItem :execrows
UPDATE action_items SET content = sqlc.arg(content), status = sqlc.arg(status), note = sqlc.arg(note), due_at = sqlc.arg(due_at), refs = sqlc.arg(refs), source = sqlc.arg(source), source_key = sqlc.arg(source_key), tracking = sqlc.arg(tracking), auto_reason = sqlc.arg(auto_reason), auto_ref = sqlc.arg(auto_ref), auto_at = sqlc.arg(auto_at), detected_at = sqlc.arg(detected_at), tracked_at = sqlc.arg(tracked_at), kind = sqlc.arg(kind), event_title = sqlc.arg(event_title), event_start_at = sqlc.arg(event_start_at), event_end_at = sqlc.arg(event_end_at), parent_id = sqlc.arg(parent_id), importance = sqlc.arg(importance), version = sqlc.arg(new_version), updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id) AND version = sqlc.arg(current_version);
-- name: TouchActionItemTracked :execrows
UPDATE action_items SET tracked_at = sqlc.arg(tracked_at)
WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id) AND version = sqlc.arg(version);
-- name: UntrackSourceActionItems :exec
UPDATE action_items SET tracking = 0, source_key = NULL, version = version + 1, updated_at = sqlc.arg(updated_at)
WHERE user_id = sqlc.arg(user_id) AND source = sqlc.arg(source) AND (tracking = 1 OR source_key IS NOT NULL);
-- name: StopTrackingSourceActionItems :exec
UPDATE action_items SET tracking = 0, version = version + 1, updated_at = sqlc.arg(updated_at)
WHERE user_id = sqlc.arg(user_id) AND source = sqlc.arg(source) AND tracking = 1;
-- name: ListCandidateSweepUserIDs :many
SELECT DISTINCT user_id FROM action_items
WHERE (status = 'candidate' OR (kind = 'event' AND status IN ('open','candidate') AND event_start_at IS NOT NULL AND event_start_at <= sqlc.arg(now)))
AND user_id IN (SELECT id FROM users WHERE disabled = 0) ORDER BY user_id;
-- name: ExpireCandidates :many
UPDATE action_items SET status = 'dismissed', tracking = 0, auto_reason = 'candidate.expired', auto_ref = NULL, updated_at = sqlc.arg(now), auto_at = sqlc.arg(now), version = version + 1
WHERE user_id = sqlc.arg(user_id) AND status = 'candidate' AND updated_at <= sqlc.arg(updated_before)
RETURNING parent_id;
-- name: ExpireEvents :many
UPDATE action_items SET status = 'dismissed', tracking = 0, auto_reason = 'event.passed', auto_ref = NULL, updated_at = sqlc.arg(now), auto_at = sqlc.arg(now), version = version + 1
WHERE user_id = sqlc.arg(user_id) AND kind = 'event' AND status IN ('open','candidate') AND event_start_at IS NOT NULL AND event_start_at <= sqlc.arg(now)
RETURNING parent_id;
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
-- name: GetGitHubConnection :one
SELECT user_id, generation, token_sealed, token_last4, login, scopes, immediate_notify, status, last_error, failure_count, last_modified, initial_import_pending, next_poll_at, last_synced_at, last_reconciled_at, last_tracked_at, created_at, updated_at FROM github_connections
WHERE user_id = sqlc.arg(user_id);
-- name: LockGitHubConnection :one
SELECT user_id, generation, token_sealed, token_last4, login, scopes, immediate_notify, status, last_error, failure_count, last_modified, initial_import_pending, next_poll_at, last_synced_at, last_reconciled_at, last_tracked_at, created_at, updated_at FROM github_connections
WHERE user_id = sqlc.arg(user_id) FOR UPDATE;
-- name: CreateGitHubConnection :exec
INSERT INTO github_connections (user_id,generation,token_sealed,token_last4,login,scopes,immediate_notify,status,last_error,failure_count,last_modified,initial_import_pending,next_poll_at,last_synced_at,last_reconciled_at,last_tracked_at,created_at,updated_at)
VALUES (sqlc.arg(user_id),sqlc.arg(generation),sqlc.arg(token_sealed),sqlc.arg(token_last4),sqlc.arg(login),sqlc.arg(scopes),sqlc.arg(immediate_notify),sqlc.arg(status),sqlc.arg(last_error),sqlc.arg(failure_count),sqlc.arg(last_modified),sqlc.arg(initial_import_pending),sqlc.arg(next_poll_at),sqlc.arg(last_synced_at),sqlc.arg(last_reconciled_at),sqlc.arg(last_tracked_at),sqlc.arg(created_at),sqlc.arg(updated_at));
-- name: ReplaceGitHubConnection :exec
UPDATE github_connections SET generation = sqlc.arg(generation), token_sealed = sqlc.arg(token_sealed), token_last4 = sqlc.arg(token_last4), login = sqlc.arg(login), scopes = sqlc.arg(scopes), immediate_notify = sqlc.arg(immediate_notify), status = sqlc.arg(status), last_error = sqlc.arg(last_error), failure_count = sqlc.arg(failure_count), last_modified = sqlc.arg(last_modified), initial_import_pending = sqlc.arg(initial_import_pending), next_poll_at = sqlc.arg(next_poll_at), last_synced_at = sqlc.arg(last_synced_at), last_reconciled_at = sqlc.arg(last_reconciled_at), last_tracked_at = sqlc.arg(last_tracked_at), updated_at = sqlc.arg(updated_at)
WHERE user_id = sqlc.arg(user_id);
-- name: UpdateGitHubSyncState :execrows
UPDATE github_connections SET status = sqlc.arg(status), last_error = sqlc.arg(last_error), failure_count = sqlc.arg(failure_count), last_modified = sqlc.arg(last_modified), initial_import_pending = sqlc.arg(initial_import_pending), next_poll_at = sqlc.arg(next_poll_at), last_synced_at = sqlc.arg(last_synced_at), last_reconciled_at = sqlc.arg(last_reconciled_at), last_tracked_at = sqlc.arg(last_tracked_at), updated_at = sqlc.arg(updated_at)
WHERE user_id = sqlc.arg(user_id) AND generation = sqlc.arg(generation);
-- name: UpdateGitHubImmediateNotify :execrows
UPDATE github_connections SET immediate_notify = sqlc.arg(immediate_notify), updated_at = sqlc.arg(updated_at)
WHERE user_id = sqlc.arg(user_id);
-- name: DeleteGitHubConnection :exec
DELETE FROM github_connections WHERE user_id = sqlc.arg(user_id);
-- name: ListDueGitHubConnections :many
SELECT user_id, generation, token_sealed, token_last4, login, scopes, immediate_notify, status, last_error, failure_count, last_modified, initial_import_pending, next_poll_at, last_synced_at, last_reconciled_at, last_tracked_at, created_at, updated_at FROM github_connections
WHERE status = 'active' AND next_poll_at <= sqlc.arg(now) AND user_id IN (SELECT id FROM users WHERE disabled = 0)
ORDER BY next_poll_at, user_id LIMIT sqlc.arg(page_size);
-- name: GetSlackConnection :one
SELECT user_id, generation, token_sealed, token_last4, team_id, team_name, team_url, slack_user_id, slack_user_name, scopes, immediate_notify, detect_mentions, detect_dms, detect_group_dms, status, last_error, failure_count, connected_at, next_poll_at, last_synced_at, last_tracked_at, created_at, updated_at FROM slack_connections
WHERE user_id = sqlc.arg(user_id);
-- name: LockSlackConnection :one
SELECT user_id, generation, token_sealed, token_last4, team_id, team_name, team_url, slack_user_id, slack_user_name, scopes, immediate_notify, detect_mentions, detect_dms, detect_group_dms, status, last_error, failure_count, connected_at, next_poll_at, last_synced_at, last_tracked_at, created_at, updated_at FROM slack_connections
WHERE user_id = sqlc.arg(user_id) FOR UPDATE;
-- name: CreateSlackConnection :exec
INSERT INTO slack_connections (user_id,generation,token_sealed,token_last4,team_id,team_name,team_url,slack_user_id,slack_user_name,scopes,immediate_notify,detect_mentions,detect_dms,detect_group_dms,status,last_error,failure_count,connected_at,next_poll_at,last_synced_at,last_tracked_at,created_at,updated_at)
VALUES (sqlc.arg(user_id),sqlc.arg(generation),sqlc.arg(token_sealed),sqlc.arg(token_last4),sqlc.arg(team_id),sqlc.arg(team_name),sqlc.arg(team_url),sqlc.arg(slack_user_id),sqlc.arg(slack_user_name),sqlc.arg(scopes),sqlc.arg(immediate_notify),sqlc.arg(detect_mentions),sqlc.arg(detect_dms),sqlc.arg(detect_group_dms),sqlc.arg(status),sqlc.arg(last_error),sqlc.arg(failure_count),sqlc.arg(connected_at),sqlc.arg(next_poll_at),sqlc.arg(last_synced_at),sqlc.arg(last_tracked_at),sqlc.arg(created_at),sqlc.arg(updated_at));
-- name: ReplaceSlackConnection :exec
UPDATE slack_connections SET generation = sqlc.arg(generation), token_sealed = sqlc.arg(token_sealed), token_last4 = sqlc.arg(token_last4), team_id = sqlc.arg(team_id), team_name = sqlc.arg(team_name), team_url = sqlc.arg(team_url), slack_user_id = sqlc.arg(slack_user_id), slack_user_name = sqlc.arg(slack_user_name), scopes = sqlc.arg(scopes), immediate_notify = sqlc.arg(immediate_notify), detect_mentions = sqlc.arg(detect_mentions), detect_dms = sqlc.arg(detect_dms), detect_group_dms = sqlc.arg(detect_group_dms), status = sqlc.arg(status), last_error = sqlc.arg(last_error), failure_count = sqlc.arg(failure_count), connected_at = sqlc.arg(connected_at), next_poll_at = sqlc.arg(next_poll_at), last_synced_at = sqlc.arg(last_synced_at), last_tracked_at = sqlc.arg(last_tracked_at), updated_at = sqlc.arg(updated_at)
WHERE user_id = sqlc.arg(user_id);
-- name: UpdateSlackSyncState :execrows
UPDATE slack_connections SET status = sqlc.arg(status), last_error = sqlc.arg(last_error), failure_count = sqlc.arg(failure_count), next_poll_at = sqlc.arg(next_poll_at), last_synced_at = sqlc.arg(last_synced_at), last_tracked_at = sqlc.arg(last_tracked_at), updated_at = sqlc.arg(updated_at)
WHERE user_id = sqlc.arg(user_id) AND generation = sqlc.arg(generation);
-- name: UpdateSlackSettings :execrows
UPDATE slack_connections SET immediate_notify = sqlc.arg(immediate_notify), detect_mentions = sqlc.arg(detect_mentions), detect_dms = sqlc.arg(detect_dms), detect_group_dms = sqlc.arg(detect_group_dms), updated_at = sqlc.arg(updated_at)
WHERE user_id = sqlc.arg(user_id);
-- name: DeleteSlackConnection :exec
DELETE FROM slack_connections WHERE user_id = sqlc.arg(user_id);
-- name: ListDueSlackConnections :many
SELECT user_id, generation, token_sealed, token_last4, team_id, team_name, team_url, slack_user_id, slack_user_name, scopes, immediate_notify, detect_mentions, detect_dms, detect_group_dms, status, last_error, failure_count, connected_at, next_poll_at, last_synced_at, last_tracked_at, created_at, updated_at FROM slack_connections
WHERE status = 'active' AND next_poll_at <= sqlc.arg(now) AND user_id IN (SELECT id FROM users WHERE disabled = 0)
ORDER BY next_poll_at, user_id LIMIT sqlc.arg(page_size);
-- name: ListSlackJudgedKeys :many
SELECT source_key FROM action_items
WHERE user_id = sqlc.arg(user_id) AND source = 'slack' AND substr(source_key, 1, length(sqlc.arg(prefix)::text)) = sqlc.arg(prefix)::text AND created_at >= sqlc.arg(created_since)
ORDER BY created_at DESC, id DESC LIMIT sqlc.arg(page_size);
-- name: GetChatworkConnection :one
SELECT user_id, generation, token_sealed, token_last4, account_id, account_name, organization_name, immediate_notify, detect_mentions, detect_dms, import_tasks, status, last_error, failure_count, connected_at, next_poll_at, last_synced_at, last_tracked_at, last_tasks_at, created_at, updated_at FROM chatwork_connections
WHERE user_id = sqlc.arg(user_id);
-- name: LockChatworkConnection :one
SELECT user_id, generation, token_sealed, token_last4, account_id, account_name, organization_name, immediate_notify, detect_mentions, detect_dms, import_tasks, status, last_error, failure_count, connected_at, next_poll_at, last_synced_at, last_tracked_at, last_tasks_at, created_at, updated_at FROM chatwork_connections
WHERE user_id = sqlc.arg(user_id) FOR UPDATE;
-- name: CreateChatworkConnection :exec
INSERT INTO chatwork_connections (user_id,generation,token_sealed,token_last4,account_id,account_name,organization_name,immediate_notify,detect_mentions,detect_dms,import_tasks,status,last_error,failure_count,connected_at,next_poll_at,last_synced_at,last_tracked_at,last_tasks_at,created_at,updated_at)
VALUES (sqlc.arg(user_id),sqlc.arg(generation),sqlc.arg(token_sealed),sqlc.arg(token_last4),sqlc.arg(account_id),sqlc.arg(account_name),sqlc.arg(organization_name),sqlc.arg(immediate_notify),sqlc.arg(detect_mentions),sqlc.arg(detect_dms),sqlc.arg(import_tasks),sqlc.arg(status),sqlc.arg(last_error),sqlc.arg(failure_count),sqlc.arg(connected_at),sqlc.arg(next_poll_at),sqlc.arg(last_synced_at),sqlc.arg(last_tracked_at),sqlc.arg(last_tasks_at),sqlc.arg(created_at),sqlc.arg(updated_at));
-- name: ReplaceChatworkConnection :exec
UPDATE chatwork_connections SET generation = sqlc.arg(generation), token_sealed = sqlc.arg(token_sealed), token_last4 = sqlc.arg(token_last4), account_id = sqlc.arg(account_id), account_name = sqlc.arg(account_name), organization_name = sqlc.arg(organization_name), immediate_notify = sqlc.arg(immediate_notify), detect_mentions = sqlc.arg(detect_mentions), detect_dms = sqlc.arg(detect_dms), import_tasks = sqlc.arg(import_tasks), status = sqlc.arg(status), last_error = sqlc.arg(last_error), failure_count = sqlc.arg(failure_count), connected_at = sqlc.arg(connected_at), next_poll_at = sqlc.arg(next_poll_at), last_synced_at = sqlc.arg(last_synced_at), last_tracked_at = sqlc.arg(last_tracked_at), last_tasks_at = sqlc.arg(last_tasks_at), updated_at = sqlc.arg(updated_at)
WHERE user_id = sqlc.arg(user_id);
-- name: UpdateChatworkSyncState :execrows
UPDATE chatwork_connections SET status = sqlc.arg(status), last_error = sqlc.arg(last_error), failure_count = sqlc.arg(failure_count), next_poll_at = sqlc.arg(next_poll_at), last_synced_at = sqlc.arg(last_synced_at), last_tracked_at = sqlc.arg(last_tracked_at), last_tasks_at = sqlc.arg(last_tasks_at), updated_at = sqlc.arg(updated_at)
WHERE user_id = sqlc.arg(user_id) AND generation = sqlc.arg(generation);
-- name: UpdateChatworkSettings :execrows
UPDATE chatwork_connections SET immediate_notify = sqlc.arg(immediate_notify), detect_mentions = sqlc.arg(detect_mentions), detect_dms = sqlc.arg(detect_dms), import_tasks = sqlc.arg(import_tasks), updated_at = sqlc.arg(updated_at)
WHERE user_id = sqlc.arg(user_id);
-- name: DeleteChatworkConnection :exec
DELETE FROM chatwork_connections WHERE user_id = sqlc.arg(user_id);
-- name: ListDueChatworkConnections :many
SELECT user_id, generation, token_sealed, token_last4, account_id, account_name, organization_name, immediate_notify, detect_mentions, detect_dms, import_tasks, status, last_error, failure_count, connected_at, next_poll_at, last_synced_at, last_tracked_at, last_tasks_at, created_at, updated_at FROM chatwork_connections
WHERE status = 'active' AND next_poll_at <= sqlc.arg(now) AND user_id IN (SELECT id FROM users WHERE disabled = 0)
ORDER BY next_poll_at, user_id LIMIT sqlc.arg(page_size);
-- name: ListChatworkJudgedKeys :many
SELECT source_key FROM action_items
WHERE user_id = sqlc.arg(user_id) AND source = 'chatwork' AND substr(source_key, 1, length(sqlc.arg(prefix)::text)) = sqlc.arg(prefix)::text AND created_at >= sqlc.arg(created_since)
ORDER BY created_at DESC, id DESC LIMIT sqlc.arg(page_size);
-- name: ListChatworkTaskItems :many
SELECT id, user_id, content, status, note, due_at, refs, version, created_at, updated_at, source, source_key, tracking, auto_reason, auto_ref, auto_at, detected_at, tracked_at, kind, event_title, event_start_at, event_end_at, parent_id, importance FROM action_items
WHERE user_id = sqlc.arg(user_id) AND source = 'chatwork' AND status = 'open' AND substr(source_key, 1, length(sqlc.arg(prefix)::text)) = sqlc.arg(prefix)::text
ORDER BY created_at, id LIMIT sqlc.arg(page_size);
-- name: ListGmailAccounts :many
SELECT id, user_id, email, generation, token_sealed, immediate_notify, status, last_error, failure_count, connected_at, next_poll_at, last_synced_at, last_tracked_at, created_at, updated_at FROM gmail_accounts
WHERE user_id = sqlc.arg(user_id) ORDER BY email;
-- name: GetGmailAccount :one
SELECT id, user_id, email, generation, token_sealed, immediate_notify, status, last_error, failure_count, connected_at, next_poll_at, last_synced_at, last_tracked_at, created_at, updated_at FROM gmail_accounts
WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id);
-- name: LockGmailAccount :one
SELECT id, user_id, email, generation, token_sealed, immediate_notify, status, last_error, failure_count, connected_at, next_poll_at, last_synced_at, last_tracked_at, created_at, updated_at FROM gmail_accounts
WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id) FOR UPDATE;
-- name: LockGmailAccountByEmail :one
SELECT id, user_id, email, generation, token_sealed, immediate_notify, status, last_error, failure_count, connected_at, next_poll_at, last_synced_at, last_tracked_at, created_at, updated_at FROM gmail_accounts
WHERE email = sqlc.arg(email) FOR UPDATE;
-- name: CountGmailAccounts :one
SELECT COUNT(*) FROM gmail_accounts WHERE user_id = sqlc.arg(user_id);
-- name: CreateGmailAccount :execrows
INSERT INTO gmail_accounts (id,user_id,email,generation,token_sealed,immediate_notify,status,last_error,failure_count,connected_at,next_poll_at,last_synced_at,last_tracked_at,created_at,updated_at)
VALUES (sqlc.arg(id),sqlc.arg(user_id),sqlc.arg(email),sqlc.arg(generation),sqlc.arg(token_sealed),sqlc.arg(immediate_notify),sqlc.arg(status),sqlc.arg(last_error),sqlc.arg(failure_count),sqlc.arg(connected_at),sqlc.arg(next_poll_at),sqlc.arg(last_synced_at),sqlc.arg(last_tracked_at),sqlc.arg(created_at),sqlc.arg(updated_at))
ON CONFLICT (email) DO NOTHING;
-- name: ReconnectGmailAccount :execrows
UPDATE gmail_accounts SET generation = sqlc.arg(generation), token_sealed = sqlc.arg(token_sealed), status = sqlc.arg(status), last_error = sqlc.arg(last_error), failure_count = sqlc.arg(failure_count), connected_at = sqlc.arg(connected_at), next_poll_at = sqlc.arg(next_poll_at), updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id);
-- name: UpdateGmailTokenSealed :execrows
UPDATE gmail_accounts SET token_sealed = sqlc.arg(token_sealed)
WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id) AND generation = sqlc.arg(generation);
-- name: UpdateGmailSyncState :execrows
UPDATE gmail_accounts SET status = sqlc.arg(status), last_error = sqlc.arg(last_error), failure_count = sqlc.arg(failure_count), next_poll_at = sqlc.arg(next_poll_at), last_synced_at = sqlc.arg(last_synced_at), last_tracked_at = sqlc.arg(last_tracked_at), updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id) AND generation = sqlc.arg(generation);
-- name: UpdateGmailSettings :execrows
UPDATE gmail_accounts SET immediate_notify = sqlc.arg(immediate_notify), updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id);
-- name: DeleteGmailAccount :execrows
DELETE FROM gmail_accounts WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id);
-- name: DeleteUserGmailAccounts :exec
DELETE FROM gmail_accounts WHERE user_id = sqlc.arg(user_id);
-- name: ListDueGmailAccounts :many
SELECT id, user_id, email, generation, token_sealed, immediate_notify, status, last_error, failure_count, connected_at, next_poll_at, last_synced_at, last_tracked_at, created_at, updated_at FROM gmail_accounts
WHERE status = 'active' AND next_poll_at <= sqlc.arg(now) AND user_id IN (SELECT id FROM users WHERE disabled = 0)
ORDER BY next_poll_at, id LIMIT sqlc.arg(page_size);
-- name: ListGmailJudgedKeys :many
SELECT source_key FROM action_items
WHERE user_id = sqlc.arg(user_id) AND source = 'gmail' AND substr(source_key, 1, length(sqlc.arg(prefix)::text)) = sqlc.arg(prefix)::text AND created_at >= sqlc.arg(created_since)
ORDER BY created_at DESC, id DESC LIMIT sqlc.arg(page_size);
-- name: CreateGoogleAuthState :exec
INSERT INTO google_auth_states (state_hash,user_id,verifier_sealed,expires_at,created_at)
VALUES (sqlc.arg(state_hash),sqlc.arg(user_id),sqlc.arg(verifier_sealed),sqlc.arg(expires_at),sqlc.arg(created_at));
-- name: GetGoogleAuthState :one
SELECT state_hash, user_id, verifier_sealed, expires_at, created_at FROM google_auth_states
WHERE state_hash = sqlc.arg(state_hash) AND user_id = sqlc.arg(user_id);
-- name: ListGoogleAuthStateHashes :many
SELECT state_hash FROM google_auth_states
WHERE user_id = sqlc.arg(user_id) ORDER BY created_at DESC, state_hash DESC;
-- name: DeleteGoogleAuthState :execrows
DELETE FROM google_auth_states WHERE state_hash = sqlc.arg(state_hash) AND user_id = sqlc.arg(user_id);
-- name: DeleteExpiredGoogleAuthStates :exec
DELETE FROM google_auth_states WHERE user_id = sqlc.arg(user_id) AND expires_at <= sqlc.arg(now);
-- name: DeleteUserGoogleAuthStates :exec
DELETE FROM google_auth_states WHERE user_id = sqlc.arg(user_id);
-- name: StopTrackingSourcePrefixItems :exec
UPDATE action_items SET tracking = 0, version = version + 1, updated_at = sqlc.arg(updated_at)
WHERE user_id = sqlc.arg(user_id) AND source = sqlc.arg(source) AND substr(source_key, 1, length(sqlc.arg(prefix)::text)) = sqlc.arg(prefix)::text AND tracking = 1;
-- name: ListTrackedSourcePrefixItems :many
SELECT id, user_id, content, status, note, due_at, refs, version, created_at, updated_at, source, source_key, tracking, auto_reason, auto_ref, auto_at, detected_at, tracked_at, kind, event_title, event_start_at, event_end_at, parent_id, importance FROM action_items
WHERE user_id = sqlc.arg(user_id) AND source = sqlc.arg(source) AND substr(source_key, 1, length(sqlc.arg(prefix)::text)) = sqlc.arg(prefix)::text AND tracking = 1 AND status IN ('open','candidate')
ORDER BY COALESCE(tracked_at, 0), id LIMIT sqlc.arg(page_size);
-- name: ListAIDetectSamples :many
SELECT id, user_id, content, status, note, due_at, refs, version, created_at, updated_at, source, source_key, tracking, auto_reason, auto_ref, auto_at, detected_at, tracked_at, kind, event_title, event_start_at, event_end_at, parent_id, importance FROM action_items
WHERE user_id = sqlc.arg(user_id) AND source = sqlc.arg(source) AND substr(source_key, 1, length(sqlc.arg(prefix)::text)) = sqlc.arg(prefix)::text AND detected_at IS NOT NULL AND substr(content, 1, length(sqlc.arg(fallback)::text)) <> sqlc.arg(fallback)::text
ORDER BY detected_at DESC, id DESC LIMIT sqlc.arg(page_size);
-- name: ListAIResolvedSamples :many
SELECT id, user_id, content, status, note, due_at, refs, version, created_at, updated_at, source, source_key, tracking, auto_reason, auto_ref, auto_at, detected_at, tracked_at, kind, event_title, event_start_at, event_end_at, parent_id, importance FROM action_items
WHERE user_id = sqlc.arg(user_id) AND source = sqlc.arg(source) AND substr(source_key, 1, length(sqlc.arg(prefix)::text)) = sqlc.arg(prefix)::text AND auto_reason = sqlc.arg(auto_reason)
ORDER BY COALESCE(tracked_at, detected_at, 0) DESC, id DESC LIMIT sqlc.arg(page_size);
-- name: ListAITrackedSamples :many
SELECT id, user_id, content, status, note, due_at, refs, version, created_at, updated_at, source, source_key, tracking, auto_reason, auto_ref, auto_at, detected_at, tracked_at, kind, event_title, event_start_at, event_end_at, parent_id, importance FROM action_items
WHERE user_id = sqlc.arg(user_id) AND source = sqlc.arg(source) AND substr(source_key, 1, length(sqlc.arg(prefix)::text)) = sqlc.arg(prefix)::text AND status IN ('open','candidate') AND tracking = 1 AND tracked_at IS NOT NULL
ORDER BY tracked_at DESC, id DESC LIMIT sqlc.arg(page_size);
-- name: DeleteUserPendingNotificationsByKindAndSourcePrefix :exec
DELETE FROM notifications WHERE notifications.user_id = sqlc.arg(user_id) AND notifications.kind = sqlc.arg(kind) AND notifications.sent_at IS NULL
AND notifications.action_item_id IN (SELECT action_items.id FROM action_items WHERE action_items.user_id = sqlc.arg(item_user_id) AND action_items.source = sqlc.arg(source) AND substr(action_items.source_key, 1, length(sqlc.arg(prefix)::text)) = sqlc.arg(prefix)::text);
-- name: ListChildActionItems :many
SELECT id, user_id, content, status, note, due_at, refs, version, created_at, updated_at, source, source_key, tracking, auto_reason, auto_ref, auto_at, detected_at, tracked_at, kind, event_title, event_start_at, event_end_at, parent_id, importance FROM action_items
WHERE user_id = sqlc.arg(user_id) AND parent_id = sqlc.arg(parent_id)
ORDER BY created_at, id LIMIT sqlc.arg(page_size);
-- name: CountChildActionItems :one
SELECT COUNT(*) FROM action_items WHERE user_id = sqlc.arg(user_id) AND parent_id = sqlc.arg(parent_id);
-- name: CountChildrenByParents :many
SELECT parent_id, COUNT(*)::bigint AS children, COALESCE(SUM(CASE WHEN status <> 'dismissed' THEN 1 ELSE 0 END), 0)::bigint AS total, COALESCE(SUM(CASE WHEN status = 'done' THEN 1 ELSE 0 END), 0)::bigint AS done, COALESCE(SUM(CASE WHEN status IN ('open','candidate') THEN 1 ELSE 0 END), 0)::bigint AS remaining, COALESCE(MIN(CASE WHEN status IN ('open','candidate') THEN due_at END), -1)::bigint AS next_due_at FROM action_items
WHERE user_id = sqlc.arg(user_id) AND strpos(',' || sqlc.arg(ids)::text || ',', ',' || parent_id || ',') > 0
GROUP BY parent_id;
-- name: GetActionItemsByIDs :many
SELECT id, content, status FROM action_items
WHERE user_id = sqlc.arg(user_id) AND strpos(',' || sqlc.arg(ids)::text || ',', ',' || id || ',') > 0;
-- name: ListConversationActionItems :many
SELECT id, user_id, content, status, note, due_at, refs, version, created_at, updated_at, source, source_key, tracking, auto_reason, auto_ref, auto_at, detected_at, tracked_at, kind, event_title, event_start_at, event_end_at, parent_id, importance FROM action_items
WHERE user_id = sqlc.arg(user_id) AND source = sqlc.arg(source) AND substr(source_key, 1, length(sqlc.arg(prefix)::text)) = sqlc.arg(prefix)::text AND created_at >= sqlc.arg(created_since)
AND (status = 'open' OR (status = 'done' AND auto_reason = 'children.done' AND auto_at >= sqlc.arg(closed_since)))
ORDER BY created_at DESC, id DESC LIMIT sqlc.arg(page_size);
-- name: GetActionItemBySourceKey :one
SELECT id, user_id, content, status, note, due_at, refs, version, created_at, updated_at, source, source_key, tracking, auto_reason, auto_ref, auto_at, detected_at, tracked_at, kind, event_title, event_start_at, event_end_at, parent_id, importance FROM action_items
WHERE user_id = sqlc.arg(user_id) AND source_key = sqlc.arg(source_key);
-- name: GetAISettings :one
SELECT user_id, generation, provider, model, api_key_sealed, key_last4, status, last_error, activated_at, created_at, updated_at FROM ai_settings
WHERE user_id = sqlc.arg(user_id);
-- name: LockAISettings :one
SELECT user_id, generation, provider, model, api_key_sealed, key_last4, status, last_error, activated_at, created_at, updated_at FROM ai_settings
WHERE user_id = sqlc.arg(user_id) FOR UPDATE;
-- name: CreateAISettings :execrows
INSERT INTO ai_settings (user_id,generation,provider,model,api_key_sealed,key_last4,status,last_error,activated_at,created_at,updated_at)
VALUES (sqlc.arg(user_id),sqlc.arg(generation),sqlc.arg(provider),sqlc.arg(model),sqlc.arg(api_key_sealed),sqlc.arg(key_last4),sqlc.arg(status),sqlc.arg(last_error),sqlc.arg(activated_at),sqlc.arg(created_at),sqlc.arg(updated_at))
ON CONFLICT (user_id) DO NOTHING;
-- name: ReplaceAISettings :execrows
UPDATE ai_settings SET generation = sqlc.arg(generation), provider = sqlc.arg(provider), model = sqlc.arg(model), api_key_sealed = sqlc.arg(api_key_sealed), key_last4 = sqlc.arg(key_last4), status = sqlc.arg(status), last_error = sqlc.arg(last_error), activated_at = sqlc.arg(activated_at), updated_at = sqlc.arg(updated_at)
WHERE user_id = sqlc.arg(user_id) AND generation = sqlc.arg(current_generation);
-- name: UpdateAISettingsStatus :execrows
UPDATE ai_settings SET status = sqlc.arg(status), last_error = sqlc.arg(last_error), updated_at = sqlc.arg(updated_at)
WHERE user_id = sqlc.arg(user_id) AND generation = sqlc.arg(generation);
-- name: UpdateAISecret :execrows
UPDATE ai_settings SET api_key_sealed = sqlc.arg(new_sealed)
WHERE user_id = sqlc.arg(user_id) AND generation = sqlc.arg(generation) AND provider = 'openai' AND api_key_sealed = sqlc.arg(old_sealed);
-- name: DeleteAISettings :exec
DELETE FROM ai_settings WHERE user_id = sqlc.arg(user_id);
-- name: ListAIInstructions :many
SELECT user_id, scope, body, updated_at FROM ai_instructions
WHERE user_id = sqlc.arg(user_id) ORDER BY scope;
-- name: UpsertAIInstruction :exec
INSERT INTO ai_instructions (user_id,scope,body,updated_at)
VALUES (sqlc.arg(user_id),sqlc.arg(scope),sqlc.arg(body),sqlc.arg(updated_at))
ON CONFLICT (user_id, scope) DO UPDATE SET body = excluded.body, updated_at = excluded.updated_at;
-- name: DeleteAIInstruction :exec
DELETE FROM ai_instructions WHERE user_id = sqlc.arg(user_id) AND scope = sqlc.arg(scope);
-- name: DeleteUserAIInstructions :exec
DELETE FROM ai_instructions WHERE user_id = sqlc.arg(user_id);
-- name: CreatePushSubscription :exec
INSERT INTO web_push_subscriptions (id,user_id,endpoint,p256dh,auth,label,created_at,last_used_at)
VALUES (sqlc.arg(id),sqlc.arg(user_id),sqlc.arg(endpoint),sqlc.arg(p256dh),sqlc.arg(auth),sqlc.arg(label),sqlc.arg(created_at),NULL);
-- name: LockPushSubscriptionByEndpoint :one
SELECT id, user_id, endpoint, p256dh, auth, label, created_at, last_used_at FROM web_push_subscriptions
WHERE endpoint = sqlc.arg(endpoint) FOR UPDATE;
-- name: UpdatePushSubscriptionKeys :exec
UPDATE web_push_subscriptions SET p256dh = sqlc.arg(p256dh), auth = sqlc.arg(auth), label = sqlc.arg(label)
WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id);
-- name: CountPushSubscriptions :one
SELECT COUNT(*) FROM web_push_subscriptions WHERE user_id = sqlc.arg(user_id);
-- name: ListPushSubscriptions :many
SELECT id, user_id, endpoint, p256dh, auth, label, created_at, last_used_at FROM web_push_subscriptions
WHERE user_id = sqlc.arg(user_id) ORDER BY created_at DESC, id DESC;
-- name: ListNotificationUserIDs :many
SELECT user_id FROM web_push_subscriptions
WHERE user_id IN (SELECT id FROM users WHERE disabled = 0)
UNION
SELECT user_id FROM notification_channels
WHERE ((slack_kinds <> '' AND slack_webhook_sealed IS NOT NULL AND slack_last_error IS NULL) OR email_kinds <> '')
AND user_id IN (SELECT id FROM users WHERE disabled = 0)
ORDER BY user_id;
-- name: TouchPushSubscription :exec
UPDATE web_push_subscriptions SET last_used_at = sqlc.arg(last_used_at) WHERE id = sqlc.arg(id);
-- name: DeletePushSubscription :execrows
DELETE FROM web_push_subscriptions WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id);
-- name: DeletePushSubscriptionByID :exec
DELETE FROM web_push_subscriptions WHERE id = sqlc.arg(id);
-- name: DeleteUserPushSubscriptions :exec
DELETE FROM web_push_subscriptions WHERE user_id = sqlc.arg(user_id);
-- name: DeleteAllPushSubscriptions :exec
DELETE FROM web_push_subscriptions;
-- name: GetNotificationSettings :one
SELECT user_id, timezone, digest_hour, stale_days, updated_at, candidate_expire_days FROM notification_settings
WHERE user_id = sqlc.arg(user_id);
-- name: UpsertNotificationSettings :exec
INSERT INTO notification_settings (user_id,timezone,digest_hour,stale_days,updated_at,candidate_expire_days)
VALUES (sqlc.arg(user_id),sqlc.arg(timezone),sqlc.arg(digest_hour),sqlc.arg(stale_days),sqlc.arg(updated_at),sqlc.arg(candidate_expire_days))
ON CONFLICT (user_id) DO UPDATE SET timezone = excluded.timezone, digest_hour = excluded.digest_hour, stale_days = excluded.stale_days, updated_at = excluded.updated_at, candidate_expire_days = excluded.candidate_expire_days;
-- name: CreateNotificationSettingsIfMissing :exec
INSERT INTO notification_settings (user_id,timezone,digest_hour,stale_days,updated_at,candidate_expire_days)
VALUES (sqlc.arg(user_id),sqlc.arg(timezone),sqlc.arg(digest_hour),sqlc.arg(stale_days),sqlc.arg(updated_at),sqlc.arg(candidate_expire_days))
ON CONFLICT (user_id) DO NOTHING;
-- name: EnqueueNotification :execrows
INSERT INTO notifications (id,user_id,kind,action_item_id,dedupe_key,title,body,url,scheduled_at,sent_at,attempts,last_error,created_at,channel)
VALUES (sqlc.arg(id),sqlc.arg(user_id),sqlc.arg(kind),sqlc.arg(action_item_id),sqlc.arg(dedupe_key),sqlc.arg(title),sqlc.arg(body),sqlc.arg(url),sqlc.arg(scheduled_at),sqlc.arg(sent_at),sqlc.arg(attempts),sqlc.arg(last_error),sqlc.arg(created_at),sqlc.arg(channel))
ON CONFLICT (user_id, dedupe_key, channel) DO NOTHING;
-- name: CountNotificationsByDedupeKey :one
SELECT COUNT(*) FROM notifications WHERE user_id = sqlc.arg(user_id) AND dedupe_key = sqlc.arg(dedupe_key);
-- name: LatestNotificationCreatedAt :one
SELECT created_at FROM notifications WHERE user_id = sqlc.arg(user_id) AND kind = sqlc.arg(kind)
ORDER BY created_at DESC LIMIT 1;
-- name: ListDueNotifications :many
SELECT id, user_id, kind, action_item_id, dedupe_key, title, body, url, scheduled_at, sent_at, attempts, last_error, created_at, channel FROM notifications
WHERE sent_at IS NULL AND scheduled_at <= sqlc.arg(now) AND user_id IN (SELECT id FROM users WHERE disabled = 0)
ORDER BY scheduled_at, id LIMIT sqlc.arg(page_size);
-- name: UpdateNotificationResult :exec
UPDATE notifications SET attempts = sqlc.arg(attempts), last_error = sqlc.arg(last_error), scheduled_at = sqlc.arg(scheduled_at), sent_at = sqlc.arg(sent_at)
WHERE id = sqlc.arg(id) AND sent_at IS NULL;
-- name: DeleteItemPendingNotifications :exec
DELETE FROM notifications WHERE user_id = sqlc.arg(user_id) AND action_item_id = sqlc.arg(action_item_id) AND sent_at IS NULL;
-- name: DeleteUserPendingNotifications :exec
DELETE FROM notifications WHERE user_id = sqlc.arg(user_id) AND sent_at IS NULL;
-- name: DeleteUserPendingNotificationsByKindAndSource :exec
DELETE FROM notifications WHERE notifications.user_id = sqlc.arg(user_id) AND notifications.kind = sqlc.arg(kind) AND notifications.sent_at IS NULL
AND notifications.action_item_id IN (SELECT action_items.id FROM action_items WHERE action_items.user_id = sqlc.arg(item_user_id) AND action_items.source = sqlc.arg(source));
-- name: DeleteSentNotificationsBefore :exec
DELETE FROM notifications WHERE sent_at IS NOT NULL AND sent_at < sqlc.arg(before);
-- name: GetNotificationChannels :one
SELECT user_id, webpush_kinds, slack_kinds, email_kinds, slack_webhook_sealed, slack_last_error, updated_at FROM notification_channels
WHERE user_id = sqlc.arg(user_id);
-- name: UpsertNotificationRoutes :exec
INSERT INTO notification_channels (user_id,webpush_kinds,slack_kinds,email_kinds,slack_webhook_sealed,slack_last_error,updated_at)
VALUES (sqlc.arg(user_id),sqlc.arg(webpush_kinds),sqlc.arg(slack_kinds),sqlc.arg(email_kinds),NULL,NULL,sqlc.arg(updated_at))
ON CONFLICT (user_id) DO UPDATE SET webpush_kinds = excluded.webpush_kinds, slack_kinds = excluded.slack_kinds, email_kinds = excluded.email_kinds, updated_at = excluded.updated_at;
-- name: UpsertSlackWebhook :exec
INSERT INTO notification_channels (user_id,slack_webhook_sealed,slack_last_error,updated_at)
VALUES (sqlc.arg(user_id),sqlc.arg(slack_webhook_sealed),NULL,sqlc.arg(updated_at))
ON CONFLICT (user_id) DO UPDATE SET slack_webhook_sealed = excluded.slack_webhook_sealed, slack_last_error = NULL, updated_at = excluded.updated_at;
-- name: ClearSlackWebhook :exec
UPDATE notification_channels SET slack_webhook_sealed = NULL, slack_last_error = NULL, updated_at = sqlc.arg(updated_at)
WHERE user_id = sqlc.arg(user_id);
-- name: MarkSlackWebhookInvalid :execrows
UPDATE notification_channels SET slack_last_error = sqlc.arg(slack_last_error)
WHERE user_id = sqlc.arg(user_id) AND slack_webhook_sealed = sqlc.arg(slack_webhook_sealed);
-- name: GetInstanceSecret :one
SELECT name, value, created_at, updated_at FROM instance_secrets WHERE name = sqlc.arg(name);
-- name: CreateInstanceSecret :execrows
INSERT INTO instance_secrets (name,value,created_at,updated_at)
VALUES (sqlc.arg(name),sqlc.arg(value),sqlc.arg(created_at),sqlc.arg(updated_at))
ON CONFLICT (name) DO NOTHING;
-- name: ReplaceInstanceSecret :execrows
UPDATE instance_secrets SET value = sqlc.arg(new_value), updated_at = sqlc.arg(updated_at)
WHERE name = sqlc.arg(name) AND value = sqlc.arg(old_value);
-- name: CreateInvitation :exec
INSERT INTO invitations (id,token_hash,email,role,invited_by,created_at,expires_at)
VALUES (sqlc.arg(id),sqlc.arg(token_hash),sqlc.arg(email),sqlc.arg(role),sqlc.arg(invited_by),sqlc.arg(created_at),sqlc.arg(expires_at));
-- name: GetInvitationByTokenHash :one
SELECT id, token_hash, email, role, invited_by, created_at, expires_at, accepted_at, accepted_user_id, revoked_at FROM invitations WHERE token_hash = sqlc.arg(token_hash);
-- name: LockInvitationByTokenHash :one
SELECT id, token_hash, email, role, invited_by, created_at, expires_at, accepted_at, accepted_user_id, revoked_at FROM invitations WHERE token_hash = sqlc.arg(token_hash) FOR UPDATE;
-- name: LockActiveInvitationsByEmail :many
SELECT id, token_hash, email, role, invited_by, created_at, expires_at, accepted_at, accepted_user_id, revoked_at FROM invitations
WHERE email = sqlc.arg(email) AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > sqlc.arg(now)
ORDER BY id FOR UPDATE;
-- name: AcceptInvitation :execrows
UPDATE invitations SET accepted_at = sqlc.arg(accepted_at), accepted_user_id = sqlc.arg(accepted_user_id)
WHERE id = sqlc.arg(id) AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > sqlc.arg(now);
-- name: RevokeInvitation :execrows
UPDATE invitations SET revoked_at = sqlc.arg(revoked_at)
WHERE id = sqlc.arg(id) AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > sqlc.arg(now);
-- name: RevokeEmailInvitations :exec
UPDATE invitations SET revoked_at = sqlc.arg(revoked_at)
WHERE email = sqlc.arg(email) AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > sqlc.arg(now);
-- name: RevokeIssuedInvitations :exec
UPDATE invitations SET revoked_at = sqlc.arg(revoked_at)
WHERE invited_by = sqlc.arg(invited_by) AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > sqlc.arg(now);
-- name: ListActiveInvitations :many
SELECT id, token_hash, email, role, invited_by, created_at, expires_at, accepted_at, accepted_user_id, revoked_at FROM invitations
WHERE accepted_at IS NULL AND revoked_at IS NULL AND expires_at > sqlc.arg(now)
ORDER BY created_at DESC, id DESC LIMIT sqlc.arg(page_size);
-- name: CreatePasswordReset :exec
INSERT INTO password_resets (id,token_hash,user_id,created_by,created_at,expires_at)
VALUES (sqlc.arg(id),sqlc.arg(token_hash),sqlc.arg(user_id),sqlc.arg(created_by),sqlc.arg(created_at),sqlc.arg(expires_at));
-- name: GetPasswordResetByTokenHash :one
SELECT id, token_hash, user_id, created_by, created_at, expires_at, used_at, revoked_at FROM password_resets WHERE token_hash = sqlc.arg(token_hash);
-- name: LockActivePasswordResets :many
SELECT id, token_hash, user_id, created_by, created_at, expires_at, used_at, revoked_at FROM password_resets
WHERE (user_id = sqlc.arg(user_id) OR created_by = sqlc.arg(created_by)) AND used_at IS NULL AND revoked_at IS NULL AND expires_at > sqlc.arg(now)
ORDER BY id FOR UPDATE;
-- name: RevokePasswordReset :exec
UPDATE password_resets SET revoked_at = sqlc.arg(revoked_at) WHERE id = sqlc.arg(id) AND used_at IS NULL AND revoked_at IS NULL;
-- name: UsePasswordReset :execrows
UPDATE password_resets SET used_at = sqlc.arg(used_at) WHERE id = sqlc.arg(id) AND used_at IS NULL AND revoked_at IS NULL;
-- name: LatestUsedPasswordReset :one
SELECT id, token_hash, user_id, created_by, created_at, expires_at, used_at, revoked_at FROM password_resets
WHERE user_id = sqlc.arg(user_id) AND used_at > sqlc.arg(since)
ORDER BY used_at DESC, id DESC LIMIT 1;
-- name: GetMFATOTP :one
SELECT id, user_id, secret_sealed, last_step, created_at FROM mfa_totp WHERE user_id = sqlc.arg(user_id);
-- name: CreateMFATOTP :exec
INSERT INTO mfa_totp (user_id,id,secret_sealed,last_step,created_at)
VALUES (sqlc.arg(user_id),sqlc.arg(id),sqlc.arg(secret_sealed),sqlc.arg(last_step),sqlc.arg(created_at));
-- name: ReplaceMFATOTP :execrows
UPDATE mfa_totp SET id = sqlc.arg(id), secret_sealed = sqlc.arg(secret_sealed), last_step = sqlc.arg(last_step), created_at = sqlc.arg(created_at)
WHERE user_id = sqlc.arg(user_id);
-- name: ConsumeMFATOTPStep :execrows
UPDATE mfa_totp SET last_step = sqlc.arg(step) WHERE user_id = sqlc.arg(user_id) AND id = sqlc.arg(id) AND last_step < sqlc.arg(step);
-- name: DeleteMFATOTP :execrows
DELETE FROM mfa_totp WHERE user_id = sqlc.arg(user_id);
-- name: ListMFATOTPUserIDs :many
SELECT user_id FROM mfa_totp;
-- name: ListMFAWebAuthnCredentials :many
SELECT id, user_id, credential_id, credential, name, created_at, last_used_at, version, passkey FROM mfa_webauthn_credentials
WHERE user_id = sqlc.arg(user_id) ORDER BY created_at, id;
-- name: CountMFAWebAuthnCredentials :one
SELECT COUNT(*) FROM mfa_webauthn_credentials WHERE user_id = sqlc.arg(user_id) AND version <> '';
-- name: CreateMFAWebAuthnCredential :execrows
INSERT INTO mfa_webauthn_credentials (id,user_id,credential_id,credential,name,created_at,version,passkey)
VALUES (sqlc.arg(id),sqlc.arg(user_id),sqlc.arg(credential_id),sqlc.arg(credential),sqlc.arg(name),sqlc.arg(created_at),sqlc.arg(version),sqlc.arg(passkey))
ON CONFLICT (credential_id) DO NOTHING;
-- name: UpdateMFAWebAuthnCredential :execrows
UPDATE mfa_webauthn_credentials SET credential = sqlc.arg(credential), version = sqlc.arg(new_version), last_used_at = sqlc.arg(last_used_at)
WHERE user_id = sqlc.arg(user_id) AND credential_id = sqlc.arg(credential_id) AND version = sqlc.arg(old_version);
-- name: DeleteMFAWebAuthnCredential :execrows
DELETE FROM mfa_webauthn_credentials WHERE user_id = sqlc.arg(user_id) AND id = sqlc.arg(id);
-- name: DeleteUserMFAWebAuthnCredentials :exec
DELETE FROM mfa_webauthn_credentials WHERE user_id = sqlc.arg(user_id);
-- name: ListMFAWebAuthnUserIDs :many
SELECT DISTINCT user_id FROM mfa_webauthn_credentials;
-- name: CreateMFARecoveryCode :exec
INSERT INTO mfa_recovery_codes (id,user_id,code_hash,created_at)
VALUES (sqlc.arg(id),sqlc.arg(user_id),sqlc.arg(code_hash),sqlc.arg(created_at));
-- name: CountUnusedMFARecoveryCodes :one
SELECT COUNT(*) FROM mfa_recovery_codes WHERE user_id = sqlc.arg(user_id) AND used_at IS NULL;
-- name: UseMFARecoveryCode :execrows
UPDATE mfa_recovery_codes SET used_at = sqlc.arg(used_at)
WHERE user_id = sqlc.arg(user_id) AND code_hash = sqlc.arg(code_hash) AND used_at IS NULL;
-- name: DeleteUserMFARecoveryCodes :exec
DELETE FROM mfa_recovery_codes WHERE user_id = sqlc.arg(user_id);
-- name: SetOIDCLinkApproval :exec
UPDATE users SET oidc_link_approved_until = sqlc.narg(approved_until) WHERE id = sqlc.arg(id);
-- name: InsertWebSession :execrows
INSERT INTO web_sessions (id,user_id,authenticated_at,last_activity,absolute_expiry,idle_seconds)
VALUES (sqlc.arg(id),sqlc.arg(user_id),sqlc.arg(authenticated_at),sqlc.arg(last_activity),sqlc.arg(absolute_expiry),sqlc.arg(idle_seconds))
ON CONFLICT (id) DO NOTHING;
-- name: GetLiveWebSession :one
SELECT id, user_id, authenticated_at, last_activity, absolute_expiry, idle_seconds FROM web_sessions
WHERE id = sqlc.arg(id) AND absolute_expiry > sqlc.arg(now)::bigint AND last_activity + idle_seconds > sqlc.arg(now)::bigint;
-- name: TouchLiveWebSession :one
UPDATE web_sessions SET last_activity = GREATEST(last_activity, sqlc.arg(now)::bigint)
WHERE id = sqlc.arg(id) AND absolute_expiry > sqlc.arg(now)::bigint AND last_activity + idle_seconds > sqlc.arg(now)::bigint
RETURNING id, user_id, authenticated_at, last_activity, absolute_expiry, idle_seconds;
-- name: GetActiveWebSession :one
SELECT id, user_id, authenticated_at, last_activity, absolute_expiry, idle_seconds FROM web_sessions
WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id) AND absolute_expiry > sqlc.arg(now)::bigint AND last_activity + idle_seconds > sqlc.arg(now)::bigint;
-- name: DeleteWebSession :exec
DELETE FROM web_sessions WHERE id = sqlc.arg(id);
-- name: DeleteUserWebSessions :exec
DELETE FROM web_sessions WHERE user_id = sqlc.arg(user_id);
-- name: DeleteExpiredWebSessions :exec
DELETE FROM web_sessions WHERE absolute_expiry <= sqlc.arg(now)::bigint OR last_activity + idle_seconds <= sqlc.arg(now)::bigint;
-- name: DeleteExpiredOIDCLogin :exec
DELETE FROM oidc_logins WHERE key = sqlc.arg(key) AND expires_at <= sqlc.arg(now)::bigint;
-- name: DeleteExpiredOIDCLogins :exec
DELETE FROM oidc_logins WHERE expires_at <= sqlc.arg(now)::bigint;
-- name: CountLiveOIDCLogins :one
SELECT COUNT(*) FROM oidc_logins WHERE expires_at > sqlc.arg(now);
-- name: InsertOIDCLogin :execrows
INSERT INTO oidc_logins (key,nonce,verifier,started_at,expires_at,max_age_seconds,app_data)
VALUES (sqlc.arg(key),sqlc.arg(nonce),sqlc.arg(verifier),sqlc.arg(started_at),sqlc.arg(expires_at),sqlc.narg(max_age_seconds),sqlc.narg(app_data))
ON CONFLICT (key) DO NOTHING;
-- name: TakeOIDCLogin :one
DELETE FROM oidc_logins WHERE key = sqlc.arg(key)
RETURNING key, nonce, verifier, started_at, expires_at, max_age_seconds, app_data;
-- name: DeleteExpiredWebAuthnCeremony :exec
DELETE FROM webauthn_ceremonies WHERE key = sqlc.arg(key) AND expires_at <= sqlc.arg(now)::bigint;
-- name: DeleteExpiredWebAuthnCeremonies :exec
DELETE FROM webauthn_ceremonies WHERE expires_at <= sqlc.arg(now)::bigint;
-- name: InsertWebAuthnCeremony :execrows
INSERT INTO webauthn_ceremonies (key,subject,record,expires_at)
VALUES (sqlc.arg(key),sqlc.arg(subject),sqlc.arg(record),sqlc.arg(expires_at))
ON CONFLICT (key) DO NOTHING;
-- name: TakeWebAuthnCeremony :one
DELETE FROM webauthn_ceremonies WHERE key = sqlc.arg(key)
RETURNING key, subject, record, expires_at;
-- name: DeleteSubjectWebAuthnCeremonies :exec
DELETE FROM webauthn_ceremonies WHERE subject = sqlc.arg(subject);
-- name: DeleteExpiredMCPCodes :exec
DELETE FROM mcp_oauth_codes WHERE expires_at <= sqlc.arg(now)::bigint;
-- name: InsertMCPCode :execrows
INSERT INTO mcp_oauth_codes (key,client_id,redirect_uri,subject,scopes,challenge,expires_at,consumed,reused)
VALUES (sqlc.arg(key),sqlc.arg(client_id),sqlc.arg(redirect_uri),sqlc.arg(subject),sqlc.arg(scopes),sqlc.arg(challenge),sqlc.arg(expires_at),0,0)
ON CONFLICT DO NOTHING;
-- name: LockMCPCode :one
SELECT key, client_id, redirect_uri, subject, scopes, challenge, expires_at, consumed, reused FROM mcp_oauth_codes
WHERE key = sqlc.arg(key) FOR UPDATE;
-- name: DeleteMCPCode :exec
DELETE FROM mcp_oauth_codes WHERE key = sqlc.arg(key);
-- name: MarkMCPCodeConsumed :exec
UPDATE mcp_oauth_codes SET consumed = 1 WHERE key = sqlc.arg(key);
-- name: MarkMCPCodeReused :exec
UPDATE mcp_oauth_codes SET reused = 1 WHERE key = sqlc.arg(key);
-- name: DeleteMCPConnectionByCode :many
DELETE FROM mcp_oauth_connections WHERE code = sqlc.arg(code)
RETURNING id, selector, subject, client_id, scopes, created_at, refreshed_at, absolute_expiry, access, access_expiry, refresh, refresh_expiry, code, last_used_at;
-- name: DeleteExpiredMCPConnections :exec
DELETE FROM mcp_oauth_connections
WHERE refresh_expiry <= sqlc.arg(now)::bigint OR absolute_expiry <= sqlc.arg(now)::bigint;
-- name: InsertMCPConnection :execrows
INSERT INTO mcp_oauth_connections (id,selector,subject,client_id,scopes,created_at,refreshed_at,absolute_expiry,access,access_expiry,refresh,refresh_expiry,code,last_used_at)
VALUES (sqlc.arg(id),sqlc.arg(selector),sqlc.arg(subject),sqlc.arg(client_id),sqlc.arg(scopes),sqlc.arg(created_at),sqlc.arg(refreshed_at),sqlc.narg(absolute_expiry),sqlc.arg(access),sqlc.arg(access_expiry),sqlc.arg(refresh),sqlc.arg(refresh_expiry),sqlc.narg(code),NULL)
ON CONFLICT DO NOTHING;
-- name: LockMCPConnectionBySelector :one
SELECT id, selector, subject, client_id, scopes, created_at, refreshed_at, absolute_expiry, access, access_expiry, refresh, refresh_expiry, code, last_used_at FROM mcp_oauth_connections
WHERE selector = sqlc.arg(selector) FOR UPDATE;
-- name: RotateMCPConnection :one
UPDATE mcp_oauth_connections
SET access = sqlc.arg(access), access_expiry = sqlc.arg(access_expiry), refresh = sqlc.arg(refresh), refresh_expiry = sqlc.arg(refresh_expiry), refreshed_at = sqlc.arg(refreshed_at)
WHERE id = sqlc.arg(id)
RETURNING id, selector, subject, client_id, scopes, created_at, refreshed_at, absolute_expiry, access, access_expiry, refresh, refresh_expiry, code, last_used_at;
-- name: GetLiveMCPConnectionBySelector :one
SELECT id, selector, subject, client_id, scopes, created_at, refreshed_at, absolute_expiry, access, access_expiry, refresh, refresh_expiry, code, last_used_at FROM mcp_oauth_connections
WHERE selector = sqlc.arg(selector) AND refresh_expiry > sqlc.arg(now)::bigint
AND (absolute_expiry IS NULL OR absolute_expiry > sqlc.arg(now)::bigint);
-- name: GetLiveMCPConnectionByAccess :one
SELECT id, selector, subject, client_id, scopes, created_at, refreshed_at, absolute_expiry, access, access_expiry, refresh, refresh_expiry, code, last_used_at FROM mcp_oauth_connections
WHERE access = sqlc.arg(access) AND access_expiry > sqlc.arg(now)::bigint
AND (absolute_expiry IS NULL OR absolute_expiry > sqlc.arg(now)::bigint);
-- name: DeleteMCPConnection :exec
DELETE FROM mcp_oauth_connections WHERE id = sqlc.arg(id);
-- name: ListLiveMCPConnections :many
SELECT c.id, c.client_id, c.scopes, c.created_at, c.refresh_expiry, c.absolute_expiry, c.last_used_at,
COALESCE(l.name, '')::text AS client_name, COALESCE(l.host, '')::text AS client_host
FROM mcp_oauth_connections c LEFT JOIN mcp_oauth_client_labels l ON l.client_id = c.client_id
WHERE c.subject = sqlc.arg(subject) AND c.refresh_expiry > sqlc.arg(now)::bigint
AND (c.absolute_expiry IS NULL OR c.absolute_expiry > sqlc.arg(now)::bigint)
ORDER BY c.created_at DESC, c.id DESC;
-- name: DeleteSubjectMCPConnection :execrows
DELETE FROM mcp_oauth_connections WHERE id = sqlc.arg(id) AND subject = sqlc.arg(subject);
-- name: TouchMCPConnection :exec
UPDATE mcp_oauth_connections SET last_used_at = sqlc.arg(now)::bigint
WHERE id = sqlc.arg(id) AND (last_used_at IS NULL OR last_used_at <= sqlc.arg(touched_before)::bigint);
-- name: DeleteSubjectMCPCodes :exec
DELETE FROM mcp_oauth_codes WHERE subject = sqlc.arg(subject);
-- name: DeleteSubjectMCPConnections :exec
DELETE FROM mcp_oauth_connections WHERE subject = sqlc.arg(subject);
-- name: DeleteClientMCPCodes :exec
DELETE FROM mcp_oauth_codes WHERE client_id = sqlc.arg(client_id);
-- name: DeleteClientMCPConnections :execrows
DELETE FROM mcp_oauth_connections WHERE client_id = sqlc.arg(client_id);
-- name: CountClientMCPCodes :one
SELECT COUNT(*) FROM mcp_oauth_codes WHERE client_id = sqlc.arg(client_id);
-- name: CountClientMCPConnections :one
SELECT COUNT(*) FROM mcp_oauth_connections WHERE client_id = sqlc.arg(client_id);
-- name: UpsertMCPClientLabel :exec
INSERT INTO mcp_oauth_client_labels (client_id,name,host,updated_at)
VALUES (sqlc.arg(client_id),sqlc.arg(name),sqlc.arg(host),sqlc.arg(updated_at))
ON CONFLICT (client_id) DO UPDATE SET name = excluded.name, host = excluded.host, updated_at = excluded.updated_at;
-- name: ReleaseMCPRegistrations :exec
UPDATE mcp_oauth_registrations SET expires_at = sqlc.arg(release_at)
WHERE mcp_oauth_registrations.expires_at IS NULL AND mcp_oauth_registrations.created_at <= sqlc.arg(activated_before)::bigint
AND NOT EXISTS (SELECT 1 FROM mcp_oauth_connections c WHERE c.client_id = mcp_oauth_registrations.id)
AND NOT EXISTS (SELECT 1 FROM mcp_oauth_codes k WHERE k.client_id = mcp_oauth_registrations.id);
-- name: DeleteExpiredMCPRegistrations :exec
DELETE FROM mcp_oauth_registrations
WHERE mcp_oauth_registrations.expires_at IS NOT NULL AND mcp_oauth_registrations.expires_at <= sqlc.arg(now)::bigint
AND NOT EXISTS (SELECT 1 FROM mcp_oauth_connections c WHERE c.client_id = mcp_oauth_registrations.id)
AND NOT EXISTS (SELECT 1 FROM mcp_oauth_codes k WHERE k.client_id = mcp_oauth_registrations.id);
-- name: InsertMCPRegistration :execrows
INSERT INTO mcp_oauth_registrations (id,name,redirect_uris,created_at,expires_at)
VALUES (sqlc.arg(id),sqlc.arg(name),sqlc.arg(redirect_uris),sqlc.arg(created_at),sqlc.narg(expires_at))
ON CONFLICT DO NOTHING;
-- name: GetLiveMCPRegistration :one
SELECT id, name, redirect_uris, created_at, expires_at FROM mcp_oauth_registrations
WHERE id = sqlc.arg(id) AND (expires_at IS NULL OR expires_at > sqlc.arg(now)::bigint);
-- name: ActivateMCPRegistration :execrows
UPDATE mcp_oauth_registrations SET expires_at = NULL
WHERE id = sqlc.arg(id) AND (expires_at IS NULL OR expires_at > sqlc.arg(now)::bigint);
