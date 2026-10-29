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

test('the Access sign-in returns to the encoded path and query', async () => {
  const { accessLoginURL } = await harness(async () => json({}, 401));
  assert.equal(accessLoginURL('/', ''), '/auth/access/login?return_to=%2F');
  assert.equal(accessLoginURL('/consent', '?request=a%2Fb&x=1'), '/auth/access/login?return_to=%2Fconsent%3Frequest%3Da%252Fb%26x%3D1');
  assert.equal(accessLoginURL('/tasks/abc', ''), '/auth/access/login?return_to=%2Ftasks%2Fabc');
});

test('startAccessLogin marks the tab and leaves with location.assign', async () => {
  const { items, assigned } = browser();
  const { startAccessLogin } = await harness(async () => json({}, 401));
  startAccessLogin('/settings', '?x=1');
  assert.deepEqual(assigned, ['/auth/access/login?return_to=%2Fsettings%3Fx%3D1']);
  assert.equal(items.get('astovia-access-login'), '1');
  // Without storage the sign-in still starts.
  const blocked = browser({ storage: false });
  startAccessLogin('/', '');
  assert.deepEqual(blocked.assigned, ['/auth/access/login?return_to=%2F']);
});

test('the page after an Access sign-in tells the other tabs once', async () => {
  const { items } = browser();
  items.set('astovia-access-login', '1');
  const { auth, messages } = await harness(async (path) => {
    if (path === '/auth/refresh') return json(session);
    if (path === '/api/v1/me') return json(user);
    return json({}, 404);
  });
  auth.initialize();
  await until(() => messages.length === 1);
  assert.deepEqual(messages, ['session-changed']);
  assert.equal(auth.getSnapshot().status, 'authenticated');
  assert.equal(items.has('astovia-access-login'), false);
});

test('a failed restore after an Access sign-in clears the mark without telling', async () => {
  const { items } = browser();
  items.set('astovia-access-login', '1');
  const { auth, messages } = await harness(async () => json({}, 401));
  auth.initialize();
  await until(() => auth.getSnapshot().status === 'anonymous' && !items.has('astovia-access-login'));
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

test('logoutAccess leaves for Cloudflare only after the Astovia logout', async () => {
  const logoutURL = 'https://example.cloudflareaccess.com/cdn-cgi/access/logout';
  {
    const { assigned } = browser();
    const { auth, logoutAccess, messages } = await harness(async (path) => {
      if (path === '/auth/login') return json(session);
      if (path === '/api/v1/me') return json(user);
      if (path === '/auth/logout') return new Response(null, { status: 204 });
      return json({}, 404);
    });
    await auth.login('test-user@example.com', 'test-password');
    await logoutAccess(logoutURL);
    assert.equal(auth.getSnapshot().status, 'anonymous');
    assert.deepEqual(assigned, [logoutURL]);
    assert.equal(messages.at(-1), 'logout');
  }
  {
    // A failed Astovia logout stays on the screen with its error.
    const { assigned } = browser();
    const { auth, logoutAccess } = await harness(async (path) => {
      if (path === '/auth/login') return json(session);
      if (path === '/api/v1/me') return json(user);
      return json({}, 503);
    });
    await auth.login('test-user@example.com', 'test-password');
    await logoutAccess(logoutURL);
    assert.equal(auth.getSnapshot().status, 'authenticated');
    assert.match(auth.getSnapshot().error, /ログアウトを完了できません/);
    assert.deepEqual(assigned, []);
  }
  {
    // Signed out, it leaves at once.
    const { assigned } = browser();
    const calls = [];
    const { auth, logoutAccess } = await harness(async (path) => { calls.push(path); return json({}, 401); });
    auth.initialize();
    await until(() => auth.getSnapshot().status === 'anonymous');
    await logoutAccess(logoutURL);
    assert.deepEqual(assigned, [logoutURL]);
    assert.deepEqual(calls, ['/auth/refresh']);
  }
});
