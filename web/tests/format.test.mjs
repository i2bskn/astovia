import assert from 'node:assert/strict';
import { test } from 'node:test';
import { dueLabel, eventRange, happenedAt, refDetail, refLabel, relativeTime, serviceLabel, shortDate } from '../.test-build/format.js';

// Dates are built in local time so that the tests do not depend on TZ.
const now = new Date(2026, 9, 24, 10, 0); // 2026-10-24 (Sat) 10:00
const at = (...parts) => new Date(...parts).toISOString();

test('short dates show the weekday and the year only when it differs', () => {
  assert.equal(shortDate(new Date(2026, 9, 30), now), '10/30(金)');
  assert.equal(shortDate(new Date(2025, 11, 31), now), '2025/12/31(水)');
});

test('due labels name today and tomorrow and mark past dates', () => {
  assert.deepEqual(dueLabel(at(2026, 9, 24, 15, 5), now), { text: '今日 15:05', tone: 'today' });
  assert.deepEqual(dueLabel(at(2026, 9, 24, 9, 0), now), { text: '今日 9:00', tone: 'past' });
  assert.deepEqual(dueLabel(at(2026, 9, 25, 3, 2), now), { text: '明日 3:02', tone: 'soon' });
  assert.deepEqual(dueLabel(at(2026, 9, 23, 1, 2), now), { text: '昨日 1:02', tone: 'past' });
  assert.deepEqual(dueLabel(at(2026, 9, 20, 12, 0), now), { text: '10/20(火) 12:00', tone: 'past' });
  assert.deepEqual(dueLabel(at(2026, 10, 2, 9, 30), now), { text: '11/2(月) 9:30', tone: 'later' });
  assert.equal(dueLabel('not a date', now), null);
});

test('relative times count minutes, hours and days for a week', () => {
  assert.equal(relativeTime(at(2026, 9, 24, 9, 59, 30), now), 'たった今');
  assert.equal(relativeTime(at(2026, 9, 24, 9, 15), now), '45分前');
  assert.equal(relativeTime(at(2026, 9, 24, 7, 0), now), '3時間前');
  assert.equal(relativeTime(at(2026, 9, 21, 10, 0), now), '3日前');
  assert.equal(relativeTime(at(2026, 9, 10, 10, 0), now), '10/10(土)');
  assert.equal(relativeTime(at(2026, 9, 25, 8, 0), now), '10/25(日) 8:00');
  assert.equal(relativeTime('x', now), 'x');
});

test('references get short labels and keep ids for the tooltip', () => {
  const ref = { service: 'slack', account: 'acme', conversation_id: 'C1', message_id: 'M1', url: 'https://acme.slack.com/x' };
  assert.equal(refLabel(ref), 'Slack · acme');
  assert.equal(refDetail(ref), 'slack / acme / C1 / M1');
  assert.equal(refLabel({ service: 'note' }), 'note');
  assert.equal(serviceLabel('GitHub'), 'GitHub');
  assert.equal(serviceLabel('toString'), 'toString');
});

test('happenedAt joins the relative time and the event', () => {
  assert.equal(happenedAt(at(2026, 9, 24, 7, 0), now, '検出'), '3時間前に検出');
  assert.equal(happenedAt(at(2026, 9, 24, 9, 59, 50), now, '追加'), 'たった今追加');
  assert.equal(happenedAt(at(2026, 9, 10, 10, 0), now, '処理'), '10/10(土)に処理');
});

test('event ranges show the end time, with its date only on another day', () => {
  assert.equal(eventRange(at(2026, 9, 29, 12, 30), at(2026, 9, 29, 13, 30), now), '10/29(木) 12:30〜13:30');
  assert.equal(eventRange(at(2026, 9, 29, 23, 0), at(2026, 9, 30, 1, 0), now), '10/29(木) 23:00〜10/30(金) 1:00');
  assert.equal(eventRange(at(2026, 9, 29, 9, 5), null, now), '10/29(木) 9:05');
  assert.equal(eventRange('x', null, now), 'x');
});
