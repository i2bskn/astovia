import type { APIToken } from './apiTokens';
import type { ChatworkState, GitHubState, GmailState, PushDevice, SlackState } from './integrations';
import type { Connection } from './oauth';

// Pages of Settings, one per errand. path is relative to /settings.
export type SettingsPage = 'integrations' | 'ai' | 'notifications' | 'ai-clients';

// summary describes the page in the phone list until its status is loaded.
export const settingsPages: readonly { value: SettingsPage; label: string; path: string; summary: string }[] = [
  { value: 'integrations', label: '連携', path: 'integrations', summary: 'GitHub・Slack・Chatwork・メールからタスクを見つける' },
  { value: 'ai', label: 'AI', path: 'ai', summary: 'タスクを判断する AI のキーとモデル' },
  { value: 'notifications', label: '通知', path: 'notifications', summary: '受け取る端末と通知のタイミング' },
  { value: 'ai-clients', label: 'AI クライアント', path: 'ai-clients', summary: '接続のしかたと API トークン' },
];

// Pages of the account screen: the signed-in account and, for owners, the
// users of this Astovia. path is relative to /account.
export type AccountPage = 'account' | 'users';

export const accountPages: readonly { value: AccountPage; label: string; path: string }[] = [
  { value: 'account', label: 'アカウント', path: '' },
  { value: 'users', label: 'ユーザー管理', path: 'users' },
];

// visibleAccountPages returns the account pages of a user with role. The
// server refuses the user management APIs to members as well; hiding the
// page only keeps members from a page they cannot use.
export function visibleAccountPages(role: 'owner' | 'member'): typeof accountPages {
  return role === 'owner' ? accountPages : accountPages.filter((p) => p.value !== 'users');
}

// accountPageAt returns the account page a pathname shows, or null for
// other screens.
export function accountPageAt(pathname: string): AccountPage | null {
  const match = /^\/account(?:\/([^/]+))?\/?$/.exec(pathname);
  if (!match) return null;
  return accountPages.find((p) => p.path === (match[1] ?? ''))?.value ?? null;
}

// legacyAccountPath returns where an old Settings URL of the account pages
// moved, or null.
export function legacyAccountPath(pathname: string): string | null {
  const match = /^\/settings\/(account|users)\/?$/.exec(pathname);
  if (!match) return null;
  return match[1] === 'users' ? '/account/users' : '/account';
}

// settingsPageAt returns the page a pathname
// shows, or null for /settings itself and other screens.
export function settingsPageAt(pathname: string): SettingsPage | null {
  const match = /^\/settings\/([^/]+)\/?$/.exec(pathname);
  return settingsPages.find((p) => p.path === match?.[1])?.value ?? null;
}

export function githubStatus(state: GitHubState): string {
  if (!state.connected) return 'GitHub は未接続';
  return state.status === 'error' || state.last_error ? `GitHub（${state.login}）の接続を確認してください` : `GitHub（${state.login}）と連携中`;
}

export function slackStatus(state: SlackState): string {
  if (!state.connected) return 'Slack は未接続';
  return state.status === 'error' || state.last_error ? `Slack（${state.team_name}）の接続を確認してください` : `Slack（${state.team_name}）と連携中`;
}

export function chatworkStatus(state: ChatworkState): string {
  if (!state.connected) return 'Chatwork は未接続';
  return state.status === 'error' || state.last_error ? `Chatwork（${state.account_name}）の接続を確認してください` : `Chatwork（${state.account_name}）と連携中`;
}

// gmailStatus sums up the Google accounts of the mail integration.
export function gmailStatus(state: GmailState): string {
  if (state.accounts.length === 0) return 'メールは未接続';
  if (state.accounts.some((a) => a.status === 'error' || a.last_error !== null)) return 'メールの接続を確認してください';
  return `メール（${state.accounts.length} 件）と連携中`;
}

function needsAttention(state: GitHubState | SlackState | ChatworkState): boolean {
  return state.connected && (state.status === 'error' || state.last_error !== null);
}

// integrationsStatus sums up the GitHub, Slack, Chatwork and mail states for
// the phone list: the connections that need attention when there are any,
// otherwise the connected ones, otherwise that none is connected. A state
// that could not be read is left out; the result is undefined when none was
// read.
export function integrationsStatus(github?: GitHubState, slack?: SlackState, chatwork?: ChatworkState, gmail?: GmailState): string | undefined {
  const states: { attention: boolean; connected: boolean; text: string }[] = [];
  if (github) states.push({ attention: needsAttention(github), connected: github.connected, text: githubStatus(github) });
  if (slack) states.push({ attention: needsAttention(slack), connected: slack.connected, text: slackStatus(slack) });
  if (chatwork) states.push({ attention: needsAttention(chatwork), connected: chatwork.connected, text: chatworkStatus(chatwork) });
  if (gmail) {
    const connected = gmail.accounts.length > 0;
    states.push({ attention: connected && gmail.accounts.some((a) => a.status === 'error' || a.last_error !== null), connected, text: gmailStatus(gmail) });
  }
  if (states.length === 0) return undefined;
  const failing = states.filter((s) => s.attention);
  const connected = states.filter((s) => s.connected);
  const shown = failing.length > 0 ? failing : connected.length > 0 ? connected : states;
  return shown.map((s) => s.text).join(' · ');
}

export function devicesStatus(devices: readonly PushDevice[]): string {
  return devices.length > 0 ? `${devices.length}台の端末で受け取り中` : '通知を受け取る端末はありません';
}

export function clientsStatus(connections: readonly Connection[], tokens: readonly APIToken[]): string {
  return `接続中 ${connections.length}件 · トークン ${tokens.length}件`;
}
