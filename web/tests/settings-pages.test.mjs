import assert from 'node:assert/strict';
import { test } from 'node:test';
import { clientsStatus, devicesStatus, githubStatus, settingsPageAt, settingsPages } from '../.test-build/settingsPages.js';

test('settings has four pages under /settings', () => {
  assert.deepEqual(settingsPages.map((p) => p.path), ['integrations', 'notifications', 'ai-clients', 'account']);
  assert.equal(settingsPageAt('/settings/notifications'), 'notifications');
  assert.equal(settingsPageAt('/settings/ai-clients/'), 'ai-clients');
  assert.equal(settingsPageAt('/settings'), null);
  assert.equal(settingsPageAt('/settings/unknown'), null);
  assert.equal(settingsPageAt('/auto'), null);
});

const github = { connected: true, login: 'octo', token_last4: '1234', scopes: ['repo'], immediate_notify: true, status: 'active', last_error: null, last_synced_at: null, created_at: '2026-10-01T00:00:00Z' };

test('the phone list shows the state of each page', () => {
  assert.equal(githubStatus({ connected: false }), 'GitHub は未接続');
  assert.equal(githubStatus(github), 'GitHub（octo）と連携中');
  assert.match(githubStatus({ ...github, status: 'error' }), /確認してください/);
  assert.match(githubStatus({ ...github, last_error: 'rate_limited' }), /確認してください/);
  assert.equal(devicesStatus([]), '通知を受け取る端末はありません');
  assert.equal(devicesStatus([{}, {}]), '2台の端末で受け取り中');
  assert.equal(clientsStatus([{}], []), '接続中 1件 · トークン 0件');
});
