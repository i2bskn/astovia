import assert from 'node:assert/strict';
import { test } from 'node:test';

const user = { id: 'test-user', username: 'test-user' };
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
  await auth.login('test-user', 'test-password');
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
  await auth.login('test-user', 'test-password');
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
  await auth.login('test-user', 'test-password');
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
  await auth.login('test-user', 'test-password');
  await assert.rejects(request('/api/v1/test'));
  assert.equal(refreshCalls, 1);
  assert.equal(privateCalls, 2);
  assert.equal(auth.getSnapshot().status, 'anonymous');
});
