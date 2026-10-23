// The request function is injected (normally auth.ts's request) so that this
// module holds no authentication state and can be tested with a fake.
import type { RequestFunction } from './actionItems';

export interface APIToken {
  id: string;
  name: string;
  created_at: string;
  last_used_at: string | null;
}

// CreatedAPIToken carries the secret, which the server returns only once.
export interface CreatedAPIToken extends APIToken {
  token: string;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function isAPIToken(value: unknown): value is APIToken {
  return isRecord(value) &&
    typeof value.id === 'string' && value.id !== '' &&
    typeof value.name === 'string' &&
    typeof value.created_at === 'string' &&
    (value.last_used_at === null || typeof value.last_used_at === 'string');
}

function readList(data: unknown): APIToken[] {
  if (!isRecord(data) || !Array.isArray(data.items) || !data.items.every(isAPIToken)) {
    throw new Error('Unexpected API token list response');
  }
  return data.items;
}

function isCreatedAPIToken(value: unknown): value is CreatedAPIToken {
  return isAPIToken(value) && 'token' in value && typeof value.token === 'string' && value.token.startsWith('ast_');
}

function readCreated(data: unknown): CreatedAPIToken {
  if (!isCreatedAPIToken(data)) throw new Error('Unexpected API token response');
  return data;
}

export function createAPITokensClient(request: RequestFunction) {
  return {
    async listAPITokens(): Promise<APIToken[]> {
      return readList(await request<unknown>('/api/v1/api-tokens'));
    },
    async createAPIToken(name: string): Promise<CreatedAPIToken> {
      return readCreated(await request<unknown>('/api/v1/api-tokens', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name }),
      }));
    },
    async deleteAPIToken(id: string): Promise<void> {
      await request<unknown>(`/api/v1/api-tokens/${encodeURIComponent(id)}`, { method: 'DELETE' });
    },
  };
}

export type APITokensClient = ReturnType<typeof createAPITokensClient>;
