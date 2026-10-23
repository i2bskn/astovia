import assert from 'node:assert/strict';
import { test } from 'node:test';
import { createAPITokensClient } from '../.test-build/apiTokens.js';

const token = {
  id: 'token-1',
  name: 'Claude Code',
  created_at: '2026-10-23T01:02:03Z',
  last_used_at: null,
};
const created = { ...token, token: 'ast_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA' };

function fake(response) {
  const calls = [];
  const client = createAPITokensClient(async (path, init) => {
    calls.push({ path, init });
    if (response instanceof Error) throw response;
    return response;
  });
  return { client, calls };
}

test('listAPITokens gets the list', async () => {
  const used = { ...token, id: 'token-2', last_used_at: '2026-10-23T02:00:00Z' };
  const { client, calls } = fake({ items: [used, token] });
  assert.deepEqual(await client.listAPITokens(), [used, token]);
  assert.equal(calls[0].path, '/api/v1/api-tokens');
  assert.equal(calls[0].init, undefined);
  assert.deepEqual(await fake({ items: [] }).client.listAPITokens(), []);
});

test('createAPIToken posts the name as JSON', async () => {
  const { client, calls } = fake(created);
  assert.deepEqual(await client.createAPIToken('Claude Code'), created);
  assert.equal(calls[0].path, '/api/v1/api-tokens');
  assert.equal(calls[0].init.method, 'POST');
  assert.equal(new Headers(calls[0].init.headers).get('content-type'), 'application/json');
  assert.deepEqual(JSON.parse(calls[0].init.body), { name: 'Claude Code' });
});

test('deleteAPIToken sends DELETE without a body', async () => {
  const { client, calls } = fake(undefined);
  assert.equal(await client.deleteAPIToken('a/b'), undefined);
  assert.equal(calls[0].path, '/api/v1/api-tokens/a%2Fb');
  assert.equal(calls[0].init.method, 'DELETE');
  assert.equal(calls[0].init.body, undefined);
});

test('malformed responses are rejected', async () => {
  for (const body of [null, [], {}, { items: {} }, { items: [{}] }, { items: [{ ...token, id: '' }] }, { items: [{ ...token, last_used_at: 1 }] }, { items: [{ ...token, name: null }] }]) {
    await assert.rejects(fake(body).client.listAPITokens(), /Unexpected API token list response/);
  }
  const { token: _secret, ...withoutToken } = created;
  for (const body of [null, [], token, withoutToken, { ...created, token: 1 }, { ...created, token: 'secret' }, { ...created, created_at: null }]) {
    await assert.rejects(fake(body).client.createAPIToken('x'), /Unexpected API token response/);
  }
});

test('errors from request are passed through', async () => {
  const failure = Object.assign(new Error('Request failed'), { status: 409 });
  await assert.rejects(fake(failure).client.createAPIToken('x'), (error) => error.status === 409);
});

test('request resolves 204 responses without parsing a body', async () => {
  // auth.ts opens a BroadcastChannel on import; a real one would keep Node running.
  globalThis.BroadcastChannel = class {
    addEventListener() {}
    postMessage() {}
  };
  const calls = [];
  globalThis.fetch = async (path, init) => {
    calls.push({ path, init });
    return new Response(null, { status: 204 });
  };
  const { request } = await import(`../.test-build/auth.js?test=${crypto.randomUUID()}`);
  assert.equal(await request('/api/v1/api-tokens/token-1', { method: 'DELETE' }), undefined);
  assert.equal(calls[0].init.method, 'DELETE');
});
