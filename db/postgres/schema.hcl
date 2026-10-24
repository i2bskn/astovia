schema "public" {}
table "users" {
  schema = schema.public
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
  index "users_email_key" {
    unique = true
    columns = [column.email]
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
table "action_items" {
  schema = schema.public
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
    type = bigint
    null = true
  }
  column "refs" {
    type = text
    null = false
  }
  column "version" {
    type = bigint
    null = false
  }
  column "created_at" {
    type = bigint
    null = false
  }
  column "updated_at" {
    type = bigint
    null = false
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
}
table "api_tokens" {
  schema = schema.public
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
    type = bigint
    null = false
  }
  column "last_used_at" {
    type = bigint
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
  schema = schema.public
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
    type = bigint
    null = true
  }
  column "created_at" {
    type = bigint
    null = false
  }
  column "updated_at" {
    type = bigint
    null = false
  }
  primary_key {
    columns = [column.client_id]
  }
}
table "oauth_grants" {
  schema = schema.public
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
    type = bigint
    null = false
  }
  column "code_used_at" {
    type = bigint
    null = true
  }
  column "created_at" {
    type = bigint
    null = false
  }
  column "expires_at" {
    type = bigint
    null = false
  }
  column "last_used_at" {
    type = bigint
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
  schema = schema.public
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
    type = bigint
    null = false
  }
  column "expires_at" {
    type = bigint
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
