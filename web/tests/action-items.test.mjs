import assert from 'node:assert/strict';
import { test } from 'node:test';
import { createActionItemsClient, isConflict } from '../.test-build/actionItems.js';

const item = {
  id: 'item-1',
  content: '日程を返答する',
  status: 'open',
  note: null,
  due_at: null,
  refs: [{ service: 'slack', url: 'https://example.com/x' }],
  version: 1,
  created_at: '2026-10-23T01:02:03Z',
  updated_at: '2026-10-23T01:02:03Z',
};

function fake(response) {
  const calls = [];
  const client = createActionItemsClient(async (path, init) => {
    calls.push({ path, init });
    if (response instanceof Error) throw response;
    return response;
  });
  return { client, calls };
}

test('listActionItems adds status and cursor to the query', async () => {
  const { client, calls } = fake({ items: [item], next_cursor: 'next' });
  assert.deepEqual(await client.listActionItems(), { items: [item], next_cursor: 'next' });
  await client.listActionItems({ status: 'done', cursor: 'abc_-' });
  assert.equal(calls[0].path, '/api/v1/action-items');
  assert.equal(calls[0].init, undefined);
  assert.equal(calls[1].path, '/api/v1/action-items?status=done&cursor=abc_-');
});

test('listActionItems asks for automatically completed items', async () => {
  const auto = { ...item, status: 'done', source: 'github', auto: { reason: 'github.pr_merged', ref_url: null, at: '2026-10-24T10:00:00Z' } };
  const { client, calls } = fake({ items: [auto], next_cursor: 'a.next' });
  assert.deepEqual(await client.listActionItems({ status: 'done', auto: true, cursor: 'a.x' }), { items: [auto], next_cursor: 'a.next' });
  assert.equal(calls[0].path, '/api/v1/action-items?status=done&auto=true&cursor=a.x');
  for (const bad of [{ ...auto, source: 1 }, { ...auto, auto: { reason: 'x', ref_url: 1, at: 'y' } }, { ...auto, auto: { ref_url: null, at: 'y' } }, { ...auto, auto: 'x' }]) {
    await assert.rejects(fake({ items: [bad], next_cursor: null }).client.listActionItems(), /Unexpected action item list response/);
  }
});

test('createActionItem posts JSON', async () => {
  const { client, calls } = fake(item);
  assert.deepEqual(await client.createActionItem({ content: 'x', note: 'memo' }), item);
  await client.createActionItem({ content: 'y' });
  assert.equal(calls[0].path, '/api/v1/action-items');
  assert.equal(calls[0].init.method, 'POST');
  assert.equal(new Headers(calls[0].init.headers).get('content-type'), 'application/json');
  assert.deepEqual(JSON.parse(calls[0].init.body), { content: 'x', note: 'memo' });
  assert.deepEqual(JSON.parse(calls[1].init.body), { content: 'y', note: null });
});

test('updateActionItem patches JSON', async () => {
  const { client, calls } = fake({ ...item, status: 'done', version: 2 });
  const updated = await client.updateActionItem('item-1', { status: 'done', version: 1 });
  assert.equal(updated.version, 2);
  assert.equal(calls[0].path, '/api/v1/action-items/item-1');
  assert.equal(calls[0].init.method, 'PATCH');
  assert.equal(new Headers(calls[0].init.headers).get('content-type'), 'application/json');
  assert.deepEqual(JSON.parse(calls[0].init.body), { status: 'done', version: 1 });
});

test('malformed responses are rejected', async () => {
  const { refs: _omitted, ...withoutRefs } = item;
  for (const body of [null, [], { ...item, status: 'closed' }, { ...item, version: '1' }, { ...item, note: 1 }, withoutRefs, { ...item, refs: [{ url: 'https://example.com' }] }]) {
    await assert.rejects(fake(body).client.createActionItem({ content: 'x' }), /Unexpected action item response/);
    await assert.rejects(fake(body).client.updateActionItem('item-1', { status: 'done' }), /Unexpected action item response/);
  }
  for (const body of [null, { items: [item] }, { items: [{}], next_cursor: null }, { items: [item], next_cursor: 1 }, { items: {}, next_cursor: null }]) {
    await assert.rejects(fake(body).client.listActionItems(), /Unexpected action item list response/);
  }
});

test('the APIError status distinguishes conflicts', async () => {
  // auth.ts opens a BroadcastChannel on import; a real one would keep Node running.
  globalThis.BroadcastChannel = class {
    addEventListener() {}
    postMessage() {}
  };
  const { APIError } = await import(`../.test-build/auth.js?test=${crypto.randomUUID()}`);
  const conflict = new APIError(409);
  assert.equal(conflict.status, 409);
  await assert.rejects(fake(conflict).client.updateActionItem('item-1', { status: 'done', version: 1 }), (error) => error.status === 409);
  assert.equal(isConflict(conflict), true);
  assert.equal(isConflict(new APIError(404)), false);
  assert.equal(isConflict(new Error('x')), false);
  assert.equal(isConflict({ status: 409 }), false);
});
