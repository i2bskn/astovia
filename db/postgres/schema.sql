-- Create "users" table
CREATE TABLE "users" (
  "id" text NOT NULL,
  "email" text NOT NULL,
  "password_hash" text NOT NULL,
  "disabled" bigint NOT NULL,
  "created_at" bigint NOT NULL,
  PRIMARY KEY ("id")
);
-- Create index "users_email_key" to table: "users"
CREATE UNIQUE INDEX "users_email_key" ON "users" ("email");
-- Create "action_items" table
CREATE TABLE "action_items" (
  "id" text NOT NULL,
  "user_id" text NOT NULL,
  "content" text NOT NULL,
  "status" text NOT NULL,
  "note" text NULL,
  "due_at" bigint NULL,
  "refs" text NOT NULL,
  "version" bigint NOT NULL,
  "created_at" bigint NOT NULL,
  "updated_at" bigint NOT NULL,
  PRIMARY KEY ("id"),
  CONSTRAINT "action_items_user_id_fk" FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION
);
-- Create index "action_items_user_created_idx" to table: "action_items"
CREATE INDEX "action_items_user_created_idx" ON "action_items" ("user_id", "created_at", "id");
-- Create index "action_items_user_status_created_idx" to table: "action_items"
CREATE INDEX "action_items_user_status_created_idx" ON "action_items" ("user_id", "status", "created_at", "id");
-- Create "api_tokens" table
CREATE TABLE "api_tokens" (
  "id" text NOT NULL,
  "user_id" text NOT NULL,
  "name" text NOT NULL,
  "token_hash" text NOT NULL,
  "created_at" bigint NOT NULL,
  "last_used_at" bigint NULL,
  PRIMARY KEY ("id"),
  CONSTRAINT "api_tokens_user_id_fk" FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION
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
  "metadata_expires_at" bigint NULL,
  "created_at" bigint NOT NULL,
  "updated_at" bigint NOT NULL,
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
  "code_expires_at" bigint NOT NULL,
  "code_used_at" bigint NULL,
  "created_at" bigint NOT NULL,
  "expires_at" bigint NOT NULL,
  "last_used_at" bigint NULL,
  PRIMARY KEY ("id"),
  CONSTRAINT "oauth_grants_client_id_fk" FOREIGN KEY ("client_id") REFERENCES "oauth_clients" ("client_id") ON UPDATE NO ACTION ON DELETE NO ACTION,
  CONSTRAINT "oauth_grants_user_id_fk" FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION
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
  "created_at" bigint NOT NULL,
  "expires_at" bigint NOT NULL,
  PRIMARY KEY ("token_hash"),
  CONSTRAINT "oauth_tokens_grant_id_fk" FOREIGN KEY ("grant_id") REFERENCES "oauth_grants" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION,
  CONSTRAINT "oauth_tokens_user_id_fk" FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION
);
-- Create index "oauth_tokens_grant_idx" to table: "oauth_tokens"
CREATE INDEX "oauth_tokens_grant_idx" ON "oauth_tokens" ("grant_id");
-- Create index "oauth_tokens_user_expires_idx" to table: "oauth_tokens"
CREATE INDEX "oauth_tokens_user_expires_idx" ON "oauth_tokens" ("user_id", "expires_at");
-- Create "sessions" table
CREATE TABLE "sessions" (
  "id" text NOT NULL,
  "user_id" text NOT NULL,
  "expires_at" bigint NOT NULL,
  "revoked_at" bigint NULL,
  "created_at" bigint NOT NULL,
  PRIMARY KEY ("id"),
  CONSTRAINT "sessions_user_id_fk" FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION
);
-- Create index "sessions_user_id_idx" to table: "sessions"
CREATE INDEX "sessions_user_id_idx" ON "sessions" ("user_id");
-- Create "refresh_tokens" table
CREATE TABLE "refresh_tokens" (
  "hash" text NOT NULL,
  "session_id" text NOT NULL,
  "consumed_at" bigint NULL,
  "created_at" bigint NOT NULL,
  PRIMARY KEY ("hash"),
  CONSTRAINT "refresh_tokens_session_id_fk" FOREIGN KEY ("session_id") REFERENCES "sessions" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION
);
-- Create index "refresh_tokens_session_id_idx" to table: "refresh_tokens"
CREATE INDEX "refresh_tokens_session_id_idx" ON "refresh_tokens" ("session_id");
