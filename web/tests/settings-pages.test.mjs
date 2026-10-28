import assert from 'node:assert/strict';
import { test } from 'node:test';
import { accountPageAt, accountPages, chatworkStatus, clientsStatus, devicesStatus, githubStatus, gmailStatus, integrationsStatus, legacyAccountPath, settingsPageAt, settingsPages, slackStatus, visibleAccountPages } from '../.test-build/settingsPages.js';

test('settings has four pages under /settings', () => {
  assert.deepEqual(settingsPages.map((p) => p.path), ['integrations', 'ai', 'notifications', 'ai-clients']);
  assert.equal(settingsPageAt('/settings/ai'), 'ai');
  assert.equal(settingsPageAt('/settings/notifications'), 'notifications');
  assert.equal(settingsPageAt('/settings/ai-clients/'), 'ai-clients');
  assert.equal(settingsPageAt('/settings'), null);
  assert.equal(settingsPageAt('/settings/unknown'), null);
  assert.equal(settingsPageAt('/settings/account'), null);
  assert.equal(settingsPageAt('/auto'), null);
});

test('the account screen has its own pages under /account', () => {
  assert.deepEqual(accountPages.map((p) => p.path), ['', 'users']);
  assert.equal(accountPageAt('/account'), 'account');
  assert.equal(accountPageAt('/account/'), 'account');
  assert.equal(accountPageAt('/account/users'), 'users');
  assert.equal(accountPageAt('/account/users/'), 'users');
  assert.equal(accountPageAt('/account/unknown'), null);
  assert.equal(accountPageAt('/accounts'), null);
  assert.equal(accountPageAt('/settings/account'), null);
});

test('the old account URLs under /settings lead to /account', () => {
  assert.equal(legacyAccountPath('/settings/account'), '/account');
  assert.equal(legacyAccountPath('/settings/account/'), '/account');
  assert.equal(legacyAccountPath('/settings/users'), '/account/users');
  assert.equal(legacyAccountPath('/settings/ai'), null);
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

const chatwork = { connected: true, account_name: 'alice', organization_name: 'Acme', token_last4: '1234', immediate_notify: true, detect_mentions: true, detect_dms: true, import_tasks: true, status: 'active', last_error: null, last_synced_at: null, created_at: '2026-10-01T00:00:00Z' };

test('the integrations page sums up Chatwork too', () => {
  assert.equal(settingsPages[0].summary, 'GitHub・Slack・Chatwork・メールからタスクを見つける');
  assert.equal(chatworkStatus({ connected: false }), 'Chatwork は未接続');
  assert.equal(chatworkStatus(chatwork), 'Chatwork（alice）と連携中');
  assert.equal(chatworkStatus({ ...chatwork, last_error: 'rate_limited' }), 'Chatwork（alice）の接続を確認してください');
  assert.equal(chatworkStatus({ ...chatwork, status: 'error' }), 'Chatwork（alice）の接続を確認してください');
  assert.equal(integrationsStatus(github, slack, chatwork), 'GitHub（octo）と連携中 · Slack（Acme）と連携中 · Chatwork（alice）と連携中');
  assert.equal(integrationsStatus({ connected: false }, { connected: false }, { connected: false }), 'GitHub は未接続 · Slack は未接続 · Chatwork は未接続');
  assert.equal(integrationsStatus(github, slack, { ...chatwork, status: 'error' }), 'Chatwork（alice）の接続を確認してください');
  assert.equal(integrationsStatus(undefined, undefined, chatwork), 'Chatwork（alice）と連携中');
  assert.equal(integrationsStatus({ connected: false }, undefined, chatwork), 'Chatwork（alice）と連携中');
  assert.equal(integrationsStatus(undefined, undefined, undefined), undefined);
});

const gmailAccount = { id: 'a1', email: 'alice@example.com', immediate_notify: true, status: 'active', last_error: null, last_synced_at: null, created_at: '2026-10-01T00:00:00Z' };
const gmail = { available: true, redirect_uri: 'https://astovia.test/integrations/google/callback', max_accounts: 10, accounts: [gmailAccount, { ...gmailAccount, id: 'a2', email: 'b@example.com' }] };

test('the integrations page sums up the mail accounts', () => {
  assert.equal(gmailStatus({ ...gmail, accounts: [] }), 'メールは未接続');
  assert.equal(gmailStatus({ ...gmail, available: false, accounts: [] }), 'メールは未接続');
  assert.equal(gmailStatus(gmail), 'メール（2 件）と連携中');
  assert.equal(gmailStatus({ ...gmail, accounts: [gmailAccount, { ...gmailAccount, id: 'a2', status: 'error', last_error: 'token_invalid' }] }), 'メールの接続を確認してください');
  assert.equal(gmailStatus({ ...gmail, accounts: [{ ...gmailAccount, last_error: 'rate_limited' }] }), 'メールの接続を確認してください');
  assert.equal(integrationsStatus(github, slack, chatwork, gmail), 'GitHub（octo）と連携中 · Slack（Acme）と連携中 · Chatwork（alice）と連携中 · メール（2 件）と連携中');
  assert.equal(integrationsStatus({ connected: false }, { connected: false }, { connected: false }, { ...gmail, accounts: [] }), 'GitHub は未接続 · Slack は未接続 · Chatwork は未接続 · メールは未接続');
  assert.equal(integrationsStatus(github, undefined, undefined, { ...gmail, accounts: [{ ...gmailAccount, status: 'error' }] }), 'メールの接続を確認してください');
  assert.equal(integrationsStatus({ connected: false }, undefined, undefined, gmail), 'メール（2 件）と連携中');
  assert.equal(integrationsStatus(undefined, undefined, undefined, { ...gmail, accounts: [] }), 'メールは未接続');
  assert.equal(integrationsStatus(undefined, undefined, undefined, undefined), undefined);
});

test('the user management page is shown to owners only', () => {
  assert.deepEqual(visibleAccountPages('owner').map((p) => p.value), ['account', 'users']);
  assert.deepEqual(visibleAccountPages('member').map((p) => p.value), ['account']);
  assert.equal(accountPages.find((p) => p.value === 'users').label, 'ユーザー管理');
});
