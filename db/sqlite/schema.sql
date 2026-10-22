-- Create "users" table
CREATE TABLE "users" (
  "id" text NOT NULL,
  "username" text NOT NULL,
  "password_hash" text NOT NULL,
  "disabled" integer NOT NULL,
  "created_at" integer NOT NULL,
  PRIMARY KEY ("id")
);
-- Create index "users_username_key" to table: "users"
CREATE UNIQUE INDEX "users_username_key" ON "users" ("username");
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
