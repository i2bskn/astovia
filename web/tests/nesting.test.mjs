import assert from 'node:assert/strict';
import { test } from 'node:test';
import { backLink, canAddChild, canJoinParent, detailTitle, parentChoices, splitUrgent, taskPath, urgentTime, urgentTone } from '../.test-build/nesting.js';
import { autoReasonLabel } from '../.test-build/automation.js';
import { dueLabel } from '../.test-build/format.js';

function task(id, extra = {}) {
  return {
    id, content: id, status: 'open', importance: 'normal', note: null, due_at: null, refs: [], version: 1,
    created_at: '2026-10-28T00:00:00Z', updated_at: '2026-10-28T00:00:00Z', kind: 'task', event: null,
    parent_id: null, parent: null, children: null, ...extra,
  };
}

function children(nextDueAt, extra = {}) {
  return { total: 2, done: 0, remaining: 2, next_due_at: nextDueAt, ...extra };
}

const now = new Date(2026, 9, 28, 12, 0);
// The rule the task list uses: the tone of dueLabel.
const isUrgent = (value) => urgentTone(dueLabel(value, now)?.tone);
const local = (day, hour = 9) => new Date(2026, 9, day, hour, 0).toISOString();
const ids = (items) => items.map((i) => i.id);

test('past dates, today and tomorrow are urgent', () => {
  assert.equal(isUrgent(local(27)), true);
  assert.equal(isUrgent(local(28, 9)), true);
  assert.equal(isUrgent(local(28, 18)), true);
  assert.equal(isUrgent(local(29, 23)), true);
  assert.equal(isUrgent(local(30, 0)), false);
  assert.equal(isUrgent('not a date'), false);
  assert.equal(urgentTone(undefined), false);
  assert.equal(urgentTone('later'), false);
});

test('a parent is urgent by the next due date of its subtasks', () => {
  const parent = task('p', { children: children(local(29)) });
  assert.equal(urgentTime(parent, isUrgent), Date.parse(local(29)));
  // Without a near date of its own or of a subtask, it is not.
  assert.equal(urgentTime(task('q', { children: children(local(30)) }), isUrgent), null);
  assert.equal(urgentTime(task('r', { children: children(null) }), isUrgent), null);
  // The earlier of the two dates counts.
  assert.equal(urgentTime(task('s', { due_at: local(29), children: children(local(27)) }), isUrgent), Date.parse(local(27)));
  assert.equal(urgentTime(task('t', { due_at: local(27), children: children(local(29)) }), isUrgent), Date.parse(local(27)));
  // Only open items are urgent.
  assert.equal(urgentTime(task('u', { status: 'done', due_at: local(27), children: children(local(27)) }), isUrgent), null);
});

test('urgent items come first, by the nearest date, past dates first', () => {
  const items = [
    task('later'),
    task('parent', { children: children(local(29)) }),
    task('past', { due_at: local(26) }),
    task('far parent', { children: children(local(31)) }),
    task('today', { due_at: local(28, 15), children: children(local(30)) }),
    task('past'),
  ];
  const { soon, rest } = splitUrgent(items, isUrgent);
  assert.deepEqual(ids(soon), ['past', 'today', 'parent']);
  assert.deepEqual(ids(rest), ['later', 'far parent']);
  // Without urgent dates the order is kept, each item once.
  assert.deepEqual(splitUrgent(items, () => false), { soon: [], rest: [items[0], items[1], items[2], items[3], items[4]] });
});

test('subtasks can be added to an open item without a parent', () => {
  assert.equal(canAddChild(task('a')), true);
  assert.equal(canAddChild(task('a', { children: children(null) })), true);
  assert.equal(canAddChild(task('a', { parent_id: 'p' })), false);
  assert.equal(canAddChild(task('a', { status: 'candidate' })), false);
  assert.equal(canAddChild(task('a', { status: 'done' })), false);
  assert.equal(canAddChild(task('a', { kind: 'event', event: { title: 'x', start_at: local(30), end_at: null } })), true);
});

test('an item can be put under another open item without a parent', () => {
  const items = [task('a'), task('b'), task('done', { status: 'done' }), task('child', { parent_id: 'b' }), task('b'), task('parent', { children: children(null) })];
  assert.deepEqual(ids(parentChoices(items[0], items)), ['b', 'parent']);
  // A subtask, an item with subtasks, or one that is not open cannot be put
  // under one.
  assert.equal(canJoinParent(items[3]), false);
  assert.deepEqual(parentChoices(items[3], items), []);
  assert.deepEqual(parentChoices(items[5], items), []);
  assert.deepEqual(parentChoices(task('x', { status: 'candidate' }), items), []);
  assert.equal(canJoinParent(task('x')), true);
});

test('the detail screen goes back to the parent or to the list', () => {
  assert.equal(taskPath('a-b_1'), '/tasks/a-b_1');
  assert.equal(taskPath('a/b'), '/tasks/a%2Fb');
  assert.deepEqual(backLink(task('a')), { to: '/', label: '← タスク' });
  assert.deepEqual(backLink(task('c', { parent_id: 'p', parent: { id: 'p', content: '障害対応', status: 'open' } })), { to: '/tasks/p', label: '← 親: 障害対応' });
  // Without the parent's summary, back to the list.
  assert.deepEqual(backLink(task('c', { parent_id: 'p' })), { to: '/', label: '← タスク' });
});

test('the tab title starts with the content', () => {
  assert.equal(detailTitle('日程を返答する'), '日程を返答する – Astovia');
  assert.equal(detailTitle(' 一行目\n二行目 '), '一行目 二行目 – Astovia');
  assert.equal(detailTitle('あ'.repeat(50)), `${'あ'.repeat(40)} – Astovia`);
  assert.equal(detailTitle('😀'.repeat(41)), `${'😀'.repeat(40)} – Astovia`);
});

test('a parent completed by its subtasks has a label', () => {
  assert.equal(autoReasonLabel('children.done'), '子タスクがすべて完了した');
});
