import assert from 'node:assert/strict';
import { test } from 'node:test';

async function fresh(fetcher) {
  globalThis.fetch = fetcher;
  return import(`../.test-build/authConfig.js?test=${crypto.randomUUID()}`);
}

test('the sign-in mode is password or oidc', async () => {
  const { parseAuthConfig } = await fresh(async () => new Response('{}'));
  assert.deepEqual(parseAuthConfig({ mode: 'password' }), { mode: 'password' });
  assert.deepEqual(parseAuthConfig({ mode: 'oidc' }), { mode: 'oidc' });
  // Extra members are ignored.
  assert.deepEqual(parseAuthConfig({ mode: 'oidc', logout_url: 'https://evil.test/' }), { mode: 'oidc' });
  for (const body of [
    null, [], 'password', 'oidc', {}, { mode: 'OIDC' }, { mode: 1 },
    // The mode of earlier releases is no longer known.
    { mode: 'cloudflare_access' },
    { mode: 'cloudflare_access', logout_url: 'https://example.cloudflareaccess.com/cdn-cgi/access/logout' },
  ]) assert.equal(parseAuthConfig(body), null, JSON.stringify(body));
});

test('load reads /auth/config without credentials beyond the origin', async () => {
  const calls = [];
  const { authConfig } = await fresh(async (path, init) => {
    calls.push([path, init]);
    return new Response(JSON.stringify({ mode: 'oidc' }));
  });
  assert.deepEqual(authConfig.getSnapshot(), { status: 'loading', config: null });
  let notified = 0;
  authConfig.subscribe(() => { notified += 1; });
  await authConfig.load();
  assert.deepEqual(authConfig.getSnapshot(), { status: 'ready', config: { mode: 'oidc' } });
  assert.deepEqual(calls, [['/auth/config', { credentials: 'same-origin', cache: 'no-store' }]]);
  assert.ok(notified >= 1);
});

test('a failed load is not taken for password sign-in', async () => {
  for (const fetcher of [
    async () => { throw new TypeError('offline'); },
    async () => new Response('{}', { status: 503 }),
    async () => new Response('<html>'),
    async () => new Response(JSON.stringify({ mode: 'unknown' })),
  ]) {
    const { authConfig } = await fresh(fetcher);
    await authConfig.load();
    assert.deepEqual(authConfig.getSnapshot(), { status: 'failed', config: null });
  }
});

test('the sign-in error screen explains each reason', async () => {
  const { isSigninErrorPath, signinErrorMessage, signinReason } = await fresh(async () => new Response('{}'));
  assert.equal(isSigninErrorPath('/signin-error'), true);
  assert.equal(isSigninErrorPath('/signin-error/'), true);
  for (const p of ['/', '/signin-error//', '/signin-errors', '/signin-error/x']) assert.equal(isSigninErrorPath(p), false, p);

  assert.equal(signinReason('?reason=denied'), 'denied');
  assert.equal(signinReason(''), null);
  assert.equal(signinReason('?reason=a&reason=b'), null);

  assert.match(signinErrorMessage('failed'), /ログインできませんでした/);
  assert.match(signinErrorMessage('denied'), /ログインが許可されませんでした.*ログインサービスからログアウト/);
  assert.equal(signinErrorMessage('not_allowed'), 'このアカウントではログインできません。招待されていないか、別のアカウントで登録されています。この Astovia のオーナーに確認してください。');
  assert.match(signinErrorMessage('email_not_verified'), /メールアドレスが確認済みでないため/);
  assert.equal(signinErrorMessage('unsupported_account'), 'この Google アカウントではログインできません。@gmail.com のアドレスか Google Workspace のアカウントでログインしてください。');
  assert.match(signinErrorMessage('unavailable'), /ログインサービスに接続できませんでした.*しばらくしてから/);
  assert.match(signinErrorMessage('disabled'), /無効化されています/);
  assert.match(signinErrorMessage('return_too_long'), /元の画面に戻れませんでした/);
  const messages = new Set(['failed', 'denied', 'not_allowed', 'email_not_verified', 'unsupported_account', 'unavailable', 'disabled', 'return_too_long'].map(signinErrorMessage));
  assert.equal(messages.size, 8);
  for (const reason of [null, '', 'unknown', 'constructor', '__proto__', 'toString']) assert.equal(signinErrorMessage(reason), signinErrorMessage('failed'), String(reason));
});
