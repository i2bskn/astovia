import assert from 'node:assert/strict';
import { test } from 'node:test';
import { createOAuthClient, isBadRequest } from '../.test-build/oauth.js';

const consent = {
  client_name: 'Claude Code',
  client_kind: 'cimd',
  client_host: 'claude.ai',
  redirect_host: 'localhost',
  redirect_loopback: true,
  scopes: ['mcp', 'offline_access'],
  expires_at: '2026-10-23T01:12:03Z',
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

test('getConsent sends the request as a query parameter', async () => {
  const { client, calls } = fake(consent);
  assert.deepEqual(await client.getConsent('a.b+c/='), consent);
  assert.equal(calls[0].path, '/api/v1/oauth/consent?request=a.b%2Bc%2F%3D');
  assert.equal(calls[0].init, undefined);
  const dcr = { ...consent, client_kind: 'dcr', client_host: null, scopes: ['mcp'] };
  assert.deepEqual(await fake(dcr).client.getConsent('x'), dcr);
});

test('decide posts the decision and returns an http(s) redirect URL', async () => {
  const { client, calls } = fake({ redirect_url: 'http://localhost:5/callback?code=asc_x&state=s' });
  assert.equal(await client.decide('req', true), 'http://localhost:5/callback?code=asc_x&state=s');
  assert.equal(calls[0].path, '/api/v1/oauth/consent');
  assert.equal(calls[0].init.method, 'POST');
  assert.equal(new Headers(calls[0].init.headers).get('content-type'), 'application/json');
  assert.deepEqual(JSON.parse(calls[0].init.body), { request: 'req', approve: true });
  assert.equal(await fake({ redirect_url: 'https://claude.ai/cb?error=access_denied' }).client.decide('req', false), 'https://claude.ai/cb?error=access_denied');
  for (const body of [null, {}, { redirect_url: 1 }, { redirect_url: 'javascript:alert(1)' }, { redirect_url: 'myapp://cb' }, { redirect_url: '/admin/' }, { redirect_url: 'HTTPS://x' }]) {
    await assert.rejects(fake(body).client.decide('req', true), /Unexpected consent response/);
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
  for (const body of [null, [], {}, { ...consent, client_name: '' }, { ...consent, client_kind: 'other' }, { ...consent, scopes: ['admin'] }, { ...consent, redirect_loopback: 'yes' }, { ...consent, client_host: 1 }]) {
    await assert.rejects(fake(body).client.getConsent('x'), /Unexpected consent response/);
  }
  for (const body of [null, {}, { items: {} }, { items: [{}] }, { items: [{ ...connection, id: '' }] }, { items: [{ ...connection, last_used_at: 1 }] }, { items: [{ ...connection, scopes: 'mcp' }] }]) {
    await assert.rejects(fake(body).client.listConnections(), /Unexpected connection list response/);
  }
});

test('errors from request are passed through', async () => {
  const failure = Object.assign(new Error('Request failed'), { status: 400 });
  await assert.rejects(fake(failure).client.getConsent('x'), (error) => isBadRequest(error));
  assert.equal(isBadRequest(Object.assign(new Error('x'), { status: 401 })), false);
  assert.equal(isBadRequest({ status: 400 }), false);
});
