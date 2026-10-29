import assert from 'node:assert/strict';
import { test } from 'node:test';

const logoutURL = 'https://example.cloudflareaccess.com/cdn-cgi/access/logout';

async function fresh(fetcher) {
  globalThis.fetch = fetcher;
  return import(`../.test-build/authConfig.js?test=${crypto.randomUUID()}`);
}

test('the sign-in mode is password or Cloudflare Access with its logout URL', async () => {
  const { parseAuthConfig } = await fresh(async () => new Response('{}'));
  assert.deepEqual(parseAuthConfig({ mode: 'password' }), { mode: 'password' });
  assert.deepEqual(parseAuthConfig({ mode: 'cloudflare_access', logout_url: logoutURL }), { mode: 'cloudflare_access', logoutURL });
  for (const body of [
    null, [], 'password', {}, { mode: 'oidc' },
    { mode: 'cloudflare_access' },
    { mode: 'cloudflare_access', logout_url: 1 },
    { mode: 'cloudflare_access', logout_url: 'http://example.cloudflareaccess.com/cdn-cgi/access/logout' },
    { mode: 'cloudflare_access', logout_url: 'https://evil.test/cdn-cgi/access/logout' },
    { mode: 'cloudflare_access', logout_url: 'https://example.cloudflareaccess.com.evil.test/cdn-cgi/access/logout' },
    { mode: 'cloudflare_access', logout_url: 'https://example.cloudflareaccess.com/cdn-cgi/access/logout?next=https://evil.test' },
    { mode: 'cloudflare_access', logout_url: 'https://example.cloudflareaccess.com:8443/cdn-cgi/access/logout' },
    { mode: 'cloudflare_access', logout_url: 'https://user@example.cloudflareaccess.com/cdn-cgi/access/logout' },
    { mode: 'cloudflare_access', logout_url: 'https://example.cloudflareaccess.com/other' },
    { mode: 'cloudflare_access', logout_url: 'javascript:alert(1)' },
  ]) assert.equal(parseAuthConfig(body), null, JSON.stringify(body));
});

test('load reads /auth/config without credentials beyond the origin', async () => {
  const calls = [];
  const { authConfig } = await fresh(async (path, init) => {
    calls.push([path, init]);
    return new Response(JSON.stringify({ mode: 'cloudflare_access', logout_url: logoutURL }));
  });
  assert.deepEqual(authConfig.getSnapshot(), { status: 'loading', config: null });
  let notified = 0;
  authConfig.subscribe(() => { notified += 1; });
  await authConfig.load();
  assert.deepEqual(authConfig.getSnapshot(), { status: 'ready', config: { mode: 'cloudflare_access', logoutURL } });
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
  assert.match(signinErrorMessage('denied'), /Cloudflare Access で.*許可されませんでした/);
  assert.match(signinErrorMessage('unavailable'), /Cloudflare に接続できませんでした.*時間をおいて/);
  assert.match(signinErrorMessage('disabled'), /無効化されています/);
  assert.match(signinErrorMessage('return_too_long'), /元の画面に戻れませんでした/);
  const messages = new Set(['failed', 'denied', 'unavailable', 'disabled', 'return_too_long'].map(signinErrorMessage));
  assert.equal(messages.size, 5);
  for (const reason of [null, '', 'unknown', 'constructor', '__proto__', 'toString']) assert.equal(signinErrorMessage(reason), signinErrorMessage('failed'), String(reason));
});
