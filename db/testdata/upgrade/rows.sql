-- Rows in the database before authlume, in both dialects, for
-- scripts/check-schema-upgrade.sh: users with the second factors of that
-- version, sessions and refresh tokens, an API token, MCP OAuth clients,
-- grants and tokens, and an invitation and a password reset.
INSERT INTO users (id, email, password_hash, disabled, created_at, role, oidc_issuer, oidc_subject) VALUES
  ('alice-id', 'alice@example.com', '$2a$10$abcdefghijklmnopqrstuuJ4yW6oE0Gx7L0D1yvH2b3c4d5e6f7g8', 0, 1, 'owner', NULL, NULL),
  ('bob-id', 'bob@example.com', '!access', 0, 2, 'member', 'https://idp.example', 'bob-sub'),
  ('carol-id', 'carol@example.com', '!access', 1, 3, 'member', NULL, NULL);
INSERT INTO sessions (id, user_id, expires_at, revoked_at, created_at) VALUES
  ('session-1', 'alice-id', 4102444800, NULL, 10),
  ('session-2', 'bob-id', 4102444800, 20, 10);
INSERT INTO refresh_tokens (hash, session_id, consumed_at, created_at) VALUES
  ('refresh-1', 'session-1', NULL, 10),
  ('refresh-2', 'session-1', 11, 10);
INSERT INTO api_tokens (id, user_id, name, token_hash, created_at, last_used_at) VALUES
  ('token-1', 'alice-id', 'cli', 'token-hash-1', 10, 11);
INSERT INTO oauth_clients (client_id, kind, name, redirect_uris, auth_method, secret_hash, metadata_expires_at, created_at, updated_at) VALUES
  ('client-1', 'dynamic', 'Client', '["https://client.example/callback"]', 'none', NULL, NULL, 10, 10);
INSERT INTO oauth_grants (id, user_id, client_id, client_name, scope, resource, redirect_uri, code_hash, code_challenge, code_expires_at, code_used_at, created_at, expires_at, last_used_at) VALUES
  ('grant-1', 'alice-id', 'client-1', 'Client', 'mcp', 'https://astovia.test/mcp', 'https://client.example/callback', 'code-hash-1', 'challenge', 20, 15, 10, 4102444800, 16);
INSERT INTO oauth_tokens (token_hash, kind, grant_id, user_id, created_at, expires_at) VALUES
  ('access-1', 'access', 'grant-1', 'alice-id', 15, 4102444800),
  ('refresh-3', 'refresh', 'grant-1', 'alice-id', 15, 4102444800);
INSERT INTO mfa_totp (user_id, secret_sealed, last_step, created_at) VALUES
  ('alice-id', 'sealed-secret', 5, 10);
INSERT INTO mfa_webauthn_credentials (id, user_id, credential_id, credential, name, created_at, last_used_at) VALUES
  ('key-1', 'alice-id', 'credential-1', '{}', 'Key', 10, NULL);
INSERT INTO mfa_recovery_codes (id, user_id, code_hash, created_at, used_at) VALUES
  ('code-1', 'alice-id', 'code-hash-1', 10, NULL),
  ('code-2', 'alice-id', 'code-hash-2', 10, 12);
INSERT INTO mfa_used_challenges (challenge_hash, expires_at) VALUES
  ('challenge-1', 4102444800);
INSERT INTO invitations (id, token_hash, email, role, invited_by, created_at, expires_at, accepted_at, accepted_user_id, revoked_at) VALUES
  ('invitation-1', 'invitation-hash-1', 'dave@example.com', 'member', 'alice-id', 10, 4102444800, NULL, NULL, NULL);
INSERT INTO password_resets (id, token_hash, user_id, created_by, created_at, expires_at, used_at, revoked_at) VALUES
  ('reset-1', 'reset-hash-1', 'alice-id', 'alice-id', 10, 4102444800, NULL, NULL);
