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
  column "role" {
    type    = text
    null    = false
    default = "owner"
  }
  column "oidc_issuer" {
    type = text
    null = true
  }
  column "oidc_subject" {
    type = text
    null = true
  }
  column "oidc_link_approved_until" {
    type = integer
    null = true
  }
  primary_key {
    columns = [column.id]
  }
  index "users_email_key" {
    unique = true
    columns = [column.email]
  }
  index "users_oidc_identity_key" {
    unique = true
    columns = [column.oidc_issuer, column.oidc_subject]
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
  column "parent_id" {
    type = text
    null = true
  }
  column "importance" {
    type    = text
    null    = false
    default = "normal"
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
  index "action_items_user_parent_idx" {
    columns = [column.user_id, column.parent_id]
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
table "chatwork_connections" {
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
  column "account_id" {
    type = text
    null = false
  }
  column "account_name" {
    type = text
    null = false
  }
  column "organization_name" {
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
  column "import_tasks" {
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
  column "last_tasks_at" {
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
  foreign_key "chatwork_connections_user_id_fk" {
    columns = [column.user_id]
    ref_columns = [table.users.column.id]
  }
  index "chatwork_connections_status_next_poll_idx" {
    columns = [column.status, column.next_poll_at]
  }
}
table "gmail_accounts" {
  schema = schema.main
  column "id" {
    type = text
    null = false
  }
  column "user_id" {
    type = text
    null = false
  }
  column "email" {
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
    columns = [column.id]
  }
  foreign_key "gmail_accounts_user_id_fk" {
    columns = [column.user_id]
    ref_columns = [table.users.column.id]
  }
  index "gmail_accounts_email_idx" {
    unique  = true
    columns = [column.email]
  }
  index "gmail_accounts_user_idx" {
    columns = [column.user_id, column.email]
  }
  index "gmail_accounts_status_next_poll_idx" {
    columns = [column.status, column.next_poll_at]
  }
}
table "google_auth_states" {
  schema = schema.main
  column "state_hash" {
    type = text
    null = false
  }
  column "user_id" {
    type = text
    null = false
  }
  column "verifier_sealed" {
    type = text
    null = false
  }
  column "expires_at" {
    type = integer
    null = false
  }
  column "created_at" {
    type = integer
    null = false
  }
  primary_key {
    columns = [column.state_hash]
  }
  foreign_key "google_auth_states_user_id_fk" {
    columns = [column.user_id]
    ref_columns = [table.users.column.id]
  }
  index "google_auth_states_user_idx" {
    columns = [column.user_id]
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
table "ai_instructions" {
  schema = schema.main
  column "user_id" {
    type = text
    null = false
  }
  column "scope" {
    type = text
    null = false
  }
  column "body" {
    type = text
    null = false
  }
  column "updated_at" {
    type = integer
    null = false
  }
  primary_key {
    columns = [column.user_id, column.scope]
  }
  foreign_key "ai_instructions_user_id_fk" {
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
  column "channel" {
    type    = text
    null    = false
    default = "webpush"
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
    columns = [column.user_id, column.dedupe_key, column.channel]
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
table "notification_channels" {
  schema = schema.main
  column "user_id" {
    type = text
    null = false
  }
  column "webpush_kinds" {
    type    = text
    null    = false
    default = "detected,stale,due_soon,overdue,digest"
  }
  column "slack_kinds" {
    type    = text
    null    = false
    default = ""
  }
  column "email_kinds" {
    type    = text
    null    = false
    default = ""
  }
  column "slack_webhook_sealed" {
    type = text
    null = true
  }
  column "slack_last_error" {
    type = text
    null = true
  }
  column "updated_at" {
    type = integer
    null = false
  }
  primary_key {
    columns = [column.user_id]
  }
  foreign_key "notification_channels_user_id_fk" {
    columns = [column.user_id]
    ref_columns = [table.users.column.id]
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
table "invitations" {
  schema = schema.main
  column "id" {
    type = text
    null = false
  }
  column "token_hash" {
    type = text
    null = false
  }
  column "email" {
    type = text
    null = false
  }
  column "role" {
    type = text
    null = false
  }
  column "invited_by" {
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
  column "accepted_at" {
    type = integer
    null = true
  }
  column "accepted_user_id" {
    type = text
    null = true
  }
  column "revoked_at" {
    type = integer
    null = true
  }
  primary_key {
    columns = [column.id]
  }
  foreign_key "invitations_invited_by_fk" {
    columns = [column.invited_by]
    ref_columns = [table.users.column.id]
  }
  foreign_key "invitations_accepted_user_id_fk" {
    columns = [column.accepted_user_id]
    ref_columns = [table.users.column.id]
  }
  index "invitations_token_hash_key" {
    unique  = true
    columns = [column.token_hash]
  }
  index "invitations_email_idx" {
    columns = [column.email]
  }
  index "invitations_created_at_idx" {
    columns = [column.created_at]
  }
}
table "password_resets" {
  schema = schema.main
  column "id" {
    type = text
    null = false
  }
  column "token_hash" {
    type = text
    null = false
  }
  column "user_id" {
    type = text
    null = false
  }
  column "created_by" {
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
  column "used_at" {
    type = integer
    null = true
  }
  column "revoked_at" {
    type = integer
    null = true
  }
  primary_key {
    columns = [column.id]
  }
  foreign_key "password_resets_user_id_fk" {
    columns = [column.user_id]
    ref_columns = [table.users.column.id]
  }
  foreign_key "password_resets_created_by_fk" {
    columns = [column.created_by]
    ref_columns = [table.users.column.id]
  }
  index "password_resets_token_hash_key" {
    unique  = true
    columns = [column.token_hash]
  }
  index "password_resets_user_id_idx" {
    columns = [column.user_id]
  }
}
table "mfa_totp" {
  schema = schema.main
  column "id" {
    type    = text
    null    = false
    default = ""
  }
  column "user_id" {
    type = text
    null = false
  }
  column "secret_sealed" {
    type = text
    null = false
  }
  column "last_step" {
    type = integer
    null = false
  }
  column "created_at" {
    type = integer
    null = false
  }
  primary_key {
    columns = [column.user_id]
  }
  foreign_key "mfa_totp_user_id_fk" {
    columns = [column.user_id]
    ref_columns = [table.users.column.id]
  }
}
table "mfa_webauthn_credentials" {
  schema = schema.main
  column "id" {
    type = text
    null = false
  }
  column "user_id" {
    type = text
    null = false
  }
  column "credential_id" {
    type = text
    null = false
  }
  column "credential" {
    type = text
    null = false
  }
  column "name" {
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
  column "version" {
    type    = text
    null    = false
    default = ""
  }
  column "passkey" {
    type    = integer
    null    = false
    default = 0
  }
  primary_key {
    columns = [column.id]
  }
  foreign_key "mfa_webauthn_credentials_user_id_fk" {
    columns = [column.user_id]
    ref_columns = [table.users.column.id]
  }
  index "mfa_webauthn_credentials_credential_id_key" {
    unique  = true
    columns = [column.credential_id]
  }
  index "mfa_webauthn_credentials_user_id_idx" {
    columns = [column.user_id]
  }
}
table "mfa_recovery_codes" {
  schema = schema.main
  column "id" {
    type = text
    null = false
  }
  column "user_id" {
    type = text
    null = false
  }
  column "code_hash" {
    type = text
    null = false
  }
  column "created_at" {
    type = integer
    null = false
  }
  column "used_at" {
    type = integer
    null = true
  }
  primary_key {
    columns = [column.id]
  }
  foreign_key "mfa_recovery_codes_user_id_fk" {
    columns = [column.user_id]
    ref_columns = [table.users.column.id]
  }
  index "mfa_recovery_codes_code_hash_key" {
    unique  = true
    columns = [column.code_hash]
  }
  index "mfa_recovery_codes_user_id_idx" {
    columns = [column.user_id]
  }
}
table "web_sessions" {
  schema = schema.main
  column "id" {
    type = text
    null = false
  }
  column "user_id" {
    type = text
    null = false
  }
  column "authenticated_at" {
    type = integer
    null = false
  }
  column "last_activity" {
    type = integer
    null = false
  }
  column "absolute_expiry" {
    type = integer
    null = false
  }
  column "idle_seconds" {
    type = integer
    null = false
  }
  primary_key {
    columns = [column.id]
  }
  foreign_key "web_sessions_user_id_fk" {
    columns = [column.user_id]
    ref_columns = [table.users.column.id]
  }
  index "web_sessions_user_id_idx" {
    columns = [column.user_id]
  }
  index "web_sessions_absolute_expiry_idx" {
    columns = [column.absolute_expiry]
  }
}
table "oidc_logins" {
  schema = schema.main
  column "key" {
    type = text
    null = false
  }
  column "nonce" {
    type = text
    null = false
  }
  column "verifier" {
    type = text
    null = false
  }
  column "started_at" {
    type = integer
    null = false
  }
  column "expires_at" {
    type = integer
    null = false
  }
  column "max_age_seconds" {
    type = integer
    null = true
  }
  column "app_data" {
    type = text
    null = true
  }
  primary_key {
    columns = [column.key]
  }
  index "oidc_logins_expires_at_idx" {
    columns = [column.expires_at]
  }
}
table "webauthn_ceremonies" {
  schema = schema.main
  column "key" {
    type = text
    null = false
  }
  column "subject" {
    type = text
    null = false
  }
  column "record" {
    type = text
    null = false
  }
  column "expires_at" {
    type = integer
    null = false
  }
  primary_key {
    columns = [column.key]
  }
  index "webauthn_ceremonies_subject_idx" {
    columns = [column.subject]
  }
  index "webauthn_ceremonies_expires_at_idx" {
    columns = [column.expires_at]
  }
}
table "mcp_oauth_codes" {
  schema = schema.main
  column "key" {
    type = text
    null = false
  }
  column "client_id" {
    type = text
    null = false
  }
  column "redirect_uri" {
    type = text
    null = false
  }
  column "subject" {
    type = text
    null = false
  }
  column "scopes" {
    type = text
    null = false
  }
  column "challenge" {
    type = text
    null = false
  }
  column "expires_at" {
    type = integer
    null = false
  }
  column "consumed" {
    type = integer
    null = false
  }
  column "reused" {
    type = integer
    null = false
  }
  primary_key {
    columns = [column.key]
  }
  index "mcp_oauth_codes_subject_idx" {
    columns = [column.subject]
  }
  index "mcp_oauth_codes_client_id_idx" {
    columns = [column.client_id]
  }
  index "mcp_oauth_codes_expires_at_idx" {
    columns = [column.expires_at]
  }
}
table "mcp_oauth_connections" {
  schema = schema.main
  column "id" {
    type = text
    null = false
  }
  column "selector" {
    type = text
    null = false
  }
  column "subject" {
    type = text
    null = false
  }
  column "client_id" {
    type = text
    null = false
  }
  column "scopes" {
    type = text
    null = false
  }
  column "created_at" {
    type = integer
    null = false
  }
  column "refreshed_at" {
    type = integer
    null = false
  }
  column "absolute_expiry" {
    type = integer
    null = true
  }
  column "access" {
    type = text
    null = false
  }
  column "access_expiry" {
    type = integer
    null = false
  }
  column "refresh" {
    type = text
    null = false
  }
  column "refresh_expiry" {
    type = integer
    null = false
  }
  column "code" {
    type = text
    null = true
  }
  column "last_used_at" {
    type = integer
    null = true
  }
  primary_key {
    columns = [column.id]
  }
  index "mcp_oauth_connections_selector_key" {
    unique  = true
    columns = [column.selector]
  }
  index "mcp_oauth_connections_access_key" {
    unique  = true
    columns = [column.access]
  }
  index "mcp_oauth_connections_code_key" {
    unique  = true
    columns = [column.code]
  }
  index "mcp_oauth_connections_subject_idx" {
    columns = [column.subject]
  }
  index "mcp_oauth_connections_client_id_idx" {
    columns = [column.client_id]
  }
  index "mcp_oauth_connections_refresh_expiry_idx" {
    columns = [column.refresh_expiry]
  }
}
table "mcp_oauth_registrations" {
  schema = schema.main
  column "id" {
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
  column "created_at" {
    type = integer
    null = false
  }
  column "expires_at" {
    type = integer
    null = true
  }
  primary_key {
    columns = [column.id]
  }
  index "mcp_oauth_registrations_expires_at_idx" {
    columns = [column.expires_at]
  }
}
table "mcp_oauth_client_labels" {
  schema = schema.main
  column "client_id" {
    type = text
    null = false
  }
  column "name" {
    type = text
    null = false
  }
  column "host" {
    type = text
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
