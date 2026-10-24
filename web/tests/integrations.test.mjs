import assert from 'node:assert/strict';
import { test } from 'node:test';
import { autoReasonLabel, githubErrorMessage, githubStatusMessage, httpsURL } from '../.test-build/automation.js';
import { createIntegrationsClient } from '../.test-build/integrations.js';
import { base64URLToBytes, bytesToBase64URL, deviceLabel, isIOS, pushSupport, sameKey } from '../.test-build/notifications.js';
import { parseView, viewPath } from '../.test-build/view.js';

test('automatic completion reasons have Japanese labels', () => {
  assert.equal(autoReasonLabel('github.review_submitted'), 'レビューを提出した');
  assert.equal(autoReasonLabel('github.review_request_removed'), 'レビュー依頼が取り消された');
  assert.equal(autoReasonLabel('github.pr_merged'), 'PR がマージされた');
  assert.equal(autoReasonLabel('github.pr_closed'), 'PR がクローズされた');
  assert.equal(autoReasonLabel('github.issue_closed'), 'Issue がクローズされた');
  assert.equal(autoReasonLabel('github.unassigned'), '割り当てが外れた');
  assert.equal(autoReasonLabel('slack.thread_resolved'), 'slack.thread_resolved');
  assert.equal(autoReasonLabel('toString'), 'toString');
});

test('only https URLs become evidence links', () => {
  assert.equal(httpsURL('https://github.com/o/r/pull/1'), 'https://github.com/o/r/pull/1');
  for (const v of ['http://github.com/o/r', 'javascript:alert(1)', 'data:text/html,x', '//evil.example', 'not a url', '', null, undefined]) {
    assert.equal(httpsURL(v), null, String(v));
  }
});

test('GitHub messages', () => {
  assert.equal(githubErrorMessage('github_token_invalid'), 'トークンが無効です。');
  assert.equal(githubErrorMessage('github_scopes_missing'), '必要な権限（repo）が足りません。');
  assert.equal(githubErrorMessage('github_unavailable'), 'GitHub に接続できませんでした。時間をおいて試してください。');
  assert.match(githubErrorMessage(null), /接続できませんでした/);
  assert.equal(githubStatusMessage('active', null), '正常');
  assert.match(githubStatusMessage('active', 'rate_limited'), /利用上限/);
  assert.match(githubStatusMessage('error', 'token_invalid'), /トークンが無効/);
  assert.match(githubStatusMessage('error', 'token_undecryptable'), /登録し直して/);
  assert.match(githubStatusMessage('error', null), /停止/);
});

test('the view query parameter selects the screen', () => {
  assert.equal(parseView(''), 'tasks');
  assert.equal(parseView('?view=auto'), 'auto');
  assert.equal(parseView('?view=settings&x=1'), 'settings');
  assert.equal(parseView('?view=unknown'), 'tasks');
  assert.equal(parseView('?view=AUTO'), 'tasks');
  assert.equal(viewPath('tasks'), '/admin/');
  assert.equal(viewPath('auto'), '/admin/?view=auto');
  assert.equal(viewPath('settings'), '/admin/?view=settings');
});

const supported = { hasServiceWorker: true, hasPushManager: true, hasNotification: true, isIOS: false, standalone: false, permission: 'default' };

test('push support', () => {
  assert.equal(pushSupport(supported), 'supported');
  assert.equal(pushSupport({ ...supported, permission: 'denied' }), 'denied');
  assert.equal(pushSupport({ ...supported, hasPushManager: false }), 'unsupported');
  assert.equal(pushSupport({ ...supported, hasPushManager: false, hasNotification: false, isIOS: true }), 'ios-home-screen');
  assert.equal(pushSupport({ ...supported, hasPushManager: false, isIOS: true, standalone: true }), 'unsupported');
  // iOS 16.4+ added to the home screen has PushManager.
  assert.equal(pushSupport({ ...supported, isIOS: true, standalone: true }), 'supported');
});

test('iOS detection and device labels', () => {
  const iphone = 'Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1';
  const ipadDesktop = 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Safari/605.1.15';
  const chromeAndroid = 'Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Mobile Safari/537.36';
  const edge = 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36 Edg/130.0.0.0';
  assert.equal(isIOS(iphone, 'iPhone', 5), true);
  assert.equal(isIOS(ipadDesktop, 'MacIntel', 5), true);
  assert.equal(isIOS(ipadDesktop, 'MacIntel', 0), false);
  assert.equal(isIOS(chromeAndroid, 'Linux armv8l', 5), false);
  assert.equal(deviceLabel(iphone), 'iPhone Safari');
  assert.equal(deviceLabel(ipadDesktop), 'Mac Safari');
  assert.equal(deviceLabel(chromeAndroid), 'Android Chrome');
  assert.equal(deviceLabel(edge), 'Windows Edge');
  assert.equal(deviceLabel(''), 'ブラウザ');
});

test('base64url keys', () => {
  const bytes = new Uint8Array([0, 251, 255, 4, 62, 63]);
  const encoded = bytesToBase64URL(bytes.buffer);
  assert.equal(encoded, 'APv_BD4_');
  assert.deepEqual([...base64URLToBytes(encoded)], [...bytes]);
  assert.deepEqual([...base64URLToBytes('APv_BD4_==')], [...bytes]);
  assert.equal(sameKey(bytes.buffer, base64URLToBytes(encoded)), true);
  assert.equal(sameKey(bytes.buffer, new Uint8Array([0, 251])), false);
  assert.equal(sameKey(null, bytes), false);
});

function fake(response) {
  const calls = [];
  const client = createIntegrationsClient(async (path, init) => {
    calls.push({ path, init });
    if (response instanceof Error) throw response;
    return response;
  });
  return { client, calls };
}

const connection = { connected: true, login: 'octocat', token_last4: 'abcd', scopes: ['notifications', 'repo'], immediate_notify: true, status: 'active', last_error: null, last_synced_at: null, created_at: '2026-10-24T10:00:00Z' };

test('GitHub integration client', async () => {
  let f = fake({ connected: false });
  assert.deepEqual(await f.client.getGitHub(), { connected: false });
  assert.equal(f.calls[0].path, '/api/v1/integrations/github');
  f = fake(connection);
  assert.deepEqual(await f.client.connectGitHub('ghp_x'), connection);
  assert.equal(f.calls[0].init.method, 'PUT');
  assert.deepEqual(JSON.parse(f.calls[0].init.body), { token: 'ghp_x' });
  await f.client.connectGitHub('ghp_y', false);
  assert.deepEqual(JSON.parse(f.calls[1].init.body), { token: 'ghp_y', immediate_notify: false });
  await f.client.setGitHubImmediateNotify(false);
  assert.equal(f.calls[2].init.method, 'PATCH');
  assert.deepEqual(JSON.parse(f.calls[2].init.body), { immediate_notify: false });
  f = fake(undefined);
  await f.client.disconnectGitHub();
  assert.equal(f.calls[0].init.method, 'DELETE');
  for (const bad of [null, {}, { connected: true }, { ...connection, scopes: 'repo' }, { ...connection, last_error: 1 }]) {
    await assert.rejects(fake(bad).client.getGitHub(), /Unexpected GitHub integration response/);
  }
});

test('push and settings clients', async () => {
  const device = { id: 'd1', label: 'iPhone Safari', created_at: '2026-10-24T10:00:00Z', last_used_at: null };
  let f = fake({ subscriptions: [device] });
  assert.deepEqual(await f.client.listPushDevices(), [device]);
  assert.equal(f.calls[0].path, '/api/v1/push/subscriptions');
  f = fake(device);
  const sub = { endpoint: 'https://push.example/x', keys: { p256dh: 'p', auth: 'a' }, label: 'iPhone Safari', timezone: 'Asia/Tokyo' };
  assert.deepEqual(await f.client.registerPushDevice(sub), device);
  assert.equal(f.calls[0].init.method, 'POST');
  assert.deepEqual(JSON.parse(f.calls[0].init.body), sub);
  f = fake(undefined);
  await f.client.deletePushDevice('a/b');
  assert.equal(f.calls[0].path, '/api/v1/push/subscriptions/a%2Fb');
  f = fake({ queued: true });
  await f.client.sendTestNotification();
  assert.equal(f.calls[0].path, '/api/v1/push/test');
  assert.equal(f.calls[0].init.method, 'POST');
  f = fake({ public_key: 'BKey' });
  assert.equal(await f.client.getPushPublicKey(), 'BKey');
  await assert.rejects(fake({}).client.getPushPublicKey(), /Unexpected public key response/);
  const settings = { timezone: 'Asia/Tokyo', digest_hour: 8, stale_days: 3 };
  f = fake(settings);
  assert.deepEqual(await f.client.saveNotificationSettings(settings), settings);
  assert.equal(f.calls[0].init.method, 'PUT');
  assert.deepEqual(JSON.parse(f.calls[0].init.body), settings);
  await assert.rejects(fake({ ...settings, digest_hour: 8.5 }).client.getNotificationSettings(), /Unexpected notification settings response/);
  await assert.rejects(fake({ subscriptions: [{ id: '' }] }).client.listPushDevices(), /Unexpected push subscription list response/);
});
