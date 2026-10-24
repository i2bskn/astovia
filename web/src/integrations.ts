// Clients of the GitHub integration, Web Push subscription and notification
// settings APIs. The request function is injected (normally auth.ts's
// request) so that this module can be tested with a fake.
import type { RequestFunction } from './actionItems';

export interface GitHubConnection {
  connected: true;
  login: string;
  token_last4: string;
  scopes: string[];
  immediate_notify: boolean;
  status: string;
  last_error: string | null;
  last_synced_at: string | null;
  created_at: string;
}

export type GitHubState = { connected: false } | GitHubConnection;

export interface PushDevice {
  id: string;
  label: string;
  created_at: string;
  last_used_at: string | null;
}

export interface NewPushSubscription {
  endpoint: string;
  keys: { p256dh: string; auth: string };
  label: string;
  timezone: string;
}

export interface NotificationSettings {
  timezone: string;
  digest_hour: number;
  stale_days: number;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function nullableString(value: unknown): boolean {
  return value === null || typeof value === 'string';
}

function readGitHub(data: unknown): GitHubState {
  if (isRecord(data) && data.connected === false) return { connected: false };
  if (
    isRecord(data) && data.connected === true &&
    typeof data.login === 'string' && typeof data.token_last4 === 'string' &&
    Array.isArray(data.scopes) && data.scopes.every((s) => typeof s === 'string') &&
    typeof data.immediate_notify === 'boolean' && typeof data.status === 'string' &&
    nullableString(data.last_error) && nullableString(data.last_synced_at) && typeof data.created_at === 'string'
  ) return data as unknown as GitHubConnection;
  throw new Error('Unexpected GitHub integration response');
}

function isDevice(value: unknown): value is PushDevice {
  return isRecord(value) && typeof value.id === 'string' && value.id !== '' && typeof value.label === 'string' &&
    typeof value.created_at === 'string' && nullableString(value.last_used_at);
}

function readDevice(data: unknown): PushDevice {
  if (!isDevice(data)) throw new Error('Unexpected push subscription response');
  return data;
}

function readSettings(data: unknown): NotificationSettings {
  if (
    !isRecord(data) || typeof data.timezone !== 'string' ||
    typeof data.digest_hour !== 'number' || !Number.isInteger(data.digest_hour) ||
    typeof data.stale_days !== 'number' || !Number.isInteger(data.stale_days)
  ) throw new Error('Unexpected notification settings response');
  return { timezone: data.timezone, digest_hour: data.digest_hour, stale_days: data.stale_days };
}

function json(method: 'POST' | 'PUT' | 'PATCH', body: object): RequestInit {
  return { method, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) };
}

export function createIntegrationsClient(request: RequestFunction) {
  return {
    async getGitHub(): Promise<GitHubState> {
      return readGitHub(await request<unknown>('/api/v1/integrations/github'));
    },
    // immediateNotify is left out to keep the current setting.
    async connectGitHub(token: string, immediateNotify?: boolean): Promise<GitHubState> {
      const body: { token: string; immediate_notify?: boolean } = { token };
      if (immediateNotify !== undefined) body.immediate_notify = immediateNotify;
      return readGitHub(await request<unknown>('/api/v1/integrations/github', json('PUT', body)));
    },
    async setGitHubImmediateNotify(immediateNotify: boolean): Promise<GitHubState> {
      return readGitHub(await request<unknown>('/api/v1/integrations/github', json('PATCH', { immediate_notify: immediateNotify })));
    },
    async disconnectGitHub(): Promise<void> {
      await request<unknown>('/api/v1/integrations/github', { method: 'DELETE' });
    },
    async getPushPublicKey(): Promise<string> {
      const data = await request<unknown>('/api/v1/push/public-key');
      if (!isRecord(data) || typeof data.public_key !== 'string' || data.public_key === '') throw new Error('Unexpected public key response');
      return data.public_key;
    },
    async listPushDevices(): Promise<PushDevice[]> {
      const data = await request<unknown>('/api/v1/push/subscriptions');
      if (!isRecord(data) || !Array.isArray(data.subscriptions) || !data.subscriptions.every(isDevice)) throw new Error('Unexpected push subscription list response');
      return data.subscriptions;
    },
    async registerPushDevice(subscription: NewPushSubscription): Promise<PushDevice> {
      return readDevice(await request<unknown>('/api/v1/push/subscriptions', json('POST', subscription)));
    },
    async deletePushDevice(id: string): Promise<void> {
      await request<unknown>(`/api/v1/push/subscriptions/${encodeURIComponent(id)}`, { method: 'DELETE' });
    },
    async sendTestNotification(): Promise<void> {
      await request<unknown>('/api/v1/push/test', { method: 'POST' });
    },
    async getNotificationSettings(): Promise<NotificationSettings> {
      return readSettings(await request<unknown>('/api/v1/notification-settings'));
    },
    async saveNotificationSettings(settings: NotificationSettings): Promise<NotificationSettings> {
      return readSettings(await request<unknown>('/api/v1/notification-settings', json('PUT', settings)));
    },
  };
}

export type IntegrationsClient = ReturnType<typeof createIntegrationsClient>;
