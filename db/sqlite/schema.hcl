schema "main" {}
table "users" {
  schema = schema.main
  column "id" {
    type = text
    null = false
  }
  column "email" {
    type = text
    null = false
  }
  column "password_hash" {
    type = text
    null = false
  }
  column "disabled" {
    type = integer
    null = false
  }
  column "created_at" {
    type = integer
    null = false
  }
  primary_key {
    columns = [column.id]
  }
  index "users_email_key" {
    unique = true
    columns = [column.email]
  }
}
table "sessions" {
  schema = schema.main
  column "id" {
    type = text
    null = false
  }
  column "user_id" {
    type = text
    null = false
  }
  column "expires_at" {
    type = integer
    null = false
  }
  column "revoked_at" {
    type = integer
    null = true
  }
  column "created_at" {
    type = integer
    null = false
  }
  primary_key {
    columns = [column.id]
  }
  foreign_key "sessions_user_id_fk" {
    columns = [column.user_id]
    ref_columns = [table.users.column.id]
  }
  index "sessions_user_id_idx" {
    columns = [column.user_id]
  }
}
table "refresh_tokens" {
  schema = schema.main
  column "hash" {
    type = text
    null = false
  }
  column "session_id" {
    type = text
    null = false
  }
  column "consumed_at" {
    type = integer
    null = true
  }
  column "created_at" {
    type = integer
    null = false
  }
  primary_key {
    columns = [column.hash]
  }
  foreign_key "refresh_tokens_session_id_fk" {
    columns = [column.session_id]
    ref_columns = [table.sessions.column.id]
  }
  index "refresh_tokens_session_id_idx" {
    columns = [column.session_id]
  }
}
table "action_items" {
  schema = schema.main
  column "id" {
    type = text
    null = false
  }
  column "user_id" {
    type = text
    null = false
  }
  column "content" {
    type = text
    null = false
  }
  column "status" {
    type = text
    null = false
  }
  column "note" {
    type = text
    null = true
  }
  column "due_at" {
    type = integer
    null = true
  }
  column "refs" {
    type = text
    null = false
  }
  column "version" {
    type = integer
    null = false
  }
  column "created_at" {
    type = integer
    null = false
  }
  column "updated_at" {
    type = integer
    null = false
  }
  column "source" {
    type = text
    null = true
  }
  column "source_key" {
    type = text
    null = true
  }
  column "tracking" {
    type    = integer
    null    = false
    default = 0
  }
  column "auto_reason" {
    type = text
    null = true
  }
  column "auto_ref" {
    type = text
    null = true
  }
  column "auto_at" {
    type = integer
    null = true
  }
  column "detected_at" {
    type = integer
    null = true
  }
  column "tracked_at" {
    type = integer
    null = true
  }
  column "kind" {
    type    = text
    null    = false
    default = "task"
  }
  column "event_title" {
    type = text
    null = true
  }
  column "event_start_at" {
    type = integer
    null = true
  }
  column "event_end_at" {
    type = integer
    null = true
  }
  primary_key {
    columns = [column.id]
  }
  foreign_key "action_items_user_id_fk" {
    columns = [column.user_id]
    ref_columns = [table.users.column.id]
  }
  index "action_items_user_created_idx" {
    columns = [column.user_id, column.created_at, column.id]
  }
  index "action_items_user_status_created_idx" {
    columns = [column.user_id, column.status, column.created_at, column.id]
  }
  index "action_items_user_source_key_key" {
    unique  = true
    columns = [column.user_id, column.source_key]
  }
  index "action_items_user_tracking_idx" {
    columns = [column.user_id, column.source, column.tracking, column.status, column.tracked_at]
  }
  index "action_items_user_auto_idx" {
    columns = [column.user_id, column.auto_at, column.id]
  }
}
table "api_tokens" {
  schema = schema.main
  column "id" {
    type = text
    null = false
  }
  column "user_id" {
    type = text
    null = false
  }
  column "name" {
    type = text
    null = false
  }
  column "token_hash" {
    type = text
    null = false
  }
  column "created_at" {
    type = integer
    null = false
  }
  column "last_used_at" {
    type = integer
    null = true
  }
  primary_key {
    columns = [column.id]
  }
  foreign_key "api_tokens_user_id_fk" {
    columns = [column.user_id]
    ref_columns = [table.users.column.id]
  }
  index "api_tokens_token_hash_key" {
    unique = true
    columns = [column.token_hash]
  }
  index "api_tokens_user_created_idx" {
    columns = [column.user_id, column.created_at, column.id]
  }
}
table "oauth_clients" {
  schema = schema.main
  column "client_id" {
    type = text
    null = false
  }
  column "kind" {
    type = text
    null = false
  }
  column "name" {
    type = text
    null = false
  }
  column "redirect_uris" {
    type = text
    null = false
  }
  column "auth_method" {
    type = text
    null = false
  }
  column "secret_hash" {
    type = text
    null = true
  }
  column "metadata_expires_at" {
    type = integer
    null = true
  }
  column "created_at" {
    type = integer
    null = false
  }
  column "updated_at" {
    type = integer
    null = false
  }
  primary_key {
    columns = [column.client_id]
  }
}
table "oauth_grants" {
  schema = schema.main
  column "id" {
    type = text
    null = false
  }
  column "user_id" {
    type = text
    null = false
  }
  column "client_id" {
    type = text
    null = false
  }
  column "client_name" {
    type = text
    null = false
  }
  column "scope" {
    type = text
    null = false
  }
  column "resource" {
    type = text
    null = false
  }
  column "redirect_uri" {
    type = text
    null = false
  }
  column "code_hash" {
    type = text
    null = false
  }
  column "code_challenge" {
    type = text
    null = false
  }
  column "code_expires_at" {
    type = integer
    null = false
  }
  column "code_used_at" {
    type = integer
    null = true
  }
  column "created_at" {
    type = integer
    null = false
  }
  column "expires_at" {
    type = integer
    null = false
  }
  column "last_used_at" {
    type = integer
    null = true
  }
  primary_key {
    columns = [column.id]
  }
  foreign_key "oauth_grants_user_id_fk" {
    columns = [column.user_id]
    ref_columns = [table.users.column.id]
  }
  foreign_key "oauth_grants_client_id_fk" {
    columns = [column.client_id]
    ref_columns = [table.oauth_clients.column.client_id]
  }
  index "oauth_grants_code_hash_key" {
    unique = true
    columns = [column.code_hash]
  }
  index "oauth_grants_user_created_idx" {
    columns = [column.user_id, column.created_at, column.id]
  }
}
table "oauth_tokens" {
  schema = schema.main
  column "token_hash" {
    type = text
    null = false
  }
  column "kind" {
    type = text
    null = false
  }
  column "grant_id" {
    type = text
    null = false
  }
  column "user_id" {
    type = text
    null = false
  }
  column "created_at" {
    type = integer
    null = false
  }
  column "expires_at" {
    type = integer
    null = false
  }
  primary_key {
    columns = [column.token_hash]
  }
  foreign_key "oauth_tokens_grant_id_fk" {
    columns = [column.grant_id]
    ref_columns = [table.oauth_grants.column.id]
  }
  foreign_key "oauth_tokens_user_id_fk" {
    columns = [column.user_id]
    ref_columns = [table.users.column.id]
  }
  index "oauth_tokens_grant_idx" {
    columns = [column.grant_id]
  }
  index "oauth_tokens_user_expires_idx" {
    columns = [column.user_id, column.expires_at]
  }
}
table "github_connections" {
  schema = schema.main
  column "user_id" {
    type = text
    null = false
  }
  column "generation" {
    type = integer
    null = false
  }
  column "token_sealed" {
    type = text
    null = false
  }
  column "token_last4" {
    type = text
    null = false
  }
  column "login" {
    type = text
    null = false
  }
  column "scopes" {
    type = text
    null = false
  }
  column "immediate_notify" {
    type    = integer
    null    = false
    default = 1
  }
  column "status" {
    type = text
    null = false
  }
  column "last_error" {
    type = text
    null = true
  }
  column "failure_count" {
    type    = integer
    null    = false
    default = 0
  }
  column "last_modified" {
    type = text
    null = true
  }
  column "initial_import_pending" {
    type = integer
    null = false
  }
  column "next_poll_at" {
    type = integer
    null = false
  }
  column "last_synced_at" {
    type = integer
    null = true
  }
  column "last_reconciled_at" {
    type = integer
    null = true
  }
  column "last_tracked_at" {
    type = integer
    null = true
  }
  column "created_at" {
    type = integer
    null = false
  }
  column "updated_at" {
    type = integer
    null = false
  }
  primary_key {
    columns = [column.user_id]
  }
  foreign_key "github_connections_user_id_fk" {
    columns = [column.user_id]
    ref_columns = [table.users.column.id]
  }
  index "github_connections_status_next_poll_idx" {
    columns = [column.status, column.next_poll_at]
  }
}
table "slack_connections" {
  schema = schema.main
  column "user_id" {
    type = text
    null = false
  }
  column "generation" {
    type = integer
    null = false
  }
  column "token_sealed" {
    type = text
    null = false
  }
  column "token_last4" {
    type = text
    null = false
  }
  column "team_id" {
    type = text
    null = false
  }
  column "team_name" {
    type = text
    null = false
  }
  column "team_url" {
    type = text
    null = false
  }
  column "slack_user_id" {
    type = text
    null = false
  }
  column "slack_user_name" {
    type = text
    null = false
  }
  column "scopes" {
    type = text
    null = false
  }
  column "immediate_notify" {
    type    = integer
    null    = false
    default = 1
  }
  column "detect_mentions" {
    type    = integer
    null    = false
    default = 1
  }
  column "detect_dms" {
    type    = integer
    null    = false
    default = 1
  }
  column "detect_group_dms" {
    type    = integer
    null    = false
    default = 0
  }
  column "status" {
    type = text
    null = false
  }
  column "last_error" {
    type = text
    null = true
  }
  column "failure_count" {
    type    = integer
    null    = false
    default = 0
  }
  column "connected_at" {
    type = integer
    null = false
  }
  column "next_poll_at" {
    type = integer
    null = false
  }
  column "last_synced_at" {
    type = integer
    null = true
  }
  column "last_tracked_at" {
    type = integer
    null = true
  }
  column "created_at" {
    type = integer
    null = false
  }
  column "updated_at" {
    type = integer
    null = false
  }
  primary_key {
    columns = [column.user_id]
  }
  foreign_key "slack_connections_user_id_fk" {
    columns = [column.user_id]
    ref_columns = [table.users.column.id]
  }
  index "slack_connections_status_next_poll_idx" {
    columns = [column.status, column.next_poll_at]
  }
}
table "ai_settings" {
  schema = schema.main
  column "user_id" {
    type = text
    null = false
  }
  column "generation" {
    type = integer
    null = false
  }
  column "provider" {
    type = text
    null = false
  }
  column "model" {
    type = text
    null = false
  }
  column "api_key_sealed" {
    type = text
    null = false
  }
  column "key_last4" {
    type = text
    null = false
  }
  column "status" {
    type = text
    null = false
  }
  column "last_error" {
    type = text
    null = true
  }
  column "activated_at" {
    type = integer
    null = false
  }
  column "created_at" {
    type = integer
    null = false
  }
  column "updated_at" {
    type = integer
    null = false
  }
  primary_key {
    columns = [column.user_id]
  }
  foreign_key "ai_settings_user_id_fk" {
    columns = [column.user_id]
    ref_columns = [table.users.column.id]
  }
}
table "web_push_subscriptions" {
  schema = schema.main
  column "id" {
    type = text
    null = false
  }
  column "user_id" {
    type = text
    null = false
  }
  column "endpoint" {
    type = text
    null = false
  }
  column "p256dh" {
    type = text
    null = false
  }
  column "auth" {
    type = text
    null = false
  }
  column "label" {
    type    = text
    null    = false
    default = ""
  }
  column "created_at" {
    type = integer
    null = false
  }
  column "last_used_at" {
    type = integer
    null = true
  }
  primary_key {
    columns = [column.id]
  }
  foreign_key "web_push_subscriptions_user_id_fk" {
    columns = [column.user_id]
    ref_columns = [table.users.column.id]
  }
  index "web_push_subscriptions_endpoint_key" {
    unique  = true
    columns = [column.endpoint]
  }
  index "web_push_subscriptions_user_created_idx" {
    columns = [column.user_id, column.created_at]
  }
}
table "notification_settings" {
  schema = schema.main
  column "user_id" {
    type = text
    null = false
  }
  column "timezone" {
    type = text
    null = false
  }
  column "digest_hour" {
    type = integer
    null = false
  }
  column "stale_days" {
    type = integer
    null = false
  }
  column "updated_at" {
    type = integer
    null = false
  }
  column "candidate_expire_days" {
    type    = integer
    null    = false
    default = 3
  }
  primary_key {
    columns = [column.user_id]
  }
  foreign_key "notification_settings_user_id_fk" {
    columns = [column.user_id]
    ref_columns = [table.users.column.id]
  }
}
table "notifications" {
  schema = schema.main
  column "id" {
    type = text
    null = false
  }
  column "user_id" {
    type = text
    null = false
  }
  column "kind" {
    type = text
    null = false
  }
  column "action_item_id" {
    type = text
    null = true
  }
  column "dedupe_key" {
    type = text
    null = false
  }
  column "title" {
    type = text
    null = false
  }
  column "body" {
    type = text
    null = false
  }
  column "url" {
    type = text
    null = false
  }
  column "scheduled_at" {
    type = integer
    null = false
  }
  column "sent_at" {
    type = integer
    null = true
  }
  column "attempts" {
    type    = integer
    null    = false
    default = 0
  }
  column "last_error" {
    type = text
    null = true
  }
  column "created_at" {
    type = integer
    null = false
  }
  primary_key {
    columns = [column.id]
  }
  foreign_key "notifications_user_id_fk" {
    columns = [column.user_id]
    ref_columns = [table.users.column.id]
  }
  foreign_key "notifications_action_item_id_fk" {
    columns = [column.action_item_id]
    ref_columns = [table.action_items.column.id]
  }
  index "notifications_user_dedupe_key" {
    unique  = true
    columns = [column.user_id, column.dedupe_key]
  }
  index "notifications_sent_scheduled_idx" {
    columns = [column.sent_at, column.scheduled_at]
  }
  index "notifications_action_item_idx" {
    columns = [column.action_item_id]
  }
  index "notifications_user_kind_created_idx" {
    columns = [column.user_id, column.kind, column.created_at]
  }
}
table "instance_secrets" {
  schema = schema.main
  column "name" {
    type = text
    null = false
  }
  column "value" {
    type = text
    null = false
  }
  column "created_at" {
    type = integer
    null = false
  }
  column "updated_at" {
    type = integer
    null = false
  }
  primary_key {
    columns = [column.name]
  }
}
