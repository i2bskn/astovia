// The request function is injected (normally auth.ts's request) so that this
// module holds no authentication state and can be tested with a fake.
export type RequestFunction = <T>(path: `/api/v1/${string}`, init?: RequestInit) => Promise<T>;

export type ActionItemStatus = 'open' | 'done' | 'dismissed';

export interface ActionItemRef {
  service: string;
  account?: string;
  conversation_id?: string;
  message_id?: string;
  url?: string;
}

export interface ActionItem {
  id: string;
  content: string;
  status: ActionItemStatus;
  note: string | null;
  due_at: string | null;
  refs: ActionItemRef[];
  version: number;
  created_at: string;
  updated_at: string;
}

export interface ActionItemPage {
  items: ActionItem[];
  next_cursor: string | null;
}

export interface ActionItemPatch {
  content?: string;
  status?: ActionItemStatus;
  note?: string | null;
  due_at?: string | null;
  refs?: ActionItemRef[];
  version?: number;
}

const statuses: readonly string[] = ['open', 'done', 'dismissed'];

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function optionalString(value: unknown): boolean {
  return value === undefined || typeof value === 'string';
}

function isRef(value: unknown): value is ActionItemRef {
  return isRecord(value) && typeof value.service === 'string' &&
    optionalString(value.account) && optionalString(value.conversation_id) &&
    optionalString(value.message_id) && optionalString(value.url);
}

function isActionItem(value: unknown): value is ActionItem {
  return isRecord(value) &&
    typeof value.id === 'string' && value.id !== '' &&
    typeof value.content === 'string' &&
    typeof value.status === 'string' && statuses.includes(value.status) &&
    (value.note === null || typeof value.note === 'string') &&
    (value.due_at === null || typeof value.due_at === 'string') &&
    Array.isArray(value.refs) && value.refs.every(isRef) &&
    typeof value.version === 'number' && Number.isInteger(value.version) && value.version > 0 &&
    typeof value.created_at === 'string' &&
    typeof value.updated_at === 'string';
}

function readItem(data: unknown): ActionItem {
  if (!isActionItem(data)) throw new Error('Unexpected action item response');
  return data;
}

function readPage(data: unknown): ActionItemPage {
  if (
    !isRecord(data) || !Array.isArray(data.items) || !data.items.every(isActionItem) ||
    !(data.next_cursor === null || (typeof data.next_cursor === 'string' && data.next_cursor !== ''))
  ) throw new Error('Unexpected action item list response');
  return { items: data.items, next_cursor: data.next_cursor };
}

// isConflict reports whether a request failed with 409. auth.ts's APIError is
// matched by shape so that this module does not import auth.ts at runtime.
export function isConflict(error: unknown): boolean {
  return error instanceof Error && 'status' in error && error.status === 409;
}

function json(method: 'POST' | 'PATCH', body: object): RequestInit {
  return { method, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) };
}

export function createActionItemsClient(request: RequestFunction) {
  return {
    async listActionItems({ status, cursor }: { status?: ActionItemStatus; cursor?: string } = {}): Promise<ActionItemPage> {
      const params = new URLSearchParams();
      if (status) params.set('status', status);
      if (cursor) params.set('cursor', cursor);
      const query = params.toString();
      return readPage(await request<unknown>(`/api/v1/action-items${query ? `?${query}` : ''}`));
    },
    async createActionItem({ content, note }: { content: string; note?: string | null }): Promise<ActionItem> {
      return readItem(await request<unknown>('/api/v1/action-items', json('POST', { content, note: note ?? null })));
    },
    async updateActionItem(id: string, patch: ActionItemPatch): Promise<ActionItem> {
      return readItem(await request<unknown>(`/api/v1/action-items/${encodeURIComponent(id)}`, json('PATCH', patch)));
    },
  };
}

export type ActionItemsClient = ReturnType<typeof createActionItemsClient>;
