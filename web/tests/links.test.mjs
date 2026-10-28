import assert from 'node:assert/strict';
import { test } from 'node:test';

const body = 'A'.repeat(42) + 'Q';
const invite = `inv_${body}`;
const reset = `pwr_${body}`;

async function fresh() {
  return import(`../.test-build/links.js?test=${crypto.randomUUID()}`);
}

test('the link screens are /invite and /reset-password', async () => {
  const { linkPageAt } = await fresh();
  assert.equal(linkPageAt('/invite'), 'invite');
  assert.equal(linkPageAt('/invite/'), 'invite');
  assert.equal(linkPageAt('/reset-password'), 'reset');
  assert.equal(linkPageAt('/reset-password/'), 'reset');
  for (const p of ['/', '/invite//', '/invites', '/invite/x', '/settings/users', '/reset']) assert.equal(linkPageAt(p), null, p);
});

test('tokens are read from the fragment only in their own form', async () => {
  const { tokenFromHash } = await fresh();
  assert.equal(tokenFromHash(`#${invite}`, 'invite'), invite);
  assert.equal(tokenFromHash(invite, 'invite'), invite);
  assert.equal(tokenFromHash(`#${reset}`, 'reset'), reset);
  for (const [hash, page] of [
    [`#${reset}`, 'invite'],
    [`#${invite}`, 'reset'],
    ['', 'invite'],
    ['#', 'invite'],
    [`#${invite}x`, 'invite'],
    [`#${invite.slice(0, -1)}`, 'invite'],
    [`#inv_${'A'.repeat(42)}=`, 'invite'],
    [`#inv_${'A'.repeat(42)}.`, 'invite'],
    [`#ast_${body}`, 'invite'],
    [`#${invite}&x=1`, 'invite'],
    [`#${encodeURIComponent(' ')}${invite}`, 'invite'],
  ]) assert.equal(tokenFromHash(hash, page), null, `${hash} ${page}`);
});

function fakeHistory() {
  const calls = [];
  return { calls, state: { idx: 3 }, replaceState(data, unused, url) { calls.push([data, unused, url]); } };
}

test('the token is captured once and removed from the URL', async () => {
  const { captureLinkToken, linkToken } = await fresh();
  const history = fakeHistory();
  captureLinkToken({ pathname: '/invite', search: '?from=mail', hash: `#${invite}` }, history);
  assert.equal(linkToken('invite'), invite);
  assert.equal(linkToken('reset'), null);
  assert.deepEqual(history.calls, [[{ idx: 3 }, '', '/invite?from=mail']]);
  // Later calls, for example after the URL lost its fragment, keep it.
  captureLinkToken({ pathname: '/invite', search: '', hash: '' }, history);
  captureLinkToken({ pathname: '/reset-password', search: '', hash: `#${reset}` }, history);
  assert.equal(linkToken('invite'), invite);
  assert.equal(linkToken('reset'), null);
  assert.equal(history.calls.length, 1);
});

test('a malformed fragment is removed too, and other screens are left alone', async () => {
  const { captureLinkToken, linkToken } = await fresh();
  const history = fakeHistory();
  captureLinkToken({ pathname: '/settings', search: '', hash: '#tokens' }, history);
  assert.deepEqual(history.calls, []);
  captureLinkToken({ pathname: '/reset-password/', search: '', hash: '#garbage' }, history);
  assert.equal(linkToken('reset'), null);
  assert.deepEqual(history.calls, [[{ idx: 3 }, '', '/reset-password/']]);
});

test('failures are classified by status and code', async () => {
  const { linkFailure } = await fresh();
  const failure = (status, code = null) => Object.assign(new Error('x'), { status, code });
  assert.equal(linkFailure(failure(404, 'invitation_invalid')), 'invalid');
  assert.equal(linkFailure(failure(404, 'reset_invalid')), 'invalid');
  assert.equal(linkFailure(failure(404, 'not_found')), 'network');
  assert.equal(linkFailure(failure(409, 'email_taken')), 'email_taken');
  assert.equal(linkFailure(failure(429, 'rate_limited')), 'rate_limited');
  assert.equal(linkFailure(failure(400, 'invalid_request')), 'password');
  assert.equal(linkFailure(failure(503, 'unavailable')), 'unavailable');
  assert.equal(linkFailure(new TypeError('Failed to fetch')), 'network');
  assert.equal(linkFailure('x'), 'network');
});

test('new passwords are 12-72 bytes and confirmed', async () => {
  const { passwordProblem } = await fresh();
  assert.equal(passwordProblem('correct-pass', 'correct-pass'), null);
  assert.equal(passwordProblem('x'.repeat(72), 'x'.repeat(72)), null);
  assert.match(passwordProblem('', ''), /2回入力/);
  assert.match(passwordProblem('correct-pass', ''), /2回入力/);
  assert.match(passwordProblem('short-pass', 'short-pass'), /12〜72バイト/);
  assert.match(passwordProblem('x'.repeat(73), 'x'.repeat(73)), /12〜72バイト/);
  // Bytes, not characters: 25 three-byte characters are 75 bytes.
  assert.match(passwordProblem('あ'.repeat(25), 'あ'.repeat(25)), /12〜72バイト/);
  assert.equal(passwordProblem('あ'.repeat(4), 'あ'.repeat(4)), null);
  assert.match(passwordProblem('correct-pass', 'correct-pasS'), /一致しません/);
});
