// The request function is injected (normally auth.ts's request) so that this
// module holds no authentication state and can be tested with a fake.
export type RequestFunction = <T>(path: `/api/v1/${string}`, init?: RequestInit) => Promise<T>;

// candidate is an item the AI found but was not sure the user has to do.
export type ActionItemStatus = 'open' | 'candidate' | 'done' | 'dismissed';

// task is a plain item; event asks the user to add a meeting to the calendar.
export type ActionItemKind = 'task' | 'event';

export interface ActionItemRef {
  service: string;
  account?: string;
  conversation_id?: string;
  // conversation_name is a readable name for display, such as #general.
  conversation_name?: string;
  message_id?: string;
  url?: string;
}

// ActionItemAuto describes an automatic completion by an integration.
export interface ActionItemAuto {
  reason: string;
  ref_url: string | null;
  at: string;
}

// ActionItemEvent is the meeting of an event item. end_at is null when no
// end was given.
export interface ActionItemEvent {
  title: string;
  start_at: string;
  end_at: string | null;
}

// ActionItemParent is the parent shown with a subtask.
export interface ActionItemParent {
  id: string;
  content: string;
  status: ActionItemStatus;
}

// ActionItemChildren counts the subtasks of a parent: total leaves out the
// dismissed ones, remaining counts the open and candidate ones. next_due_at
// is the earliest due date of the open and candidate ones, or null.
export interface ActionItemChildren {
  total: number;
  done: number;
  remaining: number;
  next_due_at: string | null;
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
  // source is the integration that created the item ("github"), or null.
  source?: string | null;
  auto?: ActionItemAuto | null;
  kind: ActionItemKind;
  // event is set exactly when kind is event.
  event: ActionItemEvent | null;
  // parent_id is the item this one is a subtask of; parent shows it.
  // Items nest at most two levels. children is null without subtasks.
  parent_id: string | null;
  parent: ActionItemParent | null;
  children: ActionItemChildren | null;
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
  // null removes the parent.
  parent_id?: string | null;
  version?: number;
}

const statuses: readonly string[] = ['open', 'candidate', 'done', 'dismissed'];

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function optionalString(value: unknown): boolean {
  return value === undefined || typeof value === 'string';
}

function isRef(value: unknown): value is ActionItemRef {
  return isRecord(value) && typeof value.service === 'string' &&
    optionalString(value.account) && optionalString(value.conversation_id) && optionalString(value.conversation_name) &&
    optionalString(value.message_id) && optionalString(value.url);
}

function isAuto(value: unknown): boolean {
  return value === undefined || value === null || (isRecord(value) &&
    typeof value.reason === 'string' &&
    (value.ref_url === null || typeof value.ref_url === 'string') &&
    typeof value.at === 'string');
}

function isEvent(value: unknown): value is ActionItemEvent {
  return isRecord(value) && typeof value.title === 'string' && typeof value.start_at === 'string' &&
    (value.end_at === null || typeof value.end_at === 'string');
}

function isCount(value: unknown): boolean {
  return typeof value === 'number' && Number.isInteger(value) && value >= 0;
}

function isParent(value: unknown): boolean {
  return value === null || (isRecord(value) && typeof value.id === 'string' && value.id !== '' &&
    typeof value.content === 'string' && typeof value.status === 'string' && statuses.includes(value.status));
}

function isChildren(value: unknown): boolean {
  return value === null || (isRecord(value) && isCount(value.total) && isCount(value.done) && isCount(value.remaining) &&
    (value.next_due_at === null || typeof value.next_due_at === 'string'));
}

// isNesting checks parent_id, parent and children together: parent is shown
// only for an item with parent_id, and for that very item.
function isNesting(value: Record<string, unknown>): boolean {
  if (!isParent(value.parent) || !isChildren(value.children)) return false;
  if (value.parent_id === null) return value.parent === null;
  return typeof value.parent_id === 'string' && value.parent_id !== '' && value.parent_id !== value.id &&
    (value.parent === null || (isRecord(value.parent) && value.parent.id === value.parent_id));
}

function isKindAndEvent(value: Record<string, unknown>): boolean {
  if (value.kind === 'task') return value.event === null;
  return value.kind === 'event' && isEvent(value.event);
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
    typeof value.updated_at === 'string' &&
    (value.source === undefined || value.source === null || typeof value.source === 'string') &&
    isAuto(value.auto) &&
    isKindAndEvent(value) &&
    isNesting(value);
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

// httpsLink returns value when it is a valid https URL. It is a copy of
// automation.ts's httpsURL: these modules import nothing at runtime, so that
// the tests can load them without a bundler.
function httpsLink(value: string | undefined): string | null {
  if (!value) return null;
  try {
    const url = new URL(value);
    return url.protocol === 'https:' ? url.href : null;
  } catch {
    return null;
  }
}

// calendarDate is a time in Google Calendar's UTC form, 20261029T033000Z.
function calendarDate(date: Date): string {
  return date.toISOString().replace(/\.\d{3}Z$/, 'Z').replace(/[-:]/g, '');
}

// calendarURL returns a Google Calendar link that opens a new event filled
// with the item's meeting, or null when the item is not an event. Without an
// end the event lasts an hour. The source post is linked when it is https.
export function calendarURL(item: ActionItem): string | null {
  if (item.kind !== 'event' || !item.event) return null;
  const start = new Date(item.event.start_at);
  if (Number.isNaN(start.getTime())) return null;
  let end = item.event.end_at ? new Date(item.event.end_at) : null;
  if (!end || Number.isNaN(end.getTime())) end = new Date(start.getTime() + 60 * 60 * 1000);
  const params = new URLSearchParams({ action: 'TEMPLATE', text: item.event.title, dates: `${calendarDate(start)}/${calendarDate(end)}` });
  const source = httpsLink(item.refs[0]?.url);
  if (source) params.set('details', source);
  return `https://calendar.google.com/calendar/render?${params.toString()}`;
}

// isConflict reports whether a request failed with 409. auth.ts's APIError is
// matched by shape so that this module does not import auth.ts at runtime.
export function isConflict(error: unknown): boolean {
  return error instanceof Error && 'status' in error && error.status === 409;
}

// isNotFound reports whether a request failed with 404, the same way.
export function isNotFound(error: unknown): boolean {
  return error instanceof Error && 'status' in error && error.status === 404;
}

// isInvalid reports whether a request failed with 400, the same way.
export function isInvalid(error: unknown): boolean {
  return error instanceof Error && 'status' in error && error.status === 400;
}

function json(method: 'POST' | 'PATCH', body: object): RequestInit {
  return { method, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) };
}

export function createActionItemsClient(request: RequestFunction) {
  return {
    // auto lists only items completed automatically, newest completion
    // first. Its cursors cannot be used without auto and vice versa. view
    // top lists the items without a parent, and the subtasks whose parent
    // has another status; it cannot be combined with auto.
    async listActionItems({ status, cursor, auto, view }: { status?: ActionItemStatus; cursor?: string; auto?: boolean; view?: 'top' } = {}): Promise<ActionItemPage> {
      const params = new URLSearchParams();
      if (status) params.set('status', status);
      if (auto) params.set('auto', 'true');
      if (view) params.set('view', view);
      if (cursor) params.set('cursor', cursor);
      const query = params.toString();
      return readPage(await request<unknown>(`/api/v1/action-items${query ? `?${query}` : ''}`));
    },
    async getActionItem(id: string): Promise<ActionItem> {
      return readItem(await request<unknown>(`/api/v1/action-items/${encodeURIComponent(id)}`));
    },
    // listChildren lists the subtasks of a parent, oldest first, in one
    // page (at most 100).
    async listChildren(parentID: string): Promise<ActionItemPage> {
      return readPage(await request<unknown>(`/api/v1/action-items?${new URLSearchParams({ parent_id: parentID }).toString()}`));
    },
    // parent_id makes the new item a subtask.
    async createActionItem({ content, note, parent_id }: { content: string; note?: string | null; parent_id?: string }): Promise<ActionItem> {
      const body: Record<string, unknown> = { content, note: note ?? null };
      if (parent_id) body.parent_id = parent_id;
      return readItem(await request<unknown>('/api/v1/action-items', json('POST', body)));
    },
    async updateActionItem(id: string, patch: ActionItemPatch): Promise<ActionItem> {
      return readItem(await request<unknown>(`/api/v1/action-items/${encodeURIComponent(id)}`, json('PATCH', patch)));
    },
  };
}

export type ActionItemsClient = ReturnType<typeof createActionItemsClient>;
