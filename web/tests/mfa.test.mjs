import assert from 'node:assert/strict';
import { test } from 'node:test';
import { createMFAClient, credentialName, mfaEnabled, mfaErrorMessage, qrModules, qrPath, qrQuietZone, qrSize, readMFAStatus, readRecoveryCodes, readTOTPSetup, readWebAuthnBegin } from '../.test-build/mfa.js';

const setup = {
  secret: 'JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP',
  otpauth_url: 'otpauth://totp/Astovia%20%28astovia.test%29:alice@example.com?algorithm=SHA1&digits=6&issuer=Astovia%20%28astovia.test%29&period=30&secret=JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP',
  setup_token: 'v1.setup-token',
};
const key = { id: 'key-1', name: 'Laptop', created_at: '2026-10-23T01:02:03Z', last_used_at: null };
const status = { required: false, totp: { created_at: '2026-10-23T01:02:03Z' }, webauthn: [key], recovery_codes_remaining: 10, webauthn_available: true };
const codes = Array.from({ length: 10 }, (_, i) => `abcd-efgh-jkmn-pqr${i}`);
const begin = { options: { challenge: 'AQID', rpId: 'astovia.test' }, state: 'v1.state' };

test('the setup of an authenticator app is checked', () => {
  assert.deepEqual(readTOTPSetup({ ...setup, extra: 1 }), setup);
  const { setup_token: _token, ...withoutToken } = setup;
  for (const bad of [null, [], {}, withoutToken, { ...setup, secret: '' }, { ...setup, secret: 'not base32!' }, { ...setup, secret: 'jbswy3dpehpk3pxp' },
    { ...setup, otpauth_url: 'https://example.com/' }, { ...setup, otpauth_url: 'otpauth://hotp/x' }, { ...setup, otpauth_url: null }, { ...setup, setup_token: '' }, { ...setup, setup_token: 1 }]) {
    assert.throws(() => readTOTPSetup(bad), /Unexpected TOTP setup response/, JSON.stringify(bad));
  }
});

test('the start of a WebAuthn ceremony is checked', () => {
  assert.deepEqual(readWebAuthnBegin(begin), begin);
  for (const bad of [null, {}, { options: begin.options }, { ...begin, state: '' }, { ...begin, options: [] }, { ...begin, options: {} }, { ...begin, options: { challenge: 1 } }, { state: 'v1.state', options: 'x' }]) {
    assert.throws(() => readWebAuthnBegin(bad), /Unexpected WebAuthn response/, JSON.stringify(bad));
  }
});

test('recovery codes are a list of codes, or null only where allowed', () => {
  assert.deepEqual(readRecoveryCodes({ recovery_codes: codes }, false), codes);
  assert.deepEqual(readRecoveryCodes({ recovery_codes: codes }, true), codes);
  assert.equal(readRecoveryCodes({ recovery_codes: null }, true), null);
  assert.throws(() => readRecoveryCodes({ recovery_codes: null }, false));
  for (const bad of [null, {}, { recovery_codes: [] }, { recovery_codes: ['a', ''] }, { recovery_codes: ['a', 1] }, { recovery_codes: 'abcd' }, { codes }]) {
    assert.throws(() => readRecoveryCodes(bad, true), /Unexpected recovery codes response/, JSON.stringify(bad));
  }
});

test('the status is checked and tells whether a factor is enrolled', () => {
  assert.deepEqual(readMFAStatus(status), status);
  const none = { required: true, totp: null, webauthn: [], recovery_codes_remaining: 0, webauthn_available: false };
  assert.deepEqual(readMFAStatus(none), none);
  assert.equal(mfaEnabled(readMFAStatus(status)), true);
  assert.equal(mfaEnabled(readMFAStatus({ ...status, webauthn: [] })), true);
  assert.equal(mfaEnabled(readMFAStatus({ ...status, totp: null })), true);
  // Recovery codes alone are no factor.
  assert.equal(mfaEnabled(readMFAStatus({ ...none, recovery_codes_remaining: 3 })), false);
  assert.equal(readMFAStatus({ ...status, webauthn: [{ ...key, last_used_at: '2026-10-24T00:00:00Z' }] }).webauthn[0].last_used_at, '2026-10-24T00:00:00Z');
  const { webauthn_available: _available, ...withoutAvailable } = status;
  const { totp: _totp, ...withoutTOTP } = status;
  for (const bad of [null, [], {}, withoutAvailable, withoutTOTP, { ...status, required: 'yes' }, { ...status, totp: {} }, { ...status, totp: { created_at: 1 } }, { ...status, totp: true },
    { ...status, webauthn: null }, { ...status, webauthn: [{ ...key, id: '' }] }, { ...status, webauthn: [{ ...key, last_used_at: 1 }] }, { ...status, webauthn: [{ id: 'x' }] },
    { ...status, recovery_codes_remaining: '10' }, { ...status, recovery_codes_remaining: -1 }, { ...status, recovery_codes_remaining: 1.5 }]) {
    assert.throws(() => readMFAStatus(bad), /Unexpected MFA status response/, JSON.stringify(bad));
  }
});

test('the client sends the requests and checks the responses', async () => {
  const sent = [];
  const responses = {
    'GET /api/v1/mfa': status,
    'POST /api/v1/mfa/totp/begin': setup,
    'POST /api/v1/mfa/totp/finish': { recovery_codes: codes },
    'POST /api/v1/mfa/totp/delete': undefined,
    'POST /api/v1/mfa/webauthn/begin': begin,
    'POST /api/v1/mfa/webauthn/finish': { credential: key, recovery_codes: null },
    'POST /api/v1/mfa/webauthn/a%2Fb/delete': undefined,
    'POST /api/v1/mfa/recovery-codes': { recovery_codes: codes },
  };
  const client = createMFAClient(async (path, init) => {
    const name = `${init?.method ?? 'GET'} ${path}`;
    sent.push([name, init?.body ? JSON.parse(init.body) : null, init ? new Headers(init.headers).get('content-type') : null]);
    assert.ok(name in responses, name);
    return responses[name];
  });
  assert.deepEqual(await client.status(), status);
  assert.deepEqual(await client.beginTOTP('pw'), setup);
  assert.deepEqual(await client.finishTOTP('v1.setup-token', '123 456'), codes);
  assert.equal(await client.deleteTOTP('pw'), undefined);
  assert.deepEqual(await client.beginWebAuthn('pw'), begin);
  assert.deepEqual(await client.finishWebAuthn('v1.state', { id: 'AQID' }, 'Laptop'), { credential: key, recoveryCodes: null });
  assert.equal(await client.deleteWebAuthn('a/b', 'pw'), undefined);
  assert.deepEqual(await client.reissueRecoveryCodes('pw'), codes);
  assert.deepEqual(sent, [
    ['GET /api/v1/mfa', null, null],
    ['POST /api/v1/mfa/totp/begin', { password: 'pw' }, 'application/json'],
    ['POST /api/v1/mfa/totp/finish', { setup_token: 'v1.setup-token', code: '123 456' }, 'application/json'],
    ['POST /api/v1/mfa/totp/delete', { password: 'pw' }, 'application/json'],
    ['POST /api/v1/mfa/webauthn/begin', { password: 'pw' }, 'application/json'],
    ['POST /api/v1/mfa/webauthn/finish', { state: 'v1.state', credential: { id: 'AQID' }, name: 'Laptop' }, 'application/json'],
    ['POST /api/v1/mfa/webauthn/a%2Fb/delete', { password: 'pw' }, 'application/json'],
    ['POST /api/v1/mfa/recovery-codes', { password: 'pw' }, 'application/json'],
  ]);
});

test('the client rejects unexpected responses and passes errors through', async () => {
  const reply = (data) => createMFAClient(async () => data);
  // Replacing an authenticator app issues no codes; the first one does.
  assert.equal(await reply({ recovery_codes: null }).finishTOTP('t', '123456'), null);
  await assert.rejects(reply({}).finishTOTP('t', '123456'));
  await assert.rejects(reply({ recovery_codes: null }).reissueRecoveryCodes('pw'));
  await assert.rejects(reply({ recovery_codes: codes }).finishWebAuthn('s', {}, 'n'), /Unexpected WebAuthn response/);
  await assert.rejects(reply({ credential: key }).finishWebAuthn('s', {}, 'n'), /Unexpected recovery codes response/);
  assert.deepEqual(await reply({ credential: key, recovery_codes: codes }).finishWebAuthn('s', {}, 'n'), { credential: key, recoveryCodes: codes });
  await assert.rejects(reply({ ...status, webauthn: [{}] }).status());
  await assert.rejects(reply({ secret: 'x' }).beginTOTP('pw'));
  await assert.rejects(reply({ state: 'x' }).beginWebAuthn('pw'));
  const failure = Object.assign(new Error('Request failed'), { status: 400, code: 'password_invalid' });
  await assert.rejects(createMFAClient(async () => { throw failure; }).deleteTOTP('pw'), (error) => error === failure);
});

test('errors get a message per code', () => {
  const failure = (status, code = null) => Object.assign(new Error('x'), { status, code });
  assert.equal(mfaErrorMessage(failure(401, 'mfa_code_invalid')), 'コードが正しくありません。');
  assert.equal(mfaErrorMessage(failure(400, 'mfa_code_invalid'), 'code'), 'コードが正しくありません。');
  assert.match(mfaErrorMessage(failure(401, 'mfa_code_invalid'), 'key'), /パスキー・セキュリティキーを確認できませんでした/);
  assert.equal(mfaErrorMessage(failure(400, 'password_invalid'), 'password'), 'パスワードが正しくありません。');
  assert.equal(mfaErrorMessage(failure(409, 'mfa_required'), 'password'), 'この Astovia では二段階認証が必要なため、最後の方法は削除できません。');
  assert.equal(mfaErrorMessage(failure(400, 'mfa_state_invalid')), '時間が切れました。最初からやり直してください。');
  assert.equal(mfaErrorMessage(failure(409, 'mfa_limit'), 'key'), '登録できるのは 10 個までです。');
  assert.equal(mfaErrorMessage(failure(409, 'mfa_credential_exists'), 'key'), 'このパスキーは登録済みです。');
  assert.equal(mfaErrorMessage(failure(401, 'mfa_token_invalid')), '時間が切れました。もう一度ログインしてください。');
  assert.match(mfaErrorMessage(failure(409, 'mfa_already_enrolled')), /登録済み/);
  assert.match(mfaErrorMessage(failure(403, 'mfa_not_required')), /ログイン後/);
  assert.match(mfaErrorMessage(failure(409, 'mfa_not_enrolled'), 'password'), /発行できません/);
  assert.match(mfaErrorMessage(failure(400, 'webauthn_unavailable'), 'password'), /アドレスでは/);
  // The attempts at a password are limited for 5 minutes, those at a code for longer.
  assert.equal(mfaErrorMessage(failure(429, 'rate_limited')), '試行回数が上限に達しました。しばらくしてから、もう一度お試しください。');
  assert.equal(mfaErrorMessage(failure(429, 'rate_limited'), 'key'), '試行回数が上限に達しました。しばらくしてから、もう一度お試しください。');
  assert.equal(mfaErrorMessage(failure(429, 'rate_limited'), 'password'), '試行回数が上限に達しました。5分ほどしてから、もう一度お試しください。');
  assert.match(mfaErrorMessage(failure(404, 'not_found'), 'password'), /見つかりません/);
  assert.match(mfaErrorMessage(failure(400, 'invalid_request')), /入力内容/);
  assert.match(mfaErrorMessage(failure(503, 'unavailable')), /通信に失敗/);
  assert.match(mfaErrorMessage(new TypeError('Failed to fetch')), /通信に失敗/);
  assert.match(mfaErrorMessage(null), /通信に失敗/);
});

test('the name of a passkey is trimmed and has 1-64 characters', () => {
  assert.equal(credentialName('  Laptop  '), 'Laptop');
  assert.equal(credentialName('ノートパソコン'), 'ノートパソコン');
  assert.equal(credentialName('a'.repeat(64)), 'a'.repeat(64));
  // Characters are counted, not UTF-16 units.
  assert.equal(credentialName('😀'.repeat(64)), '😀'.repeat(64));
  for (const bad of ['', '   ', 'a'.repeat(65), '😀'.repeat(65), 'a\nb', 'a\u0000b', 'a\u007fb']) assert.equal(credentialName(bad), null, JSON.stringify(bad));
});

// finder checks the 7x7 finder pattern whose top left module is at (x, y).
function finder(modules, x, y) {
  for (let dy = 0; dy < 7; dy += 1) {
    for (let dx = 0; dx < 7; dx += 1) {
      const ring = dx === 0 || dx === 6 || dy === 0 || dy === 6;
      const core = dx >= 2 && dx <= 4 && dy >= 2 && dy <= 4;
      assert.equal(modules[y + dy][x + dx], ring || core, `finder at ${x},${y}: ${dx},${dy}`);
    }
  }
}

test('the QR code of an otpauth URL is a square of modules with its finder patterns', () => {
  const modules = qrModules(setup.otpauth_url);
  const n = modules.length;
  assert.ok(n >= 21 && (n - 17) % 4 === 0, `size ${n}`);
  for (const row of modules) {
    assert.equal(row.length, n);
    for (const cell of row) assert.equal(typeof cell, 'boolean');
  }
  finder(modules, 0, 0);
  finder(modules, n - 7, 0);
  finder(modules, 0, n - 7);
  // The same text gives the same code, another text another one.
  assert.deepEqual(qrModules(setup.otpauth_url), modules);
  assert.notDeepEqual(qrModules(`${setup.otpauth_url}X`), modules);
  assert.equal(qrSize(modules), n + 2 * qrQuietZone);
  assert.ok(qrQuietZone >= 4);
  // A text that fits no QR code gives none instead of throwing.
  assert.equal(qrModules('x'.repeat(5000)), null);
});

test('the QR path draws every dark module once, inside the quiet zone', () => {
  assert.equal(qrPath([[true, false], [false, true]]), 'M4 4h1v1h-1zM5 5h1v1h-1z');
  // Horizontal runs are one rectangle.
  assert.equal(qrPath([[true, true, false, true], [false, false, false, false], [true, true, true, true]]), 'M4 4h2v1h-2zM7 4h1v1h-1zM4 6h4v1h-4z');
  assert.equal(qrPath([]), '');
  const modules = qrModules(setup.otpauth_url);
  const size = qrSize(modules);
  const drawn = Array.from({ length: size }, () => new Array(size).fill(0));
  for (const [, x, y, w] of qrPath(modules).matchAll(/M(\d+) (\d+)h(\d+)v1h-\d+z/g)) {
    for (let i = 0; i < Number(w); i += 1) drawn[Number(y)][Number(x) + i] += 1;
  }
  for (let y = 0; y < size; y += 1) {
    for (let x = 0; x < size; x += 1) {
      const inside = x >= qrQuietZone && y >= qrQuietZone && x < size - qrQuietZone && y < size - qrQuietZone;
      const dark = inside && modules[y - qrQuietZone][x - qrQuietZone];
      assert.equal(drawn[y][x], dark ? 1 : 0, `${x},${y}`);
    }
  }
});
