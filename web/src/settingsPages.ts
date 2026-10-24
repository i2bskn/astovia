import type { APIToken } from './apiTokens';
import type { GitHubState, PushDevice } from './integrations';
import type { Connection } from './oauth';

// Pages of Settings, one per errand. path is relative to /settings.
export type SettingsPage = 'integrations' | 'ai' | 'notifications' | 'ai-clients' | 'account';

// summary describes the page in the phone list until its status is loaded.
export const settingsPages: readonly { value: SettingsPage; label: string; path: string; summary: string }[] = [
  { value: 'integrations', label: '連携', path: 'integrations', summary: 'GitHub のレビュー依頼と割り当て' },
  { value: 'ai', label: 'AI', path: 'ai', summary: 'タスクを判断する AI のキーとモデル' },
  { value: 'notifications', label: '通知', path: 'notifications', summary: '受け取る端末と通知のタイミング' },
  { value: 'ai-clients', label: 'AI クライアント', path: 'ai-clients', summary: '接続のしかたと API トークン' },
  { value: 'account', label: 'アカウント', path: 'account', summary: 'ログイン中のアカウントとログアウト' },
];

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

export function devicesStatus(devices: readonly PushDevice[]): string {
  return devices.length > 0 ? `${devices.length}台の端末で受け取り中` : '通知を受け取る端末はありません';
}

export function clientsStatus(connections: readonly Connection[], tokens: readonly APIToken[]): string {
  return `接続中 ${connections.length}件 · トークン ${tokens.length}件`;
}
