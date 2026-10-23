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
