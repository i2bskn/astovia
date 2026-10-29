export type Role = 'owner' | 'member';

// PasswordResetNotice reports a password reset link the user completed in
// the last 14 days: when, and the owner that issued the link.
export interface PasswordResetNotice {
  at: string;
  by_email: string;
}

export interface User {
  id: string;
  email: string;
  role: Role;
  password_reset: PasswordResetNotice | null;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

// isUser checks the user of /api/v1/me and of the login responses. A missing
// or unknown role is rejected like a missing email.
export function isUser(value: unknown): value is User {
  if (!isRecord(value) || typeof value.id !== 'string' || typeof value.email !== 'string' ||
    (value.role !== 'owner' && value.role !== 'member') || !('password_reset' in value)) return false;
  const notice = value.password_reset;
  return notice === null || (isRecord(notice) && typeof notice.at === 'string' && typeof notice.by_email === 'string');
}

interface AuthResponse {
  access_token: string;
  expires_in: number;
  user: User;
}

export interface AuthState {
  status: 'loading' | 'anonymous' | 'authenticated' | 'unavailable';
  user: User | null;
  busy: 'login' | 'logout' | null;
  error: string | null;
}

// APIError carries the HTTP status and, when the server sent one, the error
// code of its JSON error body (for example github_token_invalid).
export class APIError extends Error {
  constructor(readonly status: number, readonly code: string | null = null) {
    super('Request failed');
  }
}

async function errorCode(response: Response): Promise<string | null> {
  try {
    const data: unknown = await response.json();
    if (typeof data === 'object' && data !== null && 'error' in data && typeof data.error === 'object' && data.error !== null &&
      'code' in data.error && typeof data.error.code === 'string') return data.error.code;
  } catch {
    // Not a JSON error body.
  }
  return null;
}

class SupersededError extends Error {}

let state: AuthState = { status: 'loading', user: null, busy: null, error: null };
let accessToken: string | null = null;
let generation = 0;
let initialized = false;
let refreshPromise: Promise<void> | null = null;
const listeners = new Set<() => void>();
const channel = typeof BroadcastChannel !== 'undefined' ? new BroadcastChannel('astovia-auth') : null;

function publish(next: AuthState): void {
  state = next;
  for (const listener of listeners) listener();
}

function anonymous(): void {
  accessToken = null;
  publish({ status: 'anonymous', user: null, busy: null, error: null });
}

function isCurrent(expected: number): void {
  if (expected !== generation) throw new SupersededError();
}

async function withAuthLock<T>(operation: () => Promise<T>): Promise<T> {
  if (navigator.locks) return navigator.locks.request('astovia-auth', operation);
  return operation();
}

async function post(path: string, body: object): Promise<Response> {
  const response = await fetch(path, {
    method: 'POST',
    credentials: 'same-origin',
    cache: 'no-store',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
  if (!response.ok) throw new APIError(response.status, await errorCode(response));
  return response;
}

async function readAuth(response: Response): Promise<AuthResponse> {
  const data: unknown = await response.json();
  if (
    typeof data !== 'object' || data === null ||
    !('access_token' in data) || typeof data.access_token !== 'string' || !data.access_token ||
    !('expires_in' in data) || typeof data.expires_in !== 'number' || data.expires_in <= 0 ||
    !('user' in data) || !isUser(data.user)
  ) throw new Error('Unexpected authentication response');
  return data as AuthResponse;
}

function accept(data: AuthResponse, expected: number): void {
  isCurrent(expected);
  accessToken = data.access_token;
  publish({ status: 'authenticated', user: data.user, busy: null, error: null });
}

function refresh(): Promise<void> {
  if (state.busy === 'logout') return Promise.reject(new SupersededError());
  if (refreshPromise) return refreshPromise;
  const expected = generation;
  const operation = withAuthLock(async () => {
    isCurrent(expected);
    const data = await readAuth(await post('/auth/refresh', {}));
    accept(data, expected);
  }).catch((error: unknown) => {
    if (expected === generation && error instanceof APIError && error.status === 401) anonymous();
    throw error;
  });
  refreshPromise = operation;
  void operation.finally(() => {
    if (refreshPromise === operation) refreshPromise = null;
  }).catch(() => { /* The caller handles the original operation's failure. */ });
  return operation;
}

function connectionMessage(error: unknown): string {
  if (error instanceof APIError && error.status === 429) return '試行回数が上限に達しました。1分ほど待ってから、もう一度お試しください。';
  if (error instanceof APIError && error.status === 403) return '接続先の設定を確認できませんでした。運用者に連絡してください。';
  return 'サーバーに接続できませんでした。少し待ってから、もう一度お試しください。';
}

async function restore(): Promise<void> {
  const expected = generation;
  publish({ ...state, status: 'loading', error: null });
  try {
    await refresh();
    await verifyProfile(expected);
  } catch (error) {
    if (expected !== generation || error instanceof SupersededError) return;
    if (error instanceof APIError && error.status === 401) return;
    publish({ status: 'unavailable', user: null, busy: null, error: connectionMessage(error) });
  }
}

// accessLoginFlag marks, in sessionStorage, a Cloudflare Access sign-in
// started from this tab. The page loaded after the sign-in tells the other
// tabs, as a password login does.
const accessLoginFlag = 'astovia-access-login';

// announceAccessLogin runs after the first restore of a page load. When the
// tab started an Access sign-in and is now signed in, the other tabs read
// the new session. The flag is removed either way.
function announceAccessLogin(): void {
  let started = false;
  try {
    started = sessionStorage.getItem(accessLoginFlag) !== null;
    sessionStorage.removeItem(accessLoginFlag);
  } catch {
    return;
  }
  if (started && state.status === 'authenticated') channel?.postMessage('session-changed');
}

// accessLoginURL starts the Cloudflare Access sign-in, returning to the
// screen at pathname and search afterwards.
export function accessLoginURL(pathname: string, search: string): string {
  return `/auth/access/login?return_to=${encodeURIComponent(pathname + search)}`;
}

// startAccessLogin leaves for the Cloudflare Access sign-in. It is only
// called from a button: an automatic redirect could loop on a failure. A
// navigation is used instead of a form, which form-action 'self' would stop
// at the redirect to Cloudflare.
export function startAccessLogin(pathname: string, search: string): void {
  try {
    sessionStorage.setItem(accessLoginFlag, '1');
  } catch {
    // Without storage the other tabs notice the session on their next
    // refresh.
  }
  location.assign(accessLoginURL(pathname, search));
}

channel?.addEventListener('message', (event: MessageEvent<unknown>) => {
  if (event.data !== 'logout' && event.data !== 'session-changed') return;
  generation += 1;
  refreshPromise = null;
  anonymous();
  if (event.data === 'session-changed') void restore();
});

export const auth = {
  subscribe(listener: () => void): () => void {
    listeners.add(listener);
    return () => listeners.delete(listener);
  },
  getSnapshot: (): AuthState => state,
  initialize(): void {
    if (initialized) return;
    initialized = true;
    void restore().then(announceAccessLogin);
  },
  retry(): void { void restore(); },
  async login(email: string, password: string): Promise<void> {
    const expected = ++generation;
    refreshPromise = null;
    publish({ status: 'anonymous', user: null, busy: 'login', error: null });
    try {
      await withAuthLock(async () => {
        isCurrent(expected);
        const data = await readAuth(await post('/auth/login', { email, password }));
        accept(data, expected);
        channel?.postMessage('session-changed');
      });
      await verifyProfile(expected);
    } catch (error) {
      if (expected !== generation || error instanceof SupersededError) return;
      publish({ status: 'anonymous', user: null, busy: null, error: error instanceof APIError && error.status === 401
        ? 'メールアドレスまたはパスワードを確認してください。' : connectionMessage(error) });
    }
  },
  // completeLink accepts an invitation or completes a password reset, which
  // answer like login, and signs in with the result in the same way as
  // login. Failures are thrown with their error code for the caller to
  // describe; the state is then anonymous.
  async completeLink(path: '/auth/invitations/accept' | '/auth/password-resets/complete', token: string, password: string): Promise<void> {
    const expected = ++generation;
    refreshPromise = null;
    publish({ status: 'anonymous', user: null, busy: 'login', error: null });
    try {
      await withAuthLock(async () => {
        isCurrent(expected);
        const data = await readAuth(await post(path, { token, password }));
        accept(data, expected);
        channel?.postMessage('session-changed');
      });
    } catch (error) {
      if (expected === generation) publish({ status: 'anonymous', user: null, busy: null, error: null });
      throw error;
    }
    await verifyProfile(expected);
  },
  // reloadProfile reads /api/v1/me again, for example after the role changed.
  async reloadProfile(): Promise<void> {
    await verifyProfile(generation);
  },
  async logout(): Promise<void> {
    const expected = ++generation;
    refreshPromise = null;
    publish({ ...state, busy: 'logout', error: null });
    try {
      await withAuthLock(async () => {
        isCurrent(expected);
        await post('/auth/logout', {});
        isCurrent(expected);
        anonymous();
        channel?.postMessage('logout');
      });
    } catch (error) {
      if (expected !== generation || error instanceof SupersededError) return;
      publish({ ...state, busy: null, error: 'ログアウトを完了できませんでした。接続を確認して、もう一度ログアウトしてください。' });
    }
  },
};

// logoutAccess signs out of Astovia and then of Cloudflare Access at
// logoutURL. When the Astovia sign-out fails, the screen stays with the
// error of logout. A screen that is not signed in goes to logoutURL at once.
export async function logoutAccess(logoutURL: string): Promise<void> {
  if (state.status !== 'anonymous') {
    await auth.logout();
    if (auth.getSnapshot().status !== 'anonymous') return;
  }
  location.assign(logoutURL);
}

// Private API requests retry once after a single-flight refresh. Tokens stay in memory.
export async function request<T>(path: `/api/v1/${string}`, init?: RequestInit): Promise<T> {
  if (state.busy === 'logout') throw new SupersededError();
  const expected = generation;
  const send = () => fetch(path, {
    ...init,
    credentials: 'same-origin',
    cache: 'no-store',
    headers: { ...Object.fromEntries(new Headers(init?.headers)), Authorization: `Bearer ${accessToken ?? ''}` },
  });
  let response = await send();
  isCurrent(expected);
  if (response.status === 401) {
    await refresh();
    isCurrent(expected);
    response = await send();
    isCurrent(expected);
  }
  if (!response.ok) {
    if (response.status === 401) anonymous();
    throw new APIError(response.status, await errorCode(response));
  }
  // 204 No Content has no body to parse.
  if (response.status === 204) return undefined as T;
  return await response.json() as T;
}

async function verifyProfile(expected: number): Promise<void> {
  try {
    const user = await request<unknown>('/api/v1/me');
    isCurrent(expected);
    if (!isUser(user)) throw new Error('Unexpected user response');
    publish({ ...state, user });
  } catch (error) {
    if (expected !== generation || error instanceof SupersededError) return;
    if (error instanceof APIError && error.status === 401) return;
    publish({ ...state, error: 'アカウント情報を確認できませんでした。接続を確認してページを再読み込みしてください。' });
  }
}

// inspectLink reads what an invitation or password reset link is for, before
// signing in. It does not touch the authentication state.
export async function inspectLink(path: '/auth/invitations/inspect' | '/auth/password-resets/inspect', token: string): Promise<unknown> {
  return await (await post(path, { token })).json() as unknown;
}
