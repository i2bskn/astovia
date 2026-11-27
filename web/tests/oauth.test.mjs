import assert from 'node:assert/strict';
import { test } from 'node:test';
import { consentErrorMessage, createOAuthClient, isUserChanged } from '../.test-build/oauth.js';

const consent = {
  status: 'ok',
  client_name: 'Claude Code',
  client_id: 'https://claude.ai/oauth/claude-code-client-metadata',
  client_host: 'claude.ai',
  redirect_host: 'localhost',
  redirect_loopback: true,
  scopes: ['mcp', 'offline_access'],
  user_id: 'user-1',
};

const rejection = {
  status: 'error',
  error: 'invalid_scope',
  redirect_url: 'https://claude.ai/api/mcp/auth_callback?error=invalid_scope&state=s',
  redirect_host: 'claude.ai',
};

const connection = {
  id: 'grant-1',
  client_name: 'Tool',
  client_host: null,
  scopes: ['mcp'],
  created_at: '2026-10-23T01:02:03Z',
  last_used_at: null,
  expires_at: '2026-10-30T01:02:03Z',
};

function fake(response) {
  const calls = [];
  const client = createOAuthClient(async (path, init) => {
    calls.push({ path, init });
    if (response instanceof Error) throw response;
    return response;
  });
  return { client, calls };
}

test('getAuthorization passes the query of the consent screen on unchanged', async () => {
  const query = 'response_type=code&client_id=https%3A%2F%2Fclaude.ai%2Fm&redirect_uri=http%3A%2F%2Flocalhost%3A5%2Fcb&scope=mcp+offline_access&state=a%2Fb';
  const { client, calls } = fake({ ...consent, extra: 1 });
  assert.deepEqual(await client.getAuthorization(query), consent);
  assert.equal(calls[0].path, `/api/v1/oauth/authorize?${query}`);
  assert.equal(calls[0].init, undefined);
  const registered = { ...consent, client_id: 'dcr_x', client_host: null, scopes: ['mcp'] };
  assert.deepEqual(await fake(registered).client.getAuthorization('x=1'), registered);
  // Without a query nothing is appended.
  const empty = fake(consent);
  await empty.client.getAuthorization('');
  assert.equal(empty.calls[0].path, '/api/v1/oauth/authorize');
});

test('getAuthorization returns a refused request with its redirect', async () => {
  assert.deepEqual(await fake({ ...rejection, extra: 1 }).client.getAuthorization('x=1'), rejection);
  for (const body of [{ ...rejection, redirect_url: 'javascript:alert(1)' }, { ...rejection, redirect_url: 'myapp://cb' }, { ...rejection, error: '' }, { ...rejection, redirect_host: 1 }]) {
    await assert.rejects(fake(body).client.getAuthorization('x=1'), /Unexpected consent response/, JSON.stringify(body));
  }
});

test('decide posts the query, the decision and the user shown, and returns an http(s) redirect URL', async () => {
  const { client, calls } = fake({ redirect_url: 'http://localhost:5/callback?code=c&state=s' });
  assert.equal(await client.decide('a=1&b=%2F', 'approve', 'user-1'), 'http://localhost:5/callback?code=c&state=s');
  assert.equal(calls[0].path, '/api/v1/oauth/authorize');
  assert.equal(calls[0].init.method, 'POST');
  assert.equal(new Headers(calls[0].init.headers).get('content-type'), 'application/json');
  assert.deepEqual(JSON.parse(calls[0].init.body), { query: 'a=1&b=%2F', decision: 'approve', user_id: 'user-1' });
  const denied = fake({ redirect_url: 'https://claude.ai/cb?error=access_denied' });
  assert.equal(await denied.client.decide('q', 'deny', 'user-1'), 'https://claude.ai/cb?error=access_denied');
  assert.deepEqual(JSON.parse(denied.calls[0].init.body), { query: 'q', decision: 'deny', user_id: 'user-1' });
  for (const body of [null, {}, { redirect_url: 1 }, { redirect_url: 'javascript:alert(1)' }, { redirect_url: 'myapp://cb' }, { redirect_url: '/admin/' }, { redirect_url: 'HTTPS://x' }]) {
    await assert.rejects(fake(body).client.decide('q', 'approve', 'user-1'), /Unexpected consent response/);
  }
});

test('connections are listed and deleted', async () => {
  const { client, calls } = fake({ items: [connection] });
  assert.deepEqual(await client.listConnections(), [connection]);
  assert.equal(calls[0].path, '/api/v1/oauth/connections');
  const deleted = fake(undefined);
  assert.equal(await deleted.client.deleteConnection('a/b'), undefined);
  assert.equal(deleted.calls[0].path, '/api/v1/oauth/connections/a%2Fb');
  assert.equal(deleted.calls[0].init.method, 'DELETE');
});

test('malformed responses are rejected', async () => {
  for (const body of [null, [], {}, { ...consent, status: undefined }, { ...consent, status: 'pending' }, { ...consent, client_name: '' }, { ...consent, client_id: '' },
    { ...consent, scopes: ['admin'] }, { ...consent, redirect_loopback: 'yes' }, { ...consent, client_host: 1 }, { ...consent, user_id: '' }, { ...consent, user_id: null }]) {
    await assert.rejects(fake(body).client.getAuthorization('x=1'), /Unexpected consent response/, JSON.stringify(body));
  }
  for (const body of [null, {}, { items: {} }, { items: [{}] }, { items: [{ ...connection, id: '' }] }, { items: [{ ...connection, last_used_at: 1 }] }, { items: [{ ...connection, scopes: 'mcp' }] }]) {
    await assert.rejects(fake(body).client.listConnections(), /Unexpected connection list response/);
  }
});

test('errors from request are passed through and described', async () => {
  const failure = (status, code = null) => Object.assign(new Error('Request failed'), { status, code });
  const invalid = failure(400, 'invalid_request');
  await assert.rejects(fake(invalid).client.getAuthorization('x=1'), (error) => error === invalid);
  // Another user signed in after the screen was shown.
  assert.equal(isUserChanged(failure(409, 'user_changed')), true);
  assert.equal(isUserChanged(failure(409, 'conflict')), false);
  assert.equal(isUserChanged({ status: 409, code: 'user_changed' }), false);
  assert.match(consentErrorMessage(failure(409, 'user_changed')), /アカウントが変わりました.*読み込み直して/);
  assert.match(consentErrorMessage(invalid), /無効か期限切れ/);
  assert.match(consentErrorMessage(failure(400, 'unknown_client')), /削除して、追加し直して/);
  assert.match(consentErrorMessage(failure(400, 'invalid_client')), /情報を確認できませんでした/);
  assert.match(consentErrorMessage(failure(503, 'client_metadata_unavailable')), /情報を取得できませんでした/);
  assert.match(consentErrorMessage(failure(429, 'rate_limited')), /試行回数が上限/);
  assert.match(consentErrorMessage(failure(503, 'unavailable')), /サーバーに接続できませんでした/);
  assert.match(consentErrorMessage(new TypeError('Failed to fetch')), /サーバーに接続できませんでした/);
  const messages = new Set(['user_changed', 'unknown_client', 'invalid_client', 'client_metadata_unavailable'].map((code) => consentErrorMessage(failure(400, code))));
  assert.equal(messages.size, 4);
});
