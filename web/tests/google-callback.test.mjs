import assert from 'node:assert/strict';
import { test } from 'node:test';

const state = `gst_${'A'.repeat(42)}Q`;

async function fresh() {
  return import(`../.test-build/googleCallback.js?test=${crypto.randomUUID()}`);
}

function fakeHistory() {
  const calls = [];
  return { calls, state: { idx: 2 }, replaceState(data, unused, url) { calls.push([data, unused, url]); } };
}

test('the callback is /integrations/google/callback', async () => {
  const { isGoogleCallbackPath } = await fresh();
  assert.equal(isGoogleCallbackPath('/integrations/google/callback'), true);
  assert.equal(isGoogleCallbackPath('/integrations/google/callback/'), true);
  for (const p of ['/', '/integrations/google', '/integrations/google/callback//', '/integrations/google/callbacks', '/settings']) assert.equal(isGoogleCallbackPath(p), false, p);
});

test('the result is read only in its own form', async () => {
  const { callbackFromSearch } = await fresh();
  assert.deepEqual(callbackFromSearch(`?state=${state}&code=4%2F0Ab-c_d&scope=x&authuser=0`), { state, code: '4/0Ab-c_d' });
  assert.deepEqual(callbackFromSearch(`?state=${state}&error=access_denied`), { state, error: 'access_denied' });
  assert.deepEqual(callbackFromSearch(`?state=${state}&code=${'c'.repeat(2048)}`), { state, code: 'c'.repeat(2048) });
  for (const search of [
    '',
    `?code=c`,
    `?state=${state}`,
    `?state=${state}&code=c&error=access_denied`,
    `?state=${state}&code=`,
    `?state=${state}&code=a+b`,
    `?state=${state}&code=${'c'.repeat(2049)}`,
    `?state=${state}&code=%E3%81%82`,
    `?state=${state}&error=Access-Denied`,
    `?state=${state}&error=${'a'.repeat(101)}`,
    `?state=${state}x&code=c`,
    `?state=inv_${'A'.repeat(43)}&code=c`,
    `?state=${state}&state=${state}&code=c`,
    `?state=${state}&code=c&code=d`,
  ]) assert.equal(callbackFromSearch(search), null, search);
});

test('the result is captured once and removed from the URL', async () => {
  const { captureGoogleCallback, googleCallback } = await fresh();
  const history = fakeHistory();
  captureGoogleCallback({ pathname: '/integrations/google/callback', search: `?state=${state}&code=c1`, hash: '' }, history);
  assert.deepEqual(googleCallback(), { state, code: 'c1' });
  assert.deepEqual(history.calls, [[{ idx: 2 }, '', '/integrations/google/callback']]);
  // Later calls keep what was read.
  captureGoogleCallback({ pathname: '/integrations/google/callback', search: `?state=${state}&code=c2`, hash: '' }, history);
  assert.deepEqual(googleCallback(), { state, code: 'c1' });
  assert.equal(history.calls.length, 1);
});

test('a malformed query is removed too, and other screens are left alone', async () => {
  const { captureGoogleCallback, googleCallback } = await fresh();
  const history = fakeHistory();
  captureGoogleCallback({ pathname: '/settings', search: '?code=c', hash: '' }, history);
  assert.deepEqual(history.calls, []);
  assert.equal(googleCallback(), null);
  captureGoogleCallback({ pathname: '/integrations/google/callback/', search: '?code=garbage', hash: '#x' }, history);
  assert.equal(googleCallback(), null);
  assert.deepEqual(history.calls, [[{ idx: 2 }, '', '/integrations/google/callback/#x']]);
});

test('a page opened without a query has no result', async () => {
  const { captureGoogleCallback, googleCallback } = await fresh();
  const history = fakeHistory();
  captureGoogleCallback({ pathname: '/integrations/google/callback', search: '', hash: '' }, history);
  assert.equal(googleCallback(), null);
  assert.deepEqual(history.calls, []);
});

test('the result is completed once however often it is asked', async () => {
  const { completeOnce } = await fresh();
  const value = { state, code: 'c1' };
  let requests = 0;
  const complete = async (v) => { requests++; return { email: 'alice@example.com', code: v.code }; };
  const first = completeOnce(value, complete);
  const second = completeOnce(value, complete);
  assert.equal(first, second);
  assert.deepEqual(await second, { email: 'alice@example.com', code: 'c1' });
  assert.equal(requests, 1);
  // A failure is not retried either.
  const other = { state, error: 'access_denied' };
  let failures = 0;
  const failing = async () => { failures++; throw new Error('denied'); };
  await assert.rejects(completeOnce(other, failing), /denied/);
  await assert.rejects(completeOnce(other, failing), /denied/);
  assert.equal(failures, 1);
});
