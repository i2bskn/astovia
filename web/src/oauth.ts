// The request function is injected (normally auth.ts's request) so that this
// module holds no authentication state and can be tested with a fake.
import type { RequestFunction } from './actionItems';

export type OAuthScope = 'mcp' | 'offline_access';

// Consent describes an authorization request on the consent page.
// client_host is set only for clients identified by a metadata document URL;
// other clients' names are self-asserted.
export interface Consent {
  client_name: string;
  client_kind: 'cimd' | 'dcr';
  client_host: string | null;
  redirect_host: string;
  redirect_loopback: boolean;
  scopes: OAuthScope[];
  expires_at: string;
}

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

function isConsent(value: unknown): value is Consent {
  return isRecord(value) &&
    typeof value.client_name === 'string' && value.client_name !== '' &&
    (value.client_kind === 'cimd' || value.client_kind === 'dcr') &&
    (value.client_host === null || typeof value.client_host === 'string') &&
    typeof value.redirect_host === 'string' &&
    typeof value.redirect_loopback === 'boolean' &&
    isScopes(value.scopes) &&
    typeof value.expires_at === 'string';
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

// readRedirect accepts only http(s) URLs, because the page navigates to it.
function readRedirect(data: unknown): string {
  if (!isRecord(data) || typeof data.redirect_url !== 'string' ||
    !(data.redirect_url.startsWith('https://') || data.redirect_url.startsWith('http://'))) {
    throw new Error('Unexpected consent response');
  }
  return data.redirect_url;
}

export function createOAuthClient(request: RequestFunction) {
  return {
    async getConsent(authorizationRequest: string): Promise<Consent> {
      const data = await request<unknown>(`/api/v1/oauth/consent?request=${encodeURIComponent(authorizationRequest)}`);
      if (!isConsent(data)) throw new Error('Unexpected consent response');
      return data;
    },
    // decide approves or denies the request and returns the URL to send the
    // browser to.
    async decide(authorizationRequest: string, approve: boolean): Promise<string> {
      return readRedirect(await request<unknown>('/api/v1/oauth/consent', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ request: authorizationRequest, approve }),
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

// isBadRequest reports whether a request failed with 400, matching auth.ts's
// APIError by shape like isConflict.
export function isBadRequest(error: unknown): boolean {
  return error instanceof Error && 'status' in error && error.status === 400;
}
