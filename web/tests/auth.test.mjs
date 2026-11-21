import assert from 'node:assert/strict';
import { test } from 'node:test';

const user = { id: 'test-user', email: 'test-user@example.com', role: 'owner', password_reset: null };
const session = { access_token: 'test-access-token', expires_in: 600, user };
const json = (body, status = 200) => new Response(JSON.stringify(body), { status });
function deferred() {
  let resolve;
  const promise = new Promise((complete) => { resolve = complete; });
  return { promise, resolve };
}
async function until(predicate) {
  for (let i = 0; i < 50; i += 1) {
    if (predicate()) return;
    await new Promise((resolve) => setImmediate(resolve));
  }
  assert.fail('Expected state was not reached');
}

async function harness(fetcher) {
  const locks = [];
  let tail = Promise.resolve();
  Object.defineProperty(globalThis, 'navigator', { configurable: true, value: {
    locks: { request(name, operation) {
      locks.push(name);
      const next = tail.then(operation);
      tail = next.catch(() => {});
      return next;
    } },
  } });
  const messages = [];
  let receiver;
  globalThis.BroadcastChannel = class {
    addEventListener(_name, callback) { receiver = callback; }
    postMessage(value) { messages.push(value); }
  };
  globalThis.fetch = fetcher;
  const module = await import(`../.test-build/auth.js?test=${crypto.randomUUID()}`);
  return { ...module, locks, messages, receive: (data) => receiver({ data }) };
}

test('startup performs one refresh and treats 401 as logged out', async () => {
  const calls = [];
  const { auth, locks } = await harness(async (path) => { calls.push(path); return json({}, 401); });
  auth.initialize();
  auth.initialize();
  await until(() => auth.getSnapshot().status === 'anonymous');
  assert.deepEqual(calls, ['/auth/refresh']);
  assert.deepEqual(locks, ['astovia-auth']);
  assert.equal(auth.getSnapshot().error, null);
});

test('concurrent private requests share one refresh and each retry only once', async () => {
  let privateCalls = 0;
  let refreshCalls = 0;
  const rotation = deferred();
  const { auth, request } = await harness(async (path) => {
    if (path === '/auth/login') return json(session);
    if (path === '/api/v1/me') return json(user);
    if (path === '/auth/refresh') { refreshCalls += 1; return rotation.promise; }
    privateCalls += 1;
    return privateCalls <= 2 ? json({}, 401) : json({ ok: true });
  });
  await auth.login('test-user@example.com', 'test-password');
  const first = request('/api/v1/test');
  const second = request('/api/v1/test');
  await until(() => refreshCalls === 1);
  rotation.resolve(json(session));
  assert.deepEqual(await Promise.all([first, second]), [{ ok: true }, { ok: true }]);
  assert.equal(refreshCalls, 1);
  assert.equal(privateCalls, 4);
});

test('a delayed refresh cannot undo logout and auth endpoints share the lock', async () => {
  const rotation = deferred();
  let refreshing = false;
  const { auth, request, locks, messages } = await harness(async (path) => {
    if (path === '/auth/login') return json(session);
    if (path === '/api/v1/me') return json(user);
    if (path === '/auth/refresh') { refreshing = true; return rotation.promise; }
    if (path === '/auth/logout') return new Response(null, { status: 204 });
    return json({}, 401);
  });
  await auth.login('test-user@example.com', 'test-password');
  const pending = request('/api/v1/test').catch(() => null);
  await until(() => refreshing);
  const logout = auth.logout();
  rotation.resolve(json(session));
  await Promise.all([pending, logout]);
  assert.equal(auth.getSnapshot().status, 'anonymous');
  assert.equal(auth.getSnapshot().user, null);
  assert.deepEqual(locks, ['astovia-auth', 'astovia-auth', 'astovia-auth']);
  assert.equal(messages.at(-1), 'logout');
});

test('failed logout keeps identity and gives a retry error', async () => {
  const { auth, messages } = await harness(async (path) => {
    if (path === '/auth/login') return json(session);
    if (path === '/api/v1/me') return json(user);
    return json({}, 503);
  });
  await auth.login('test-user@example.com', 'test-password');
  await auth.logout();
  assert.equal(auth.getSnapshot().status, 'authenticated');
  assert.deepEqual(auth.getSnapshot().user, user);
  assert.match(auth.getSnapshot().error, /ログアウトを完了できません/);
  assert.equal(messages.includes('logout'), false);
});

test('cross-tab logout invalidates an in-flight startup refresh', async () => {
  const rotation = deferred();
  let refreshing = false;
  const { auth, receive } = await harness(async () => { refreshing = true; return rotation.promise; });
  auth.initialize();
  await until(() => refreshing);
  receive('logout');
  rotation.resolve(json(session));
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(auth.getSnapshot().status, 'anonymous');
  assert.equal(auth.getSnapshot().user, null);
});

test('a second 401 does not create a refresh loop', async () => {
  let refreshCalls = 0;
  let privateCalls = 0;
  const { auth, request } = await harness(async (path) => {
    if (path === '/auth/login') return json(session);
    if (path === '/api/v1/me') return json(user);
    if (path === '/auth/refresh') { refreshCalls += 1; return json(session); }
    privateCalls += 1;
    return json({}, 401);
  });
  await auth.login('test-user@example.com', 'test-password');
  await assert.rejects(request('/api/v1/test'));
  assert.equal(refreshCalls, 1);
  assert.equal(privateCalls, 2);
  assert.equal(auth.getSnapshot().status, 'anonymous');
});

test('login failures map status codes to messages', async () => {
  for (const [status, check] of [
    [401, (error) => assert.equal(error, 'メールアドレスまたはパスワードを確認してください。')],
    [429, (error) => assert.ok(error.startsWith('試行回数が上限に達しました'), error)],
    [403, (error) => assert.ok(error.startsWith('接続先の設定を確認できませんでした'), error)],
    [500, (error) => assert.ok(error.startsWith('サーバーに接続できませんでした'), error)],
  ]) {
    const { auth } = await harness(async () => json({ error: { code: 'x', message: 'x' } }, status));
    await auth.login('test-user@example.com', 'test-password');
    assert.equal(auth.getSnapshot().status, 'anonymous', `status ${status}`);
    assert.equal(auth.getSnapshot().user, null);
    check(auth.getSnapshot().error);
  }
});

test('malformed login responses do not authenticate', async () => {
  const { user: _omitted, ...withoutUser } = session;
  const usernameOnly = { ...session, user: { id: user.id, username: 'test-user' } };
  for (const body of [{ ...session, expires_in: 0 }, withoutUser, usernameOnly]) {
    const calls = [];
    const { auth } = await harness(async (path) => {
      calls.push(path);
      if (path === '/auth/login') return json(body);
      return json(user);
    });
    await auth.login('test-user@example.com', 'test-password');
    assert.notEqual(auth.getSnapshot().status, 'authenticated');
    assert.equal(auth.getSnapshot().user, null);
    assert.deepEqual(calls, ['/auth/login']);
  }
});

test('login sends the email and password only', async () => {
  const bodies = [];
  const { auth } = await harness(async (path, init) => {
    if (path === '/auth/login') {
      bodies.push(JSON.parse(init.body));
      return json(session);
    }
    if (path === '/api/v1/me') return json(user);
    return json({}, 404);
  });
  await auth.login('test-user@example.com', 'test-password');
  assert.deepEqual(bodies, [{ email: 'test-user@example.com', password: 'test-password' }]);
  assert.equal('username' in bodies[0], false);
  assert.equal(auth.getSnapshot().status, 'authenticated');
});

test('a profile response without email is rejected', async () => {
  const { auth } = await harness(async (path) => {
    if (path === '/auth/login') return json(session);
    if (path === '/api/v1/me') return json({ id: user.id, username: 'test-user' });
    return json({}, 404);
  });
  await auth.login('test-user@example.com', 'test-password');
  assert.deepEqual(auth.getSnapshot().user, user);
  assert.ok(auth.getSnapshot().error?.startsWith('アカウント情報を確認できませんでした'), auth.getSnapshot().error);
});

test('session-changed from another tab triggers a refresh', async () => {
  const calls = [];
  const { auth, receive } = await harness(async (path) => {
    calls.push(path);
    if (path === '/auth/refresh') return json(session);
    if (path === '/api/v1/me') return json(user);
    return json({}, 404);
  });
  receive('session-changed');
  await until(() => auth.getSnapshot().status === 'authenticated');
  assert.equal(calls[0], '/auth/refresh');
  assert.deepEqual(auth.getSnapshot().user, user);
});

test('request sends the access token as a Bearer header', async () => {
  const headers = [];
  const { auth, request } = await harness(async (path, init) => {
    if (path === '/auth/login') return json(session);
    headers.push([path, init.headers.Authorization]);
    if (path === '/api/v1/me') return json(user);
    return json({ ok: true });
  });
  await auth.login('test-user@example.com', 'test-password');
  assert.deepEqual(await request('/api/v1/test'), { ok: true });
  assert.deepEqual(headers, [
    ['/api/v1/me', 'Bearer test-access-token'],
    ['/api/v1/test', 'Bearer test-access-token'],
  ]);
});

test('request keeps the method, body and content type of a POST', async () => {
  const sent = [];
  const { auth, request } = await harness(async (path, init) => {
    if (path === '/auth/login') return json(session);
    if (path === '/api/v1/me') return json(user);
    sent.push([path, init]);
    return json({ ok: true }, 201);
  });
  await auth.login('test-user@example.com', 'test-password');
  const body = JSON.stringify({ content: 'x' });
  assert.deepEqual(await request('/api/v1/action-items', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body }), { ok: true });
  assert.equal(sent.length, 1);
  const [path, init] = sent[0];
  const headers = new Headers(init.headers);
  assert.equal(path, '/api/v1/action-items');
  assert.equal(init.method, 'POST');
  assert.equal(init.body, body);
  assert.equal(headers.get('content-type'), 'application/json');
  assert.equal(headers.get('authorization'), 'Bearer test-access-token');
});

test('request failures carry the error code of the JSON body', async () => {
  const { auth, request, APIError } = await harness(async (path) => {
    if (path === '/auth/login') return json(session);
    if (path === '/api/v1/me') return json(user);
    if (path === '/api/v1/coded') return json({ error: { code: 'github_token_invalid', message: 'x' } }, 400);
    return new Response('not json', { status: 503 });
  });
  await auth.login('test-user@example.com', 'test-password');
  await assert.rejects(request('/api/v1/coded'), (error) => error instanceof APIError && error.status === 400 && error.code === 'github_token_invalid');
  await assert.rejects(request('/api/v1/plain'), (error) => error instanceof APIError && error.status === 503 && error.code === null);
});

test('the user must carry a valid role and password reset notice', async () => {
  const { isUser } = await harness(async () => json({}, 404));
  assert.equal(isUser(user), true);
  assert.equal(isUser({ ...user, role: 'member' }), true);
  assert.equal(isUser({ ...user, password_reset: { at: '2026-10-23T01:02:03Z', by_email: 'owner@example.com' } }), true);
  const { role: _role, ...withoutRole } = user;
  const { password_reset: _notice, ...withoutNotice } = user;
  for (const bad of [
    withoutRole,
    { ...user, role: 'admin' },
    { ...user, role: 'Owner' },
    { ...user, role: null },
    withoutNotice,
    { ...user, password_reset: {} },
    { ...user, password_reset: { at: '2026-10-23T01:02:03Z' } },
    { ...user, password_reset: { at: 1, by_email: 'owner@example.com' } },
    { ...user, password_reset: 'yes' },
    { ...user, email: null },
    null,
    [],
  ]) assert.equal(isUser(bad), false, JSON.stringify(bad));
});

test('a login response without a valid role does not authenticate', async () => {
  for (const body of [{ ...session, user: { ...user, role: 'admin' } }, { ...session, user: { id: user.id, email: user.email, password_reset: null } }]) {
    const calls = [];
    const { auth } = await harness(async (path) => {
      calls.push(path);
      if (path === '/auth/login') return json(body);
      return json(user);
    });
    await auth.login('test-user@example.com', 'test-password');
    assert.notEqual(auth.getSnapshot().status, 'authenticated');
    assert.deepEqual(calls, ['/auth/login']);
  }
});

test('a profile without a valid role is rejected', async () => {
  const { auth } = await harness(async (path) => {
    if (path === '/auth/login') return json(session);
    if (path === '/api/v1/me') return json({ ...user, role: 'superuser' });
    return json({}, 404);
  });
  await auth.login('test-user@example.com', 'test-password');
  assert.deepEqual(auth.getSnapshot().user, user);
  assert.ok(auth.getSnapshot().error?.startsWith('アカウント情報を確認できませんでした'), auth.getSnapshot().error);
});

test('completeLink signs in like login under the same lock and tells other tabs', async () => {
  const sent = [];
  const member = { ...user, role: 'member', password_reset: { at: '2026-10-23T01:02:03Z', by_email: 'owner@example.com' } };
  const { auth, locks, messages } = await harness(async (path, init) => {
    sent.push([path, init?.body ? JSON.parse(init.body) : null]);
    if (path === '/auth/password-resets/complete') return json({ ...session, user: member });
    if (path === '/api/v1/me') return json(member);
    return json({}, 404);
  });
  await auth.completeLink('/auth/password-resets/complete', 'pwr_token', 'new-password-123');
  assert.equal(auth.getSnapshot().status, 'authenticated');
  assert.deepEqual(auth.getSnapshot().user, member);
  assert.deepEqual(sent[0], ['/auth/password-resets/complete', { token: 'pwr_token', password: 'new-password-123' }]);
  assert.deepEqual(locks, ['astovia-auth']);
  assert.deepEqual(messages, ['session-changed']);
});

test('completeLink failures carry the error code and leave the tab signed out', async () => {
  const { auth, APIError, messages } = await harness(async (path) => {
    if (path === '/auth/invitations/accept') return json({ error: { code: 'email_taken', message: 'x' } }, 409);
    return json({}, 404);
  });
  await assert.rejects(auth.completeLink('/auth/invitations/accept', 'inv_token', 'new-password-123'),
    (error) => error instanceof APIError && error.status === 409 && error.code === 'email_taken');
  assert.equal(auth.getSnapshot().status, 'anonymous');
  assert.equal(auth.getSnapshot().busy, null);
  assert.deepEqual(messages, []);
});

test('inspectLink returns the body and throws coded errors without touching the state', async () => {
  const { auth, inspectLink, APIError } = await harness(async (path, init) => {
    const body = JSON.parse(init.body);
    if (path === '/auth/invitations/inspect' && body.token === 'good') return json({ email: 'a@example.com', role: 'member', expires_at: 'x' });
    return json({ error: { code: 'invitation_invalid', message: 'x' } }, 404);
  });
  assert.deepEqual(await inspectLink('/auth/invitations/inspect', 'good'), { email: 'a@example.com', role: 'member', expires_at: 'x' });
  await assert.rejects(inspectLink('/auth/invitations/inspect', 'bad'), (error) => error instanceof APIError && error.status === 404 && error.code === 'invitation_invalid');
  assert.equal(auth.getSnapshot().status, 'loading');
});

// browser installs sessionStorage and location for the Access sign-in.
function browser({ storage = true } = {}) {
  const items = new Map();
  const assigned = [];
  Object.defineProperty(globalThis, 'sessionStorage', { configurable: true, get() {
    if (!storage) throw new Error('SecurityError');
    return {
      getItem: (key) => (items.has(key) ? items.get(key) : null),
      setItem: (key, value) => { items.set(key, String(value)); },
      removeItem: (key) => { items.delete(key); },
    };
  } });
  Object.defineProperty(globalThis, 'location', { configurable: true, value: { assign: (url) => { assigned.push(url); } } });
  return { items, assigned };
}

test('the OIDC sign-in returns to the encoded path and query', async () => {
  const { oidcLoginURL } = await harness(async () => json({}, 401));
  assert.equal(oidcLoginURL('/', ''), '/auth/oidc/login?return_to=%2F');
  assert.equal(oidcLoginURL('/consent', '?request=a%2Fb&x=1'), '/auth/oidc/login?return_to=%2Fconsent%3Frequest%3Da%252Fb%26x%3D1');
  assert.equal(oidcLoginURL('/tasks/abc', ''), '/auth/oidc/login?return_to=%2Ftasks%2Fabc');
});

test('startOIDCLogin marks the tab and leaves with location.assign', async () => {
  const { items, assigned } = browser();
  const { startOIDCLogin } = await harness(async () => json({}, 401));
  startOIDCLogin('/settings', '?x=1');
  assert.deepEqual(assigned, ['/auth/oidc/login?return_to=%2Fsettings%3Fx%3D1']);
  assert.equal(items.get('astovia-oidc-login'), '1');
  // Without storage the sign-in still starts.
  const blocked = browser({ storage: false });
  startOIDCLogin('/', '');
  assert.deepEqual(blocked.assigned, ['/auth/oidc/login?return_to=%2F']);
});

test('the page after an OIDC sign-in tells the other tabs once', async () => {
  const { items } = browser();
  items.set('astovia-oidc-login', '1');
  const { auth, messages } = await harness(async (path) => {
    if (path === '/auth/refresh') return json(session);
    if (path === '/api/v1/me') return json(user);
    return json({}, 404);
  });
  auth.initialize();
  await until(() => messages.length === 1);
  assert.deepEqual(messages, ['session-changed']);
  assert.equal(auth.getSnapshot().status, 'authenticated');
  assert.equal(items.has('astovia-oidc-login'), false);
});

test('a failed restore after an OIDC sign-in clears the mark without telling', async () => {
  const { items } = browser();
  items.set('astovia-oidc-login', '1');
  const { auth, messages } = await harness(async () => json({}, 401));
  auth.initialize();
  await until(() => auth.getSnapshot().status === 'anonymous' && !items.has('astovia-oidc-login'));
  await new Promise((resolve) => setImmediate(resolve));
  assert.deepEqual(messages, []);
});

test('a page load without the mark tells nothing', async () => {
  const { items } = browser();
  const { auth, messages } = await harness(async (path) => {
    if (path === '/auth/refresh') return json(session);
    if (path === '/api/v1/me') return json(user);
    return json({}, 404);
  });
  auth.initialize();
  await until(() => auth.getSnapshot().status === 'authenticated');
  for (let i = 0; i < 5; i += 1) await new Promise((resolve) => setImmediate(resolve));
  assert.deepEqual(messages, []);
  assert.equal(items.size, 0);
});

test('no Access logout is exported', async () => {
  const module = await harness(async () => json({}, 401));
  assert.equal('logoutAccess' in module, false);
  assert.equal('startAccessLogin' in module, false);
});

// ---------- Two-step verification ----------

const mfaBody = (overrides = {}) => ({ mfa: { token: 'v1.test-mfa-token', enrolled: true, methods: { totp: true, webauthn: false, recovery: true }, webauthn_available: false, ...overrides } });
const pendingMFA = { token: 'v1.test-mfa-token', enrolled: true, methods: { totp: true, webauthn: false, recovery: true }, webauthnAvailable: false };
const mustEnroll = mfaBody({ enrolled: false, methods: { totp: false, webauthn: false, recovery: false }, webauthn_available: true });
const codes = Array.from({ length: 10 }, (_, i) => `abcd-efgh-jkmn-pqr${i}`);
const coded = (status, code) => json({ error: { code, message: 'x' } }, status);

// pendingHarness signs in up to the second factor. routes maps a path to
// the function that answers it.
async function pendingHarness(routes, first = mfaBody()) {
  const calls = [];
  const h = await harness(async (path, init) => {
    calls.push([path, init?.body ? JSON.parse(init.body) : null]);
    if (path === '/auth/login') return json(first);
    if (routes[path]) return routes[path]();
    if (path === '/api/v1/me') return json(user);
    return json({}, 404);
  });
  await h.auth.login('test-user@example.com', 'test-password');
  return { ...h, calls };
}

test('a login that needs a second factor neither signs in nor tells the other tabs', async () => {
  const { auth, messages, calls } = await pendingHarness({});
  const state = auth.getSnapshot();
  assert.equal(state.status, 'anonymous');
  assert.equal(state.user, null);
  assert.equal(state.busy, null);
  assert.equal(state.error, null);
  assert.deepEqual(state.mfa, pendingMFA);
  assert.deepEqual(messages, []);
  // No profile is read: there is no session.
  assert.deepEqual(calls.map(([path]) => path), ['/auth/login']);
});

test('a login that must enroll first keeps that it is not enrolled', async () => {
  const { auth, messages } = await pendingHarness({}, mustEnroll);
  assert.deepEqual(auth.getSnapshot().mfa, { token: 'v1.test-mfa-token', enrolled: false, methods: { totp: false, webauthn: false, recovery: false }, webauthnAvailable: true });
  assert.equal(auth.getSnapshot().status, 'anonymous');
  assert.deepEqual(messages, []);
});

test('malformed second factor responses neither sign in nor wait for a factor', async () => {
  const { webauthn_available: _available, ...withoutAvailable } = mfaBody().mfa;
  const { methods: _methods, ...withoutMethods } = mfaBody().mfa;
  for (const body of [
    { mfa: null },
    { mfa: {} },
    mfaBody({ token: '' }),
    mfaBody({ token: 1 }),
    mfaBody({ enrolled: 'yes' }),
    { mfa: withoutAvailable },
    { mfa: withoutMethods },
    mfaBody({ methods: { totp: true, webauthn: false } }),
    mfaBody({ methods: { totp: 1, webauthn: false, recovery: false } }),
    // Nothing can be given before a factor is enrolled.
    mfaBody({ enrolled: false }),
    // A session and a request for a second factor at once.
    { ...session, ...mfaBody() },
  ]) {
    const { auth, messages, calls } = await pendingHarness({}, body);
    assert.equal(auth.getSnapshot().status, 'anonymous', JSON.stringify(body));
    assert.equal(auth.getSnapshot().mfa, null, JSON.stringify(body));
    assert.ok(auth.getSnapshot().error, JSON.stringify(body));
    assert.deepEqual(messages, []);
    assert.deepEqual(calls.map(([path]) => path), ['/auth/login']);
  }
  const { readPendingMFA } = await harness(async () => json({}, 404));
  assert.deepEqual(readPendingMFA(mfaBody().mfa), pendingMFA);
  assert.throws(() => readPendingMFA([]));
});

test('completeLink waits for the second factor without signing in', async () => {
  const calls = [];
  const { auth, messages } = await harness(async (path) => {
    calls.push(path);
    if (path === '/auth/password-resets/complete') return json(mfaBody());
    return json(user);
  });
  await auth.completeLink('/auth/password-resets/complete', 'pwr_token', 'new-password-123');
  assert.equal(auth.getSnapshot().status, 'anonymous');
  assert.equal(auth.getSnapshot().busy, null);
  assert.deepEqual(auth.getSnapshot().mfa, pendingMFA);
  assert.deepEqual(messages, []);
  assert.deepEqual(calls, ['/auth/password-resets/complete']);
});

test('the second factor signs in like login and leaves the screen right before', async () => {
  const { auth, messages, calls, locks } = await pendingHarness({ '/auth/mfa/totp': () => json(session) });
  const seen = [];
  await auth.verifyMFA('/auth/mfa/totp', { code: '123456' }, () => { seen.push(auth.getSnapshot().status); });
  assert.deepEqual(seen, ['anonymous']);
  assert.equal(auth.getSnapshot().status, 'authenticated');
  assert.deepEqual(auth.getSnapshot().user, user);
  assert.equal(auth.getSnapshot().mfa, null);
  assert.deepEqual(messages, ['session-changed']);
  assert.deepEqual(calls.slice(1), [['/auth/mfa/totp', { code: '123456', mfa_token: 'v1.test-mfa-token' }], ['/api/v1/me', null]]);
  assert.deepEqual(locks, ['astovia-auth', 'astovia-auth']);
});

test('a wrong code is thrown with its error code and keeps the sign-in waiting', async () => {
  const { auth, messages, APIError, isSuperseded } = await pendingHarness({
    '/auth/mfa/totp': () => coded(401, 'mfa_code_invalid'),
    '/auth/mfa/recovery': () => coded(429, 'rate_limited'),
  });
  await assert.rejects(auth.verifyMFA('/auth/mfa/totp', { code: '000000' }),
    (error) => error instanceof APIError && error.status === 401 && error.code === 'mfa_code_invalid' && !isSuperseded(error));
  await assert.rejects(auth.verifyMFA('/auth/mfa/recovery', { code: 'x' }), (error) => error instanceof APIError && error.status === 429);
  assert.equal(auth.getSnapshot().status, 'anonymous');
  assert.deepEqual(auth.getSnapshot().mfa, pendingMFA);
  assert.equal(auth.getSnapshot().error, null);
  assert.deepEqual(messages, []);
});

test('a malformed session after the second factor does not sign in', async () => {
  const { auth, messages } = await pendingHarness({ '/auth/mfa/totp': () => json({ ...session, user: { id: 'x' } }) });
  await assert.rejects(auth.verifyMFA('/auth/mfa/totp', { code: '123456' }));
  assert.equal(auth.getSnapshot().status, 'anonymous');
  assert.deepEqual(messages, []);
});

test('an expired MFA token leads back to the login form', async () => {
  for (const run of [
    (auth) => auth.verifyMFA('/auth/mfa/totp', { code: '123456' }),
    (auth) => auth.beginMFA('/auth/mfa/webauthn/begin'),
    (auth) => auth.enrollMFA('/auth/mfa/enroll/totp/finish', { setup_token: 's', code: '123456' }),
  ]) {
    const expired = () => coded(401, 'mfa_token_invalid');
    const { auth, messages, isSuperseded } = await pendingHarness({ '/auth/mfa/totp': expired, '/auth/mfa/webauthn/begin': expired, '/auth/mfa/enroll/totp/finish': expired });
    await assert.rejects(run(auth), (error) => isSuperseded(error));
    assert.equal(auth.getSnapshot().status, 'anonymous');
    assert.equal(auth.getSnapshot().mfa, null);
    assert.equal(auth.getSnapshot().error, '時間が切れました。もう一度ログインしてください。');
    assert.deepEqual(messages, []);
  }
});

test('a session that could not be started leads back to the login form', async () => {
  const failed = () => coded(503, 'signin_unavailable');
  const verify = await pendingHarness({ '/auth/mfa/recovery': failed });
  await assert.rejects(verify.auth.verifyMFA('/auth/mfa/recovery', { code: 'x' }), (error) => verify.isSuperseded(error));
  assert.equal(verify.auth.getSnapshot().mfa, null);
  assert.equal(verify.auth.getSnapshot().error, 'ログインできませんでした。もう一度ログインしてください。');

  const enroll = await pendingHarness({ '/auth/mfa/enroll/webauthn/finish': failed }, mustEnroll);
  await assert.rejects(enroll.auth.enrollMFA('/auth/mfa/enroll/webauthn/finish', { state: 's', credential: {}, name: 'Key' }), (error) => enroll.isSuperseded(error));
  assert.equal(enroll.auth.getSnapshot().mfa, null);
  assert.equal(enroll.auth.getSnapshot().error, '登録は完了しました。もう一度ログインしてください。リカバリーコードは、ログイン後に二段階認証の画面で再発行してください。');

  // An enrollment that was not stored keeps the sign-in waiting.
  const unavailable = await pendingHarness({ '/auth/mfa/enroll/totp/finish': () => coded(503, 'unavailable') }, mustEnroll);
  await assert.rejects(unavailable.auth.enrollMFA('/auth/mfa/enroll/totp/finish', { setup_token: 's', code: '123456' }),
    (error) => error instanceof unavailable.APIError && error.code === 'unavailable');
  assert.notEqual(unavailable.auth.getSnapshot().mfa, null);
  assert.equal(unavailable.auth.getSnapshot().error, null);
});

test('beginMFA returns the body and sends the MFA token', async () => {
  const { auth, calls, messages } = await pendingHarness({ '/auth/mfa/enroll/totp/begin': () => json({ secret: 'S', otpauth_url: 'u', setup_token: 't' }) }, mustEnroll);
  assert.deepEqual(await auth.beginMFA('/auth/mfa/enroll/totp/begin'), { secret: 'S', otpauth_url: 'u', setup_token: 't' });
  assert.deepEqual(calls.at(-1), ['/auth/mfa/enroll/totp/begin', { mfa_token: 'v1.test-mfa-token' }]);
  assert.equal(auth.getSnapshot().status, 'anonymous');
  assert.notEqual(auth.getSnapshot().mfa, null);
  assert.deepEqual(messages, []);
});

test('an enrollment before the first sign-in stays signed out until the codes are saved', async () => {
  const { auth, messages, calls } = await pendingHarness({ '/auth/mfa/enroll/totp/finish': () => json({ ...session, recovery_codes: codes }) }, mustEnroll);
  assert.deepEqual(await auth.enrollMFA('/auth/mfa/enroll/totp/finish', { setup_token: 'setup', code: '123456' }), codes);
  for (let i = 0; i < 5; i += 1) await new Promise((resolve) => setImmediate(resolve));
  // The server has started the session; the tab does not use it yet.
  assert.equal(auth.getSnapshot().status, 'anonymous');
  assert.equal(auth.getSnapshot().user, null);
  assert.notEqual(auth.getSnapshot().mfa, null);
  assert.deepEqual(messages, []);
  assert.deepEqual(calls.at(-1), ['/auth/mfa/enroll/totp/finish', { setup_token: 'setup', code: '123456', mfa_token: 'v1.test-mfa-token' }]);
  assert.equal(calls.some(([path]) => path === '/api/v1/me'), false);

  const seen = [];
  await auth.finishEnrollment(() => { seen.push(auth.getSnapshot().status); });
  assert.deepEqual(seen, ['anonymous']);
  assert.equal(auth.getSnapshot().status, 'authenticated');
  assert.deepEqual(auth.getSnapshot().user, user);
  assert.equal(auth.getSnapshot().mfa, null);
  assert.deepEqual(messages, ['session-changed']);
  assert.deepEqual(calls.at(-1), ['/api/v1/me', null]);
  // The held session is used once.
  await assert.rejects(auth.finishEnrollment());
});

test('an enrollment response without recovery codes is rejected', async () => {
  for (const body of [session, { ...session, recovery_codes: null }, { ...session, recovery_codes: [] }, { ...session, recovery_codes: ['a', 1] }, { recovery_codes: codes }]) {
    const { auth, messages } = await pendingHarness({ '/auth/mfa/enroll/totp/finish': () => json(body) }, mustEnroll);
    await assert.rejects(auth.enrollMFA('/auth/mfa/enroll/totp/finish', { setup_token: 'setup', code: '123456' }));
    assert.equal(auth.getSnapshot().status, 'anonymous');
    assert.deepEqual(messages, []);
    await assert.rejects(auth.finishEnrollment());
    assert.equal(auth.getSnapshot().status, 'anonymous');
  }
});

test('another tab signing out or in drops the pending second factor and the held session', async () => {
  for (const message of ['logout', 'session-changed']) {
    const { auth, receive, messages } = await pendingHarness({
      '/auth/mfa/enroll/totp/finish': () => json({ ...session, recovery_codes: codes }),
      '/auth/refresh': () => json({}, 401),
    }, mustEnroll);
    await auth.enrollMFA('/auth/mfa/enroll/totp/finish', { setup_token: 'setup', code: '123456' });
    receive(message);
    assert.equal(auth.getSnapshot().mfa, null);
    await assert.rejects(auth.finishEnrollment());
    await until(() => auth.getSnapshot().status === 'anonymous');
    assert.equal(auth.getSnapshot().user, null);
    assert.deepEqual(messages, []);
  }
});

test('starting over drops the second factor and a late answer does not sign in', async () => {
  const answer = deferred();
  const { auth, messages, isSuperseded } = await pendingHarness({ '/auth/mfa/totp': () => answer.promise });
  const pending = auth.verifyMFA('/auth/mfa/totp', { code: '123456' });
  await new Promise((resolve) => setImmediate(resolve));
  auth.cancelMFA();
  assert.equal(auth.getSnapshot().mfa, null);
  assert.equal(auth.getSnapshot().error, null);
  answer.resolve(json(session));
  await assert.rejects(pending, (error) => isSuperseded(error));
  assert.equal(auth.getSnapshot().status, 'anonymous');
  assert.deepEqual(messages, []);
  // Without a pending sign-in there is nothing to send.
  await assert.rejects(auth.verifyMFA('/auth/mfa/totp', { code: '123456' }), (error) => isSuperseded(error));
  await assert.rejects(auth.beginMFA('/auth/mfa/webauthn/begin'), (error) => isSuperseded(error));
});

test('a new login replaces the pending second factor', async () => {
  let logins = 0;
  const { auth } = await harness(async (path) => {
    if (path === '/auth/login') { logins += 1; return logins === 1 ? json(mfaBody()) : json(session); }
    return json(user);
  });
  await auth.login('test-user@example.com', 'test-password');
  assert.notEqual(auth.getSnapshot().mfa, null);
  await auth.login('test-user@example.com', 'test-password');
  assert.equal(auth.getSnapshot().status, 'authenticated');
  assert.equal(auth.getSnapshot().mfa, null);
});

test('the secrets of the second factor stay in memory', async () => {
  const written = [];
  const store = () => ({ getItem: () => null, removeItem: () => {}, setItem: (key, value) => { written.push([key, value]); } });
  Object.defineProperty(globalThis, 'sessionStorage', { configurable: true, value: store() });
  Object.defineProperty(globalThis, 'localStorage', { configurable: true, value: store() });
  const logged = [];
  const original = { log: console.log, info: console.info, warn: console.warn, error: console.error, debug: console.debug };
  for (const name of Object.keys(original)) console[name] = (...values) => { logged.push(values); };
  try {
    const { auth } = await pendingHarness({
      '/auth/mfa/enroll/totp/begin': () => json({ secret: 'JBSWY3DPEHPK3PXP', otpauth_url: 'otpauth://totp/x', setup_token: 'v1.setup' }),
      '/auth/mfa/enroll/totp/finish': () => json({ ...session, recovery_codes: codes }),
    }, mustEnroll);
    await auth.beginMFA('/auth/mfa/enroll/totp/begin');
    await auth.enrollMFA('/auth/mfa/enroll/totp/finish', { setup_token: 'v1.setup', code: '123456' });
    await auth.finishEnrollment();
  } finally {
    Object.assign(console, original);
    delete globalThis.localStorage;
  }
  assert.deepEqual(written, []);
  assert.deepEqual(logged, []);
});
