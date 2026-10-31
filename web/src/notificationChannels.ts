// Client of the notification channels API (Web Push, Slack and e-mail per
// notification kind), with the labels and messages of the channel settings.
// Pure functions, tested without a browser; the request function is injected
// (normally auth.ts's request).
import type { RequestFunction } from './actionItems';

// NotificationKind is a kind of notification routed to channels, in the
// order of the table. Test notifications are not routed.
export type NotificationKind = 'detected' | 'stale' | 'due_soon' | 'overdue' | 'digest';

export const notificationKinds: readonly NotificationKind[] = ['detected', 'stale', 'due_soon', 'overdue', 'digest'];

export type NotificationChannel = 'webpush' | 'slack' | 'email';

export const notificationChannels: readonly NotificationChannel[] = ['webpush', 'slack', 'email'];

const kindLabels: Readonly<Record<NotificationKind, string>> = {
  detected: 'タスクの検出',
  stale: '未対応が続く',
  due_soon: '期限が近い',
  overdue: '期限を過ぎた',
  digest: '朝のまとめ',
};

const channelLabels: Readonly<Record<NotificationChannel, string>> = {
  webpush: 'Web Push',
  slack: 'Slack',
  email: 'メール',
};

export function kindLabel(kind: NotificationKind): string {
  return kindLabels[kind];
}

export function channelLabel(channel: NotificationChannel): string {
  return channelLabels[channel];
}

// routeLabel is the accessible name of one cell of the table.
export function routeLabel(kind: NotificationKind, channel: NotificationChannel): string {
  return `${kindLabels[kind]}を${channelLabels[channel]}に送る`;
}

export type NotificationRoutes = Record<NotificationChannel, NotificationKind[]>;

// NotificationChannelsState is what the server reports. The Slack webhook URL
// is never returned; last_error is set when Slack stopped accepting it.
export interface NotificationChannelsState {
  routes: NotificationRoutes;
  slack: { configured: boolean; last_error: string | null };
  email: { available: boolean; provider: string | null; address: string | null };
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function isKindList(value: unknown): value is NotificationKind[] {
  return Array.isArray(value) && value.every((v) => typeof v === 'string' && (notificationKinds as readonly string[]).includes(v));
}

function isNullableString(value: unknown): value is string | null {
  return value === null || typeof value === 'string';
}

export function readNotificationChannels(data: unknown): NotificationChannelsState {
  if (!isRecord(data) || !isRecord(data.routes) || !isRecord(data.slack) || !isRecord(data.email)) throw new Error('Unexpected notification channels response');
  const { routes, slack, email } = data;
  if (!isKindList(routes.webpush) || !isKindList(routes.slack) || !isKindList(routes.email) ||
    typeof slack.configured !== 'boolean' || !isNullableString(slack.last_error) ||
    typeof email.available !== 'boolean' || !isNullableString(email.provider) || !isNullableString(email.address)) {
    throw new Error('Unexpected notification channels response');
  }
  return {
    routes: { webpush: routes.webpush, slack: routes.slack, email: routes.email },
    slack: { configured: slack.configured, last_error: slack.last_error },
    email: { available: email.available, provider: email.provider, address: email.address },
  };
}

// toggleRoute returns routes with kind added to or removed from channel,
// keeping the order of the table.
export function toggleRoute(routes: NotificationRoutes, channel: NotificationChannel, kind: NotificationKind, on: boolean): NotificationRoutes {
  const current = new Set(routes[channel]);
  if (on) current.add(kind);
  else current.delete(kind);
  return { ...routes, [channel]: notificationKinds.filter((k) => current.has(k)) };
}

// slackReady reports whether Slack notifications are delivered: a webhook is
// registered and Slack has not rejected it.
export function slackReady(state: NotificationChannelsState): boolean {
  return state.slack.configured && state.slack.last_error === null;
}

// slackLastErrorMessage explains why the registered webhook stopped.
export function slackLastErrorMessage(code: string | null): string | null {
  switch (code) {
    case null: return null;
    case 'webhook_invalid': return 'Slack の Webhook が使えなくなりました。登録し直してください。';
    case 'webhook_undecryptable': return '保存した Webhook を読み出せません。登録し直してください。';
  }
  return 'Slack の Webhook に送れなくなりました。登録し直してください。';
}

// channelErrorMessage returns the message of a failed request of the channel
// settings. webhook marks the registration of a Slack webhook.
export function channelErrorMessage(status: number, code: string | null, webhook = false): string {
  if (code === 'mail_unavailable') return 'このサーバーではメール通知を設定していません。';
  if (code === 'no_destination') return 'この送り先はまだ設定されていません。';
  if (code === 'no_subscription') return '通知を受け取る端末が登録されていません。';
  if (status === 429 || code === 'rate_limited') return 'テスト通知は1分に3回までです。少し待ってからお試しください。';
  if (code === 'invalid_request' && webhook) return 'Slack の Incoming Webhook の URL（https://hooks.slack.com/services/…）を貼ってください。';
  if (status === 400) return '設定を保存できませんでした。入力内容を確認してください。';
  return '接続を確認して、もう一度お試しください。';
}

function json(method: 'POST' | 'PUT', body: object): RequestInit {
  return { method, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) };
}

export function createNotificationChannelsClient(request: RequestFunction) {
  return {
    async get(): Promise<NotificationChannelsState> {
      return readNotificationChannels(await request<unknown>('/api/v1/notification-channels'));
    },
    async saveRoutes(routes: NotificationRoutes): Promise<NotificationChannelsState> {
      const body = { routes: { webpush: routes.webpush, slack: routes.slack, email: routes.email } };
      return readNotificationChannels(await request<unknown>('/api/v1/notification-channels', json('PUT', body)));
    },
    async setSlackWebhook(url: string): Promise<NotificationChannelsState> {
      return readNotificationChannels(await request<unknown>('/api/v1/notification-channels/slack-webhook', json('PUT', { url: url.trim() })));
    },
    async deleteSlackWebhook(): Promise<void> {
      await request<unknown>('/api/v1/notification-channels/slack-webhook', { method: 'DELETE' });
    },
    async sendTest(channel: NotificationChannel): Promise<void> {
      await request<unknown>('/api/v1/notification-channels/test', json('POST', { channel }));
    },
  };
}

export type NotificationChannelsClient = ReturnType<typeof createNotificationChannelsClient>;
