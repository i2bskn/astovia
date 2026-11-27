// The request function is injected (normally auth.ts's request) so that this
// module holds no authentication state and can be tested with a fake.
import type { RequestFunction } from './actionItems';

export type OAuthScope = 'mcp' | 'offline_access';

// Consent describes a valid authorization request on the consent screen
// (/oauth/authorize). client_name is asserted by the client itself;
// client_host is the verified host of its metadata document URL, and null
// for a dynamically registered client. user_id is the signed-in user the
// screen shows, sent back with the decision.
export interface Consent {
  status: 'ok';
  client_name: string;
  client_id: string;
  client_host: string | null;
  redirect_host: string;
  redirect_loopback: boolean;
  scopes: OAuthScope[];
  user_id: string;
}

// ConsentRejection is an authorization request the server refuses with an
// error that goes back to the client. The screen shows it and goes to
// redirect_url only when the user asks.
export interface ConsentRejection {
  status: 'error';
  error: string;
  redirect_url: string;
  redirect_host: string;
}

export type Authorization = Consent | ConsentRejection;

export type ConsentDecision = 'approve' | 'deny';

// Connection is an AI client the user has authorized.
export interface Connection {
  id: string;
  client_name: string;
  client_host: string | null;
  scopes: OAuthScope[];
  created_at: string;
  last_used_at: string | null;
  expires_at: string;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function isScopes(value: unknown): value is OAuthScope[] {
  return Array.isArray(value) && value.every((scope) => scope === 'mcp' || scope === 'offline_access');
}

// isRedirect accepts only http(s) URLs, because the page navigates to them.
function isRedirect(value: unknown): value is string {
  return typeof value === 'string' && (value.startsWith('https://') || value.startsWith('http://'));
}

function readAuthorization(value: unknown): Authorization {
  if (isRecord(value) && value.status === 'ok' &&
    typeof value.client_name === 'string' && value.client_name !== '' &&
    typeof value.client_id === 'string' && value.client_id !== '' &&
    (value.client_host === null || typeof value.client_host === 'string') &&
    typeof value.redirect_host === 'string' &&
    typeof value.redirect_loopback === 'boolean' &&
    isScopes(value.scopes) &&
    typeof value.user_id === 'string' && value.user_id !== '') {
    return {
      status: 'ok', client_name: value.client_name, client_id: value.client_id, client_host: value.client_host,
      redirect_host: value.redirect_host, redirect_loopback: value.redirect_loopback, scopes: value.scopes, user_id: value.user_id,
    };
  }
  if (isRecord(value) && value.status === 'error' && typeof value.error === 'string' && value.error !== '' &&
    isRedirect(value.redirect_url) && typeof value.redirect_host === 'string') {
    return { status: 'error', error: value.error, redirect_url: value.redirect_url, redirect_host: value.redirect_host };
  }
  throw new Error('Unexpected consent response');
}

function isConnection(value: unknown): value is Connection {
  return isRecord(value) &&
    typeof value.id === 'string' && value.id !== '' &&
    typeof value.client_name === 'string' &&
    (value.client_host === null || typeof value.client_host === 'string') &&
    isScopes(value.scopes) &&
    typeof value.created_at === 'string' &&
    (value.last_used_at === null || typeof value.last_used_at === 'string') &&
    typeof value.expires_at === 'string';
}

function readRedirect(data: unknown): string {
  if (!isRecord(data) || !isRedirect(data.redirect_url)) throw new Error('Unexpected consent response');
  return data.redirect_url;
}

export function createOAuthClient(request: RequestFunction) {
  return {
    // getAuthorization checks the authorization request of the consent
    // screen. query is the screen's query string as the client sent it,
    // without the leading ?, and is passed on unchanged.
    async getAuthorization(query: string): Promise<Authorization> {
      return readAuthorization(await request<unknown>(`/api/v1/oauth/authorize${query ? `?${query}` : ''}`));
    },
    // decide approves or denies the request for userID, the user the screen
    // showed, and returns the URL to send the browser to.
    async decide(query: string, decision: ConsentDecision, userID: string): Promise<string> {
      return readRedirect(await request<unknown>('/api/v1/oauth/authorize', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ query, decision, user_id: userID }),
      }));
    },
    async listConnections(): Promise<Connection[]> {
      const data = await request<unknown>('/api/v1/oauth/connections');
      if (!isRecord(data) || !Array.isArray(data.items) || !data.items.every(isConnection)) {
        throw new Error('Unexpected connection list response');
      }
      return data.items;
    },
    async deleteConnection(id: string): Promise<void> {
      await request<unknown>(`/api/v1/oauth/connections/${encodeURIComponent(id)}`, { method: 'DELETE' });
    },
  };
}

export type OAuthClient = ReturnType<typeof createOAuthClient>;

function errorStatus(error: unknown): number | null {
  return error instanceof Error && 'status' in error && typeof error.status === 'number' ? error.status : null;
}

function errorCode(error: unknown): string | null {
  return error instanceof Error && 'code' in error && typeof error.code === 'string' ? error.code : null;
}

// isUserChanged reports whether a decision was refused because another user
// signed in after the consent screen was shown. auth.ts's APIError is
// matched by shape so that this module does not import auth.ts at runtime.
export function isUserChanged(error: unknown): boolean {
  return errorStatus(error) === 409 && errorCode(error) === 'user_changed';
}

// consentErrorMessage describes a failed request of the consent screen.
export function consentErrorMessage(error: unknown): string {
  const status = errorStatus(error);
  switch (errorCode(error)) {
    case 'user_changed': return 'ログイン中のアカウントが変わりました。ページを読み込み直して、表示されるアカウントで許可するか確認してください。';
    case 'unknown_client': return 'この AI クライアントは登録されていないか、登録の期限が切れています。AI クライアントでこのサーバーを削除して、追加し直してください。';
    case 'invalid_client': return 'AI クライアントの情報を確認できませんでした。AI クライアントから接続をやり直してください。';
    case 'client_metadata_unavailable': return 'AI クライアントの情報を取得できませんでした。少し待ってから、もう一度お試しください。';
    default: break;
  }
  if (status === 400) return 'この接続リクエストは無効か期限切れです。AI クライアントから接続をやり直してください。';
  if (status === 429) return '試行回数が上限に達しました。1分ほど待ってから、もう一度お試しください。';
  if (status === 404) return 'この Astovia では、AI クライアントを OAuth で接続できません。';
  return 'サーバーに接続できませんでした。少し待ってから、もう一度お試しください。';
}
