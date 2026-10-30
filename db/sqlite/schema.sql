-- Create "users" table
CREATE TABLE "users" (
  "id" text NOT NULL,
  "email" text NOT NULL,
  "password_hash" text NOT NULL,
  "disabled" integer NOT NULL,
  "created_at" integer NOT NULL,
  "role" text NOT NULL DEFAULT 'owner',
  PRIMARY KEY ("id")
);
-- Create index "users_email_key" to table: "users"
CREATE UNIQUE INDEX "users_email_key" ON "users" ("email");
-- Create "sessions" table
CREATE TABLE "sessions" (
  "id" text NOT NULL,
  "user_id" text NOT NULL,
  "expires_at" integer NOT NULL,
  "revoked_at" integer NULL,
  "created_at" integer NOT NULL,
  PRIMARY KEY ("id"),
  CONSTRAINT "sessions_user_id_fk" FOREIGN KEY ("user_id") REFERENCES "users" ("id")
);
-- Create index "sessions_user_id_idx" to table: "sessions"
CREATE INDEX "sessions_user_id_idx" ON "sessions" ("user_id");
-- Create "refresh_tokens" table
CREATE TABLE "refresh_tokens" (
  "hash" text NOT NULL,
  "session_id" text NOT NULL,
  "consumed_at" integer NULL,
  "created_at" integer NOT NULL,
  PRIMARY KEY ("hash"),
  CONSTRAINT "refresh_tokens_session_id_fk" FOREIGN KEY ("session_id") REFERENCES "sessions" ("id")
);
-- Create index "refresh_tokens_session_id_idx" to table: "refresh_tokens"
CREATE INDEX "refresh_tokens_session_id_idx" ON "refresh_tokens" ("session_id");
-- Create "action_items" table
CREATE TABLE "action_items" (
  "id" text NOT NULL,
  "user_id" text NOT NULL,
  "content" text NOT NULL,
  "status" text NOT NULL,
  "note" text NULL,
  "due_at" integer NULL,
  "refs" text NOT NULL,
  "version" integer NOT NULL,
  "created_at" integer NOT NULL,
  "updated_at" integer NOT NULL,
  "source" text NULL,
  "source_key" text NULL,
  "tracking" integer NOT NULL DEFAULT 0,
  "auto_reason" text NULL,
  "auto_ref" text NULL,
  "auto_at" integer NULL,
  "detected_at" integer NULL,
  "tracked_at" integer NULL,
  "kind" text NOT NULL DEFAULT 'task',
  "event_title" text NULL,
  "event_start_at" integer NULL,
  "event_end_at" integer NULL,
  "parent_id" text NULL,
  "importance" text NOT NULL DEFAULT 'normal',
  PRIMARY KEY ("id"),
  CONSTRAINT "action_items_user_id_fk" FOREIGN KEY ("user_id") REFERENCES "users" ("id")
);
-- Create index "action_items_user_created_idx" to table: "action_items"
CREATE INDEX "action_items_user_created_idx" ON "action_items" ("user_id", "created_at", "id");
-- Create index "action_items_user_status_created_idx" to table: "action_items"
CREATE INDEX "action_items_user_status_created_idx" ON "action_items" ("user_id", "status", "created_at", "id");
-- Create index "action_items_user_source_key_key" to table: "action_items"
CREATE UNIQUE INDEX "action_items_user_source_key_key" ON "action_items" ("user_id", "source_key");
-- Create index "action_items_user_tracking_idx" to table: "action_items"
CREATE INDEX "action_items_user_tracking_idx" ON "action_items" ("user_id", "source", "tracking", "status", "tracked_at");
-- Create index "action_items_user_auto_idx" to table: "action_items"
CREATE INDEX "action_items_user_auto_idx" ON "action_items" ("user_id", "auto_at", "id");
-- Create index "action_items_user_parent_idx" to table: "action_items"
CREATE INDEX "action_items_user_parent_idx" ON "action_items" ("user_id", "parent_id");
-- Create "api_tokens" table
CREATE TABLE "api_tokens" (
  "id" text NOT NULL,
  "user_id" text NOT NULL,
  "name" text NOT NULL,
  "token_hash" text NOT NULL,
  "created_at" integer NOT NULL,
  "last_used_at" integer NULL,
  PRIMARY KEY ("id"),
  CONSTRAINT "api_tokens_user_id_fk" FOREIGN KEY ("user_id") REFERENCES "users" ("id")
);
-- Create index "api_tokens_token_hash_key" to table: "api_tokens"
CREATE UNIQUE INDEX "api_tokens_token_hash_key" ON "api_tokens" ("token_hash");
-- Create index "api_tokens_user_created_idx" to table: "api_tokens"
CREATE INDEX "api_tokens_user_created_idx" ON "api_tokens" ("user_id", "created_at", "id");
-- Create "oauth_clients" table
CREATE TABLE "oauth_clients" (
  "client_id" text NOT NULL,
  "kind" text NOT NULL,
  "name" text NOT NULL,
  "redirect_uris" text NOT NULL,
  "auth_method" text NOT NULL,
  "secret_hash" text NULL,
  "metadata_expires_at" integer NULL,
  "created_at" integer NOT NULL,
  "updated_at" integer NOT NULL,
  PRIMARY KEY ("client_id")
);
-- Create "oauth_grants" table
CREATE TABLE "oauth_grants" (
  "id" text NOT NULL,
  "user_id" text NOT NULL,
  "client_id" text NOT NULL,
  "client_name" text NOT NULL,
  "scope" text NOT NULL,
  "resource" text NOT NULL,
  "redirect_uri" text NOT NULL,
  "code_hash" text NOT NULL,
  "code_challenge" text NOT NULL,
  "code_expires_at" integer NOT NULL,
  "code_used_at" integer NULL,
  "created_at" integer NOT NULL,
  "expires_at" integer NOT NULL,
  "last_used_at" integer NULL,
  PRIMARY KEY ("id"),
  CONSTRAINT "oauth_grants_user_id_fk" FOREIGN KEY ("user_id") REFERENCES "users" ("id"),
  CONSTRAINT "oauth_grants_client_id_fk" FOREIGN KEY ("client_id") REFERENCES "oauth_clients" ("client_id")
);
-- Create index "oauth_grants_code_hash_key" to table: "oauth_grants"
CREATE UNIQUE INDEX "oauth_grants_code_hash_key" ON "oauth_grants" ("code_hash");
-- Create index "oauth_grants_user_created_idx" to table: "oauth_grants"
CREATE INDEX "oauth_grants_user_created_idx" ON "oauth_grants" ("user_id", "created_at", "id");
-- Create "oauth_tokens" table
CREATE TABLE "oauth_tokens" (
  "token_hash" text NOT NULL,
  "kind" text NOT NULL,
  "grant_id" text NOT NULL,
  "user_id" text NOT NULL,
  "created_at" integer NOT NULL,
  "expires_at" integer NOT NULL,
  PRIMARY KEY ("token_hash"),
  CONSTRAINT "oauth_tokens_grant_id_fk" FOREIGN KEY ("grant_id") REFERENCES "oauth_grants" ("id"),
  CONSTRAINT "oauth_tokens_user_id_fk" FOREIGN KEY ("user_id") REFERENCES "users" ("id")
);
-- Create index "oauth_tokens_grant_idx" to table: "oauth_tokens"
CREATE INDEX "oauth_tokens_grant_idx" ON "oauth_tokens" ("grant_id");
-- Create index "oauth_tokens_user_expires_idx" to table: "oauth_tokens"
CREATE INDEX "oauth_tokens_user_expires_idx" ON "oauth_tokens" ("user_id", "expires_at");
-- Create "github_connections" table
CREATE TABLE "github_connections" (
  "user_id" text NOT NULL,
  "generation" integer NOT NULL,
  "token_sealed" text NOT NULL,
  "token_last4" text NOT NULL,
  "login" text NOT NULL,
  "scopes" text NOT NULL,
  "immediate_notify" integer NOT NULL DEFAULT 1,
  "status" text NOT NULL,
  "last_error" text NULL,
  "failure_count" integer NOT NULL DEFAULT 0,
  "last_modified" text NULL,
  "initial_import_pending" integer NOT NULL,
  "next_poll_at" integer NOT NULL,
  "last_synced_at" integer NULL,
  "last_reconciled_at" integer NULL,
  "last_tracked_at" integer NULL,
  "created_at" integer NOT NULL,
  "updated_at" integer NOT NULL,
  PRIMARY KEY ("user_id"),
  CONSTRAINT "github_connections_user_id_fk" FOREIGN KEY ("user_id") REFERENCES "users" ("id")
);
-- Create index "github_connections_status_next_poll_idx" to table: "github_connections"
CREATE INDEX "github_connections_status_next_poll_idx" ON "github_connections" ("status", "next_poll_at");
-- Create "slack_connections" table
CREATE TABLE "slack_connections" (
  "user_id" text NOT NULL,
  "generation" integer NOT NULL,
  "token_sealed" text NOT NULL,
  "token_last4" text NOT NULL,
  "team_id" text NOT NULL,
  "team_name" text NOT NULL,
  "team_url" text NOT NULL,
  "slack_user_id" text NOT NULL,
  "slack_user_name" text NOT NULL,
  "scopes" text NOT NULL,
  "immediate_notify" integer NOT NULL DEFAULT 1,
  "detect_mentions" integer NOT NULL DEFAULT 1,
  "detect_dms" integer NOT NULL DEFAULT 1,
  "detect_group_dms" integer NOT NULL DEFAULT 0,
  "status" text NOT NULL,
  "last_error" text NULL,
  "failure_count" integer NOT NULL DEFAULT 0,
  "connected_at" integer NOT NULL,
  "next_poll_at" integer NOT NULL,
  "last_synced_at" integer NULL,
  "last_tracked_at" integer NULL,
  "created_at" integer NOT NULL,
  "updated_at" integer NOT NULL,
  PRIMARY KEY ("user_id"),
  CONSTRAINT "slack_connections_user_id_fk" FOREIGN KEY ("user_id") REFERENCES "users" ("id")
);
-- Create index "slack_connections_status_next_poll_idx" to table: "slack_connections"
CREATE INDEX "slack_connections_status_next_poll_idx" ON "slack_connections" ("status", "next_poll_at");
-- Create "chatwork_connections" table
CREATE TABLE "chatwork_connections" (
  "user_id" text NOT NULL,
  "generation" integer NOT NULL,
  "token_sealed" text NOT NULL,
  "token_last4" text NOT NULL,
  "account_id" text NOT NULL,
  "account_name" text NOT NULL,
  "organization_name" text NOT NULL,
  "immediate_notify" integer NOT NULL DEFAULT 1,
  "detect_mentions" integer NOT NULL DEFAULT 1,
  "detect_dms" integer NOT NULL DEFAULT 1,
  "import_tasks" integer NOT NULL DEFAULT 1,
  "status" text NOT NULL,
  "last_error" text NULL,
  "failure_count" integer NOT NULL DEFAULT 0,
  "connected_at" integer NOT NULL,
  "next_poll_at" integer NOT NULL,
  "last_synced_at" integer NULL,
  "last_tracked_at" integer NULL,
  "last_tasks_at" integer NULL,
  "created_at" integer NOT NULL,
  "updated_at" integer NOT NULL,
  PRIMARY KEY ("user_id"),
  CONSTRAINT "chatwork_connections_user_id_fk" FOREIGN KEY ("user_id") REFERENCES "users" ("id")
);
-- Create index "chatwork_connections_status_next_poll_idx" to table: "chatwork_connections"
CREATE INDEX "chatwork_connections_status_next_poll_idx" ON "chatwork_connections" ("status", "next_poll_at");
-- Create "gmail_accounts" table
CREATE TABLE "gmail_accounts" (
  "id" text NOT NULL,
  "user_id" text NOT NULL,
  "email" text NOT NULL,
  "generation" integer NOT NULL,
  "token_sealed" text NOT NULL,
  "immediate_notify" integer NOT NULL DEFAULT 1,
  "status" text NOT NULL,
  "last_error" text NULL,
  "failure_count" integer NOT NULL DEFAULT 0,
  "connected_at" integer NOT NULL,
  "next_poll_at" integer NOT NULL,
  "last_synced_at" integer NULL,
  "last_tracked_at" integer NULL,
  "created_at" integer NOT NULL,
  "updated_at" integer NOT NULL,
  PRIMARY KEY ("id"),
  CONSTRAINT "gmail_accounts_user_id_fk" FOREIGN KEY ("user_id") REFERENCES "users" ("id")
);
-- Create index "gmail_accounts_email_idx" to table: "gmail_accounts"
CREATE UNIQUE INDEX "gmail_accounts_email_idx" ON "gmail_accounts" ("email");
-- Create index "gmail_accounts_user_idx" to table: "gmail_accounts"
CREATE INDEX "gmail_accounts_user_idx" ON "gmail_accounts" ("user_id", "email");
-- Create index "gmail_accounts_status_next_poll_idx" to table: "gmail_accounts"
CREATE INDEX "gmail_accounts_status_next_poll_idx" ON "gmail_accounts" ("status", "next_poll_at");
-- Create "google_auth_states" table
CREATE TABLE "google_auth_states" (
  "state_hash" text NOT NULL,
  "user_id" text NOT NULL,
  "verifier_sealed" text NOT NULL,
  "expires_at" integer NOT NULL,
  "created_at" integer NOT NULL,
  PRIMARY KEY ("state_hash"),
  CONSTRAINT "google_auth_states_user_id_fk" FOREIGN KEY ("user_id") REFERENCES "users" ("id")
);
-- Create index "google_auth_states_user_idx" to table: "google_auth_states"
CREATE INDEX "google_auth_states_user_idx" ON "google_auth_states" ("user_id");
-- Create "ai_settings" table
CREATE TABLE "ai_settings" (
  "user_id" text NOT NULL,
  "generation" integer NOT NULL,
  "provider" text NOT NULL,
  "model" text NOT NULL,
  "api_key_sealed" text NOT NULL,
  "key_last4" text NOT NULL,
  "status" text NOT NULL,
  "last_error" text NULL,
  "activated_at" integer NOT NULL,
  "created_at" integer NOT NULL,
  "updated_at" integer NOT NULL,
  PRIMARY KEY ("user_id"),
  CONSTRAINT "ai_settings_user_id_fk" FOREIGN KEY ("user_id") REFERENCES "users" ("id")
);
-- Create "web_push_subscriptions" table
CREATE TABLE "web_push_subscriptions" (
  "id" text NOT NULL,
  "user_id" text NOT NULL,
  "endpoint" text NOT NULL,
  "p256dh" text NOT NULL,
  "auth" text NOT NULL,
  "label" text NOT NULL DEFAULT '',
  "created_at" integer NOT NULL,
  "last_used_at" integer NULL,
  PRIMARY KEY ("id"),
  CONSTRAINT "web_push_subscriptions_user_id_fk" FOREIGN KEY ("user_id") REFERENCES "users" ("id")
);
-- Create index "web_push_subscriptions_endpoint_key" to table: "web_push_subscriptions"
CREATE UNIQUE INDEX "web_push_subscriptions_endpoint_key" ON "web_push_subscriptions" ("endpoint");
-- Create index "web_push_subscriptions_user_created_idx" to table: "web_push_subscriptions"
CREATE INDEX "web_push_subscriptions_user_created_idx" ON "web_push_subscriptions" ("user_id", "created_at");
-- Create "notification_settings" table
CREATE TABLE "notification_settings" (
  "user_id" text NOT NULL,
  "timezone" text NOT NULL,
  "digest_hour" integer NOT NULL,
  "stale_days" integer NOT NULL,
  "updated_at" integer NOT NULL,
  "candidate_expire_days" integer NOT NULL DEFAULT 3,
  PRIMARY KEY ("user_id"),
  CONSTRAINT "notification_settings_user_id_fk" FOREIGN KEY ("user_id") REFERENCES "users" ("id")
);
-- Create "notifications" table
CREATE TABLE "notifications" (
  "id" text NOT NULL,
  "user_id" text NOT NULL,
  "kind" text NOT NULL,
  "action_item_id" text NULL,
  "dedupe_key" text NOT NULL,
  "title" text NOT NULL,
  "body" text NOT NULL,
  "url" text NOT NULL,
  "scheduled_at" integer NOT NULL,
  "sent_at" integer NULL,
  "attempts" integer NOT NULL DEFAULT 0,
  "last_error" text NULL,
  "created_at" integer NOT NULL,
  PRIMARY KEY ("id"),
  CONSTRAINT "notifications_user_id_fk" FOREIGN KEY ("user_id") REFERENCES "users" ("id"),
  CONSTRAINT "notifications_action_item_id_fk" FOREIGN KEY ("action_item_id") REFERENCES "action_items" ("id")
);
-- Create index "notifications_user_dedupe_key" to table: "notifications"
CREATE UNIQUE INDEX "notifications_user_dedupe_key" ON "notifications" ("user_id", "dedupe_key");
-- Create index "notifications_sent_scheduled_idx" to table: "notifications"
CREATE INDEX "notifications_sent_scheduled_idx" ON "notifications" ("sent_at", "scheduled_at");
-- Create index "notifications_action_item_idx" to table: "notifications"
CREATE INDEX "notifications_action_item_idx" ON "notifications" ("action_item_id");
-- Create index "notifications_user_kind_created_idx" to table: "notifications"
CREATE INDEX "notifications_user_kind_created_idx" ON "notifications" ("user_id", "kind", "created_at");
-- Create "instance_secrets" table
CREATE TABLE "instance_secrets" (
  "name" text NOT NULL,
  "value" text NOT NULL,
  "created_at" integer NOT NULL,
  "updated_at" integer NOT NULL,
  PRIMARY KEY ("name")
);
-- Create "invitations" table
CREATE TABLE "invitations" (
  "id" text NOT NULL,
  "token_hash" text NOT NULL,
  "email" text NOT NULL,
  "role" text NOT NULL,
  "invited_by" text NOT NULL,
  "created_at" integer NOT NULL,
  "expires_at" integer NOT NULL,
  "accepted_at" integer NULL,
  "accepted_user_id" text NULL,
  "revoked_at" integer NULL,
  PRIMARY KEY ("id"),
  CONSTRAINT "invitations_invited_by_fk" FOREIGN KEY ("invited_by") REFERENCES "users" ("id"),
  CONSTRAINT "invitations_accepted_user_id_fk" FOREIGN KEY ("accepted_user_id") REFERENCES "users" ("id")
);
-- Create index "invitations_token_hash_key" to table: "invitations"
CREATE UNIQUE INDEX "invitations_token_hash_key" ON "invitations" ("token_hash");
-- Create index "invitations_email_idx" to table: "invitations"
CREATE INDEX "invitations_email_idx" ON "invitations" ("email");
-- Create index "invitations_created_at_idx" to table: "invitations"
CREATE INDEX "invitations_created_at_idx" ON "invitations" ("created_at");
-- Create "password_resets" table
CREATE TABLE "password_resets" (
  "id" text NOT NULL,
  "token_hash" text NOT NULL,
  "user_id" text NOT NULL,
  "created_by" text NOT NULL,
  "created_at" integer NOT NULL,
  "expires_at" integer NOT NULL,
  "used_at" integer NULL,
  "revoked_at" integer NULL,
  PRIMARY KEY ("id"),
  CONSTRAINT "password_resets_user_id_fk" FOREIGN KEY ("user_id") REFERENCES "users" ("id"),
  CONSTRAINT "password_resets_created_by_fk" FOREIGN KEY ("created_by") REFERENCES "users" ("id")
);
-- Create index "password_resets_token_hash_key" to table: "password_resets"
CREATE UNIQUE INDEX "password_resets_token_hash_key" ON "password_resets" ("token_hash");
-- Create index "password_resets_user_id_idx" to table: "password_resets"
CREATE INDEX "password_resets_user_id_idx" ON "password_resets" ("user_id");
