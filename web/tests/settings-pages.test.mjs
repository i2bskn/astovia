import assert from 'node:assert/strict';
import { test } from 'node:test';
import { clientsStatus, devicesStatus, githubStatus, integrationsStatus, settingsPageAt, settingsPages, slackStatus } from '../.test-build/settingsPages.js';

test('settings has five pages under /settings', () => {
  assert.deepEqual(settingsPages.map((p) => p.path), ['integrations', 'ai', 'notifications', 'ai-clients', 'account']);
  assert.equal(settingsPageAt('/settings/ai'), 'ai');
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

const slack = { connected: true, team_name: 'Acme', team_url: '', user_name: 'alice', token_last4: '1234', scopes: [], immediate_notify: true, detect_mentions: true, detect_dms: true, detect_group_dms: false, status: 'active', last_error: null, last_synced_at: null, created_at: '2026-10-01T00:00:00Z' };

test('the integrations page sums up GitHub and Slack', () => {
  assert.equal(slackStatus({ connected: false }), 'Slack は未接続');
  assert.equal(slackStatus(slack), 'Slack（Acme）と連携中');
  assert.match(slackStatus({ ...slack, last_error: 'unavailable' }), /確認してください/);
  assert.equal(integrationsStatus(github, slack), 'GitHub（octo）と連携中 · Slack（Acme）と連携中');
  assert.equal(integrationsStatus(github, { connected: false }), 'GitHub（octo）と連携中');
  assert.equal(integrationsStatus({ connected: false }, { connected: false }), 'GitHub は未接続 · Slack は未接続');
  assert.equal(integrationsStatus(github, { ...slack, status: 'error' }), 'Slack（Acme）の接続を確認してください');
  assert.equal(integrationsStatus(undefined, slack), 'Slack（Acme）と連携中');
  assert.equal(integrationsStatus(undefined, undefined), undefined);
});
