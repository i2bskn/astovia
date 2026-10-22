schema "public" {}
table "users" {
  schema = schema.public
  column "id" {
    type = text
    null = false
  }
  column "username" {
    type = text
    null = false
  }
  column "password_hash" {
    type = text
    null = false
  }
  column "disabled" {
    type = bigint
    null = false
  }
  column "created_at" {
    type = bigint
    null = false
  }
  primary_key {
    columns = [column.id]
  }
  index "users_username_key" {
    unique = true
    columns = [column.username]
  }
}
table "sessions" {
  schema = schema.public
  column "id" {
    type = text
    null = false
  }
  column "user_id" {
    type = text
    null = false
  }
  column "expires_at" {
    type = bigint
    null = false
  }
  column "revoked_at" {
    type = bigint
    null = true
  }
  column "created_at" {
    type = bigint
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
  schema = schema.public
  column "hash" {
    type = text
    null = false
  }
  column "session_id" {
    type = text
    null = false
  }
  column "consumed_at" {
    type = bigint
    null = true
  }
  column "created_at" {
    type = bigint
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
