export interface User {
  id: string;
  username: string;
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

class APIError extends Error {
  constructor(readonly status: number) {
    super('Request failed');
  }
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
  if (!response.ok) throw new APIError(response.status);
  return response;
}

async function readAuth(response: Response): Promise<AuthResponse> {
  const data: unknown = await response.json();
  if (
    typeof data !== 'object' || data === null ||
    !('access_token' in data) || typeof data.access_token !== 'string' || !data.access_token ||
    !('expires_in' in data) || typeof data.expires_in !== 'number' || data.expires_in <= 0 ||
    !('user' in data) || typeof data.user !== 'object' || data.user === null ||
    !('id' in data.user) || typeof data.user.id !== 'string' ||
    !('username' in data.user) || typeof data.user.username !== 'string'
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
    void restore();
  },
  retry(): void { void restore(); },
  async login(username: string, password: string): Promise<void> {
    const expected = ++generation;
    refreshPromise = null;
    publish({ status: 'anonymous', user: null, busy: 'login', error: null });
    try {
      await withAuthLock(async () => {
        isCurrent(expected);
        const data = await readAuth(await post('/auth/login', { username, password }));
        accept(data, expected);
        channel?.postMessage('session-changed');
      });
      await verifyProfile(expected);
    } catch (error) {
      if (expected !== generation || error instanceof SupersededError) return;
      publish({ status: 'anonymous', user: null, busy: null, error: error instanceof APIError && error.status === 401
        ? 'ユーザー名またはパスワードを確認してください。' : connectionMessage(error) });
    }
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
    throw new APIError(response.status);
  }
  return await response.json() as T;
}

async function verifyProfile(expected: number): Promise<void> {
  try {
    const user = await request<User>('/api/v1/me');
    isCurrent(expected);
    if (typeof user.id !== 'string' || typeof user.username !== 'string') throw new Error('Unexpected user response');
    publish({ ...state, user });
  } catch (error) {
    if (expected !== generation || error instanceof SupersededError) return;
    if (error instanceof APIError && error.status === 401) return;
    publish({ ...state, error: 'アカウント情報を確認できませんでした。接続を確認してページを再読み込みしてください。' });
  }
}
