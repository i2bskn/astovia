import assert from 'node:assert/strict';
import { test } from 'node:test';
import { channelErrorMessage, channelLabel, createNotificationChannelsClient, kindLabel, notificationChannels, notificationKinds, readNotificationChannels, routeLabel, slackLastErrorMessage, slackReady, toggleRoute } from '../.test-build/notificationChannels.js';

const state = {
  routes: { webpush: ['detected', 'stale', 'due_soon', 'overdue', 'digest'], slack: ['overdue'], email: [] },
  slack: { configured: true, last_error: null },
  email: { available: true, provider: 'resend', address: 'alice@example.com' },
};

function fake(response) {
  const calls = [];
  const client = createNotificationChannelsClient(async (path, init) => {
    calls.push({ path, init });
    if (response instanceof Error) throw response;
    return response;
  });
  return { client, calls };
}

test('kind and channel labels', () => {
  assert.deepEqual(notificationKinds, ['detected', 'stale', 'due_soon', 'overdue', 'digest']);
  assert.deepEqual(notificationChannels, ['webpush', 'slack', 'email']);
  assert.deepEqual(notificationKinds.map(kindLabel), ['タスクの検出', '未対応が続く', '期限が近い', '期限を過ぎた', '朝のまとめ']);
  assert.deepEqual(notificationChannels.map(channelLabel), ['Web Push', 'Slack', 'メール']);
  assert.equal(routeLabel('digest', 'email'), '朝のまとめをメールに送る');
  assert.equal(routeLabel('detected', 'webpush'), 'タスクの検出をWeb Pushに送る');
});

test('notification channels client', async () => {
  let f = fake(state);
  assert.deepEqual(await f.client.get(), state);
  assert.equal(f.calls[0].path, '/api/v1/notification-channels');
  assert.equal(f.calls[0].init, undefined);

  await f.client.saveRoutes({ webpush: ['detected'], slack: [], email: ['digest'] });
  assert.equal(f.calls[1].path, '/api/v1/notification-channels');
  assert.equal(f.calls[1].init.method, 'PUT');
  assert.equal(f.calls[1].init.headers['Content-Type'], 'application/json');
  assert.deepEqual(JSON.parse(f.calls[1].init.body), { routes: { webpush: ['detected'], slack: [], email: ['digest'] } });

  await f.client.setSlackWebhook('  https://hooks.slack.com/services/T1/B1/x1 \n');
  assert.equal(f.calls[2].path, '/api/v1/notification-channels/slack-webhook');
  assert.equal(f.calls[2].init.method, 'PUT');
  assert.deepEqual(JSON.parse(f.calls[2].init.body), { url: 'https://hooks.slack.com/services/T1/B1/x1' });

  f = fake(undefined);
  await f.client.deleteSlackWebhook();
  assert.equal(f.calls[0].path, '/api/v1/notification-channels/slack-webhook');
  assert.equal(f.calls[0].init.method, 'DELETE');
  await f.client.sendTest('email');
  assert.equal(f.calls[1].path, '/api/v1/notification-channels/test');
  assert.equal(f.calls[1].init.method, 'POST');
  assert.deepEqual(JSON.parse(f.calls[1].init.body), { channel: 'email' });
});

test('notification channels responses are checked', async () => {
  const unavailable = { ...state, email: { available: false, provider: null, address: null } };
  assert.deepEqual(readNotificationChannels(unavailable), unavailable);
  for (const bad of [
    null,
    {},
    { ...state, routes: { webpush: [], slack: [] } },
    { ...state, routes: { ...state.routes, slack: ['test'] } },
    { ...state, routes: { ...state.routes, email: null } },
    { ...state, slack: { configured: 'yes', last_error: null } },
    { ...state, slack: { configured: true, last_error: 1 } },
    { ...state, email: { available: true, provider: 1, address: null } },
  ]) {
    assert.throws(() => readNotificationChannels(bad), /Unexpected notification channels response/);
    await assert.rejects(fake(bad).client.get(), /Unexpected notification channels response/);
  }
});

test('routes are toggled in the order of the table', () => {
  const routes = { webpush: ['digest'], slack: [], email: [] };
  const added = toggleRoute(routes, 'webpush', 'detected', true);
  assert.deepEqual(added.webpush, ['detected', 'digest']);
  assert.deepEqual(routes.webpush, ['digest']);
  assert.deepEqual(toggleRoute(added, 'webpush', 'detected', true).webpush, ['detected', 'digest']);
  assert.deepEqual(toggleRoute(added, 'webpush', 'digest', false).webpush, ['detected']);
  assert.deepEqual(toggleRoute(added, 'email', 'overdue', true), { webpush: ['detected', 'digest'], slack: [], email: ['overdue'] });
});

test('Slack state and messages', () => {
  assert.equal(slackReady(state), true);
  assert.equal(slackReady({ ...state, slack: { configured: false, last_error: null } }), false);
  assert.equal(slackReady({ ...state, slack: { configured: true, last_error: 'webhook_invalid' } }), false);
  assert.equal(slackLastErrorMessage(null), null);
  assert.equal(slackLastErrorMessage('webhook_invalid'), 'Slack の Webhook が使えなくなりました。登録し直してください。');
  assert.equal(slackLastErrorMessage('webhook_undecryptable'), '保存した Webhook を読み出せません。登録し直してください。');
  assert.match(slackLastErrorMessage('other'), /登録し直してください/);
});

test('channel error messages', () => {
  assert.equal(channelErrorMessage(400, 'mail_unavailable'), 'このサーバーではメール通知を設定していません。');
  assert.equal(channelErrorMessage(409, 'mail_unavailable'), 'このサーバーではメール通知を設定していません。');
  assert.equal(channelErrorMessage(409, 'no_destination'), 'この送り先はまだ設定されていません。');
  assert.equal(channelErrorMessage(409, 'no_subscription'), '通知を受け取る端末が登録されていません。');
  assert.equal(channelErrorMessage(429, 'rate_limited'), 'テスト通知は1分に3回までです。少し待ってからお試しください。');
  assert.equal(channelErrorMessage(400, 'invalid_request', true), 'Slack の Incoming Webhook の URL（https://hooks.slack.com/services/…）を貼ってください。');
  assert.equal(channelErrorMessage(400, 'invalid_request'), '設定を保存できませんでした。入力内容を確認してください。');
  assert.match(channelErrorMessage(503, 'unavailable'), /接続を確認して/);
  assert.match(channelErrorMessage(0, null), /接続を確認して/);
});
