import assert from 'node:assert/strict';
import { test } from 'node:test';
import { calendarURL, createActionItemsClient, isConflict, isInvalid, isNotFound } from '../.test-build/actionItems.js';

const item = {
  id: 'item-1',
  content: '日程を返答する',
  status: 'open',
  importance: 'normal',
  note: null,
  due_at: null,
  refs: [{ service: 'slack', url: 'https://example.com/x' }],
  version: 1,
  created_at: '2026-10-23T01:02:03Z',
  updated_at: '2026-10-23T01:02:03Z',
  kind: 'task',
  event: null,
  parent_id: null,
  parent: null,
  children: null,
};

const event = {
  ...item,
  content: '10/29(木) 12:30 打ち合わせを登録する',
  status: 'candidate',
  kind: 'event',
  event: { title: '被害件数の対応 MTG（高谷さん・Kiyoさん）', start_at: '2026-10-29T03:30:00Z', end_at: '2026-10-29T04:30:00Z' },
  refs: [{ service: 'slack', url: 'https://acme.slack.com/archives/C1/p1700000000000100' }],
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
  await client.createActionItem({ content: 'z', parent_id: 'parent-1' });
  assert.deepEqual(JSON.parse(calls[2].init.body), { content: 'z', note: null, parent_id: 'parent-1' });
});

test('subtasks and parents are accepted', async () => {
  const child = { ...item, id: 'child-1', parent_id: 'parent-1', parent: { id: 'parent-1', content: '障害対応', status: 'open' } };
  const parent = { ...item, id: 'parent-1', children: { total: 3, done: 2, remaining: 1, next_due_at: null } };
  const soon = { ...parent, id: 'parent-2', children: { total: 1, done: 0, remaining: 1, next_due_at: '2026-10-29T00:00:00Z' } };
  const { client } = fake({ items: [parent, soon, child, { ...child, parent: null }], next_cursor: null });
  assert.equal((await client.listActionItems()).items.length, 4);
});

test('listActionItems asks for the items without a parent', async () => {
  const { client, calls } = fake({ items: [item], next_cursor: null });
  await client.listActionItems({ status: 'open', view: 'top', cursor: 'abc' });
  await client.listActionItems({ view: 'top' });
  assert.equal(calls[0].path, '/api/v1/action-items?status=open&view=top&cursor=abc');
  assert.equal(calls[1].path, '/api/v1/action-items?view=top');
});

test('getActionItem and listChildren read one item and its subtasks', async () => {
  const { client, calls } = fake(item);
  assert.deepEqual(await client.getActionItem('item 1'), item);
  assert.equal(calls[0].path, '/api/v1/action-items/item%201');
  assert.equal(calls[0].init, undefined);
  const child = { ...item, id: 'child-1', parent_id: 'item-1', parent: { id: 'item-1', content: '親', status: 'open' } };
  const page = fake({ items: [child], next_cursor: null });
  assert.deepEqual(await page.client.listChildren('item-1'), { items: [child], next_cursor: null });
  await page.client.listChildren('a&b');
  assert.equal(page.calls[0].path, '/api/v1/action-items?parent_id=item-1');
  assert.equal(page.calls[1].path, '/api/v1/action-items?parent_id=a%26b');
  await assert.rejects(fake({ ...item, status: 'closed' }).client.getActionItem('item-1'), /Unexpected action item response/);
  await assert.rejects(fake({ items: [{ ...child, parent_id: 1 }], next_cursor: null }).client.listChildren('item-1'), /Unexpected action item list response/);
});

test('updateActionItem moves an item out of its parent', async () => {
  const { client, calls } = fake(item);
  await client.updateActionItem('item-1', { parent_id: null, version: 1 });
  assert.deepEqual(JSON.parse(calls[0].init.body), { parent_id: null, version: 1 });
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

test('importance is one of high, normal and low', async () => {
  const { client, calls } = fake({ items: ['high', 'normal', 'low'].map((importance) => ({ ...item, importance })), next_cursor: null });
  assert.equal((await client.listActionItems()).items.length, 3);
  const { importance: _importance, ...withoutImportance } = item;
  for (const body of [withoutImportance, { ...item, importance: 'urgent' }, { ...item, importance: '' }, { ...item, importance: null }, { ...item, importance: 1 }]) {
    await assert.rejects(fake(body).client.getActionItem('item-1'), /Unexpected action item response/);
  }
  const patch = fake({ ...item, importance: 'high', version: 2 });
  assert.equal((await patch.client.updateActionItem('item-1', { importance: 'high', version: 1 })).importance, 'high');
  assert.deepEqual(JSON.parse(patch.calls[0].init.body), { importance: 'high', version: 1 });
  assert.equal(calls.length, 1);
});

test('candidates and events are accepted', async () => {
  const { client } = fake({ items: [event, { ...event, event: { ...event.event, end_at: null } }], next_cursor: null });
  assert.equal((await client.listActionItems({ status: 'candidate' })).items.length, 2);
});

test('calendarURL opens a Google Calendar event in UTC', () => {
  const url = new URL(calendarURL(event));
  assert.equal(url.origin + url.pathname, 'https://calendar.google.com/calendar/render');
  assert.equal(url.searchParams.get('action'), 'TEMPLATE');
  assert.equal(url.searchParams.get('text'), '被害件数の対応 MTG（高谷さん・Kiyoさん）');
  assert.equal(url.searchParams.get('dates'), '20261029T033000Z/20261029T043000Z');
  assert.equal(url.searchParams.get('details'), 'https://acme.slack.com/archives/C1/p1700000000000100');
  assert.match(calendarURL(event), /text=%E8%A2%AB.*&dates=20261029T033000Z%2F20261029T043000Z&details=https%3A%2F%2Facme/);
  // Without an end the event lasts an hour; only https sources are linked.
  const open = new URL(calendarURL({ ...event, event: { ...event.event, end_at: null }, refs: [{ service: 'x', url: 'http://example.com/' }] }));
  assert.equal(open.searchParams.get('dates'), '20261029T033000Z/20261029T043000Z');
  assert.equal(open.searchParams.has('details'), false);
  assert.equal(new URL(calendarURL({ ...event, refs: [] })).searchParams.has('details'), false);
  assert.equal(calendarURL(item), null);
  assert.equal(calendarURL({ ...event, event: { ...event.event, start_at: 'x' } }), null);
});

test('malformed responses are rejected', async () => {
  const { refs: _omitted, ...withoutRefs } = item;
  const { kind: _kind, ...withoutKind } = item;
  const { event: _event, ...withoutEvent } = item;
  const { parent_id: _parentID, ...withoutParentID } = item;
  const { parent: _parent, ...withoutParent } = item;
  const { children: _children, ...withoutChildren } = item;
  const nesting = [withoutParentID, withoutParent, withoutChildren, { ...item, parent_id: '' }, { ...item, parent_id: 'item-1' }, { ...item, parent_id: 1 },
    { ...item, parent: { id: 'p', content: 'x', status: 'open' } }, { ...item, parent_id: 'p', parent: { id: 'q', content: 'x', status: 'open' } },
    { ...item, parent_id: 'p', parent: { id: 'p', content: 'x', status: 'closed' } }, { ...item, parent_id: 'p', parent: { id: 'p', status: 'open' } },
    { ...item, children: { total: 1, done: 0 } }, { ...item, children: { total: 1, done: -1, remaining: 1, next_due_at: null } }, { ...item, children: { total: 1.5, done: 0, remaining: 1, next_due_at: null } }, { ...item, children: [] },
    { ...item, children: { total: 1, done: 0, remaining: 1 } }, { ...item, children: { total: 1, done: 0, remaining: 1, next_due_at: 1 } }];
  for (const body of [...nesting, null, [], { ...item, status: 'closed' }, { ...item, version: '1' }, { ...item, note: 1 }, withoutRefs, { ...item, refs: [{ url: 'https://example.com' }] },
    withoutKind, withoutEvent, { ...item, kind: 'meeting' }, { ...item, event: event.event }, { ...event, event: null }, { ...event, event: { title: 'x', start_at: 1, end_at: null } }, { ...event, event: { title: 'x', start_at: 'y' } }]) {
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
  assert.equal(isNotFound(new APIError(404)), true);
  assert.equal(isNotFound(conflict), false);
  assert.equal(isNotFound({ status: 404 }), false);
  assert.equal(isInvalid(new APIError(400)), true);
  assert.equal(isInvalid(new APIError(404)), false);
  await assert.rejects(fake(new APIError(404)).client.getActionItem('missing'), (error) => isNotFound(error));
});
