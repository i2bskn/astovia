// The import names the .ts file so that the test build, which compiles
// the modules one by one, can resolve it (--rewriteRelativeImportExtensions).
import { oidcLoginFlag } from './oidcLogin.ts';

export { markOIDCLogin, oidcLoginPath, oidcReturnTo } from './oidcLogin.ts';

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

// AuthResponse is the answer to a sign-in that started a session. The
// session itself is an HttpOnly cookie the browser keeps; the page never
// sees it.
interface AuthResponse {
  user: User;
}

// MFAMethods are the second factors a user can give now.
export interface MFAMethods {
  totp: boolean;
  webauthn: boolean;
  recovery: boolean;
}

// PendingMFA is a password sign-in that waits for its second factor, or,
// when enrolled is false, for the enrollment of one. token identifies the
// user at the /auth/mfa/* endpoints. It lives only in memory and is gone
// after a reload.
export interface PendingMFA {
  token: string;
  enrolled: boolean;
  methods: MFAMethods;
  webauthnAvailable: boolean;
}

// mfa is set only while status is anonymous.
export interface AuthState {
  status: 'loading' | 'anonymous' | 'authenticated' | 'unavailable';
  user: User | null;
  busy: 'login' | 'logout' | null;
  error: string | null;
  mfa: PendingMFA | null;
}

// MFABeginPath, MFAVerifyPath and MFAEnrollPath are the /auth/mfa/*
// endpoints: those that start a step, those that sign in with a second
// factor, and those that enroll the first one.
export type MFABeginPath = '/auth/mfa/webauthn/begin' | '/auth/mfa/enroll/totp/begin' | '/auth/mfa/enroll/webauthn/begin';
export type MFAVerifyPath = '/auth/mfa/totp' | '/auth/mfa/recovery' | '/auth/mfa/webauthn/finish';
export type MFAEnrollPath = '/auth/mfa/enroll/totp/finish' | '/auth/mfa/enroll/webauthn/finish';

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

// isSuperseded reports whether an operation was dropped because the sign-in
// it belonged to ended meanwhile: there is nothing to tell the user.
export function isSuperseded(error: unknown): boolean {
  return error instanceof SupersededError;
}

let state: AuthState = { status: 'loading', user: null, busy: null, error: null, mfa: null };
// enrolledSession is the session of an enrollment before the first sign-in.
// The server has set its cookie already; the tab holds it back until the
// user has saved the recovery codes.
let enrolledSession: { data: AuthResponse; expected: number } | null = null;
let generation = 0;
let initialized = false;
const listeners = new Set<() => void>();
const channel = typeof BroadcastChannel !== 'undefined' ? new BroadcastChannel('astovia-auth') : null;

function publish(next: AuthState): void {
  state = next;
  for (const listener of listeners) listener();
}

// anonymous signs the tab out. A pending second factor is dropped with it.
function anonymous(error: string | null = null): void {
  enrolledSession = null;
  publish({ status: 'anonymous', user: null, busy: null, error, mfa: null });
}

function isCurrent(expected: number): void {
  if (expected !== generation) throw new SupersededError();
}

// withAuthLock serializes the requests that start or end a session across
// the tabs of this browser, which share the session cookie.
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

function checkAuth(data: unknown): AuthResponse {
  if (!isRecord(data) || !isUser(data.user)) throw new Error('Unexpected authentication response');
  return { user: data.user };
}

async function readAuth(response: Response): Promise<AuthResponse> {
  return checkAuth(await response.json() as unknown);
}

// readPendingMFA checks the mfa member of a sign-in response that asks for
// a second factor. Without an enrolled factor there is no method to give.
export function readPendingMFA(value: unknown): PendingMFA {
  if (!isRecord(value) || typeof value.token !== 'string' || !value.token || typeof value.enrolled !== 'boolean' ||
    typeof value.webauthn_available !== 'boolean' || !isRecord(value.methods)) throw new Error('Unexpected MFA response');
  const { totp, webauthn, recovery } = value.methods;
  if (typeof totp !== 'boolean' || typeof webauthn !== 'boolean' || typeof recovery !== 'boolean' ||
    (!value.enrolled && (totp || webauthn || recovery))) throw new Error('Unexpected MFA response');
  return { token: value.token, enrolled: value.enrolled, methods: { totp, webauthn, recovery }, webauthnAvailable: value.webauthn_available };
}

// SignIn is the answer to a password sign-in: a session, or the request for
// a second factor. A response that carries both is rejected.
type SignIn = { session: AuthResponse; mfa: null } | { session: null; mfa: PendingMFA };

async function readSignIn(response: Response): Promise<SignIn> {
  const data: unknown = await response.json();
  if (isRecord(data) && 'mfa' in data) {
    if ('user' in data) throw new Error('Unexpected authentication response');
    return { session: null, mfa: readPendingMFA(data.mfa) };
  }
  return { session: checkAuth(data), mfa: null };
}

// readEnrolled checks the answer to an enrollment before the first sign-in:
// the session and the recovery codes, which the server returns only once.
function readEnrolled(data: unknown): { session: AuthResponse; codes: string[] } {
  const session = checkAuth(data);
  const codes: unknown = isRecord(data) ? data.recovery_codes : null;
  if (!Array.isArray(codes) || codes.length === 0 || !codes.every((code) => typeof code === 'string' && code !== '')) {
    throw new Error('Unexpected enrollment response');
  }
  return { session, codes: codes as string[] };
}

function accept(data: AuthResponse, expected: number): void {
  isCurrent(expected);
  enrolledSession = null;
  publish({ status: 'authenticated', user: data.user, busy: null, error: null, mfa: null });
}

// pend keeps a sign-in that waits for its second factor. The tab stays
// signed out: no session exists yet and the other tabs are not told.
function pend(mfa: PendingMFA, expected: number): void {
  isCurrent(expected);
  enrolledSession = null;
  publish({ status: 'anonymous', user: null, busy: null, error: null, mfa });
}

// mfaEnded handles the failures that end a pending sign-in and reports
// whether the sign-in is over. An MFA token that expired and a session that
// could not be started after the factor was spent or enrolled both lead
// back to the login form, which shows why.
function mfaEnded(error: unknown, expected: number, enrolling: boolean): boolean {
  if (expected !== generation || error instanceof SupersededError) return true;
  if (!(error instanceof APIError)) return false;
  let message: string;
  if (error.status === 401 && error.code === 'mfa_token_invalid') message = '時間が切れました。もう一度ログインしてください。';
  else if (error.status === 503 && error.code === 'signin_unavailable') {
    message = enrolling
      ? '登録は完了しました。もう一度ログインしてください。リカバリーコードは、ログイン後に二段階認証の画面で再発行してください。'
      : 'ログインできませんでした。もう一度ログインしてください。';
  } else return false;
  generation += 1;
  anonymous(message);
  return true;
}

// mfaPost sends a request of the pending sign-in with its MFA token. It
// throws SupersededError when the sign-in is over, before or after the
// request.
async function mfaPost(path: MFABeginPath | MFAVerifyPath | MFAEnrollPath, body: object, expected: number, enrolling = false): Promise<Response> {
  isCurrent(expected);
  const pending = state.mfa;
  if (!pending) throw new SupersededError();
  try {
    const response = await post(path, { ...body, mfa_token: pending.token });
    isCurrent(expected);
    return response;
  } catch (error) {
    if (mfaEnded(error, expected, enrolling)) throw new SupersededError();
    throw error;
  }
}

function connectionMessage(error: unknown): string {
  if (error instanceof APIError && error.status === 429) return '試行回数が上限に達しました。1分ほど待ってから、もう一度お試しください。';
  if (error instanceof APIError && error.status === 403) return '接続先の設定を確認できませんでした。運用者に連絡してください。';
  return 'サーバーに接続できませんでした。少し待ってから、もう一度お試しください。';
}

// restore reads GET /api/v1/me to learn whether the session cookie of this
// browser signs a user in: on a page load, on a retry and when another tab
// signed in. 401 means signed out.
async function restore(): Promise<void> {
  const expected = generation;
  enrolledSession = null;
  publish({ ...state, status: 'loading', error: null, mfa: null });
  try {
    const user = await withAuthLock(async () => {
      isCurrent(expected);
      return await request<unknown>('/api/v1/me');
    });
    isCurrent(expected);
    if (!isUser(user)) throw new Error('Unexpected user response');
    publish({ status: 'authenticated', user, busy: null, error: null, mfa: null });
  } catch (error) {
    if (expected !== generation || error instanceof SupersededError) return;
    if (error instanceof APIError && error.status === 401) return;
    publish({ status: 'unavailable', user: null, busy: null, error: connectionMessage(error), mfa: null });
  }
}

// announceOIDCLogin runs after the first restore of a page load. When the
// tab started a single sign-on sign-in and is now signed in, the other tabs
// read the new session. The flag is removed either way.
function announceOIDCLogin(): void {
  let started = false;
  try {
    started = sessionStorage.getItem(oidcLoginFlag) !== null;
    sessionStorage.removeItem(oidcLoginFlag);
  } catch {
    return;
  }
  if (started && state.status === 'authenticated') channel?.postMessage('session-changed');
}

channel?.addEventListener('message', (event: MessageEvent<unknown>) => {
  if (event.data !== 'logout' && event.data !== 'session-changed') return;
  generation += 1;
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
    void restore().then(announceOIDCLogin);
  },
  retry(): void { void restore(); },
  async login(email: string, password: string): Promise<void> {
    const expected = ++generation;
    enrolledSession = null;
    publish({ status: 'anonymous', user: null, busy: 'login', error: null, mfa: null });
    try {
      const signedIn = await withAuthLock(async () => {
        isCurrent(expected);
        const signIn = await readSignIn(await post('/auth/login', { email, password }));
        // A second factor is needed: no session, nothing to tell the other
        // tabs and no profile to read.
        if (signIn.mfa) {
          pend(signIn.mfa, expected);
          return false;
        }
        accept(signIn.session, expected);
        channel?.postMessage('session-changed');
        return true;
      });
      if (signedIn) await verifyProfile(expected);
    } catch (error) {
      if (expected !== generation || error instanceof SupersededError) return;
      publish({ status: 'anonymous', user: null, busy: null, mfa: null, error: error instanceof APIError && error.status === 401
        ? 'メールアドレスまたはパスワードを確認してください。' : connectionMessage(error) });
    }
  },
  // completeLink accepts an invitation or completes a password reset, which
  // answer like login, and signs in with the result in the same way as
  // login. Failures are thrown with their error code for the caller to
  // describe; the state is then anonymous. When a second factor is needed
  // it resolves with the state's mfa set and the tab signed out.
  async completeLink(path: '/auth/invitations/accept' | '/auth/password-resets/complete', token: string, password: string): Promise<void> {
    const expected = ++generation;
    enrolledSession = null;
    publish({ status: 'anonymous', user: null, busy: 'login', error: null, mfa: null });
    let signedIn: boolean;
    try {
      signedIn = await withAuthLock(async () => {
        isCurrent(expected);
        const signIn = await readSignIn(await post(path, { token, password }));
        if (signIn.mfa) {
          pend(signIn.mfa, expected);
          return false;
        }
        accept(signIn.session, expected);
        channel?.postMessage('session-changed');
        return true;
      });
    } catch (error) {
      if (expected === generation) publish({ status: 'anonymous', user: null, busy: null, error: null, mfa: null });
      throw error;
    }
    if (signedIn) await verifyProfile(expected);
  },
  // beginMFA starts a step of the pending sign-in that changes nothing: the
  // options of a passkey or security key, or the secret of an authenticator
  // app. The caller checks the answer. Like the other MFA requests it throws
  // an error isSuperseded reports when the sign-in ended meanwhile; an
  // expired MFA token ends it with a message for the login form.
  async beginMFA(path: MFABeginPath): Promise<unknown> {
    const expected = generation;
    const data = await (await mfaPost(path, {}, expected)).json() as unknown;
    isCurrent(expected);
    return data;
  },
  // verifyMFA gives the second factor of the pending sign-in and signs in
  // like login. leave runs right before the tab becomes signed in, for the
  // caller to leave a screen that is shown signed out. Failures are thrown
  // with their error code for the caller to describe.
  async verifyMFA(path: MFAVerifyPath, body: object, leave?: () => void): Promise<void> {
    const expected = generation;
    await withAuthLock(async () => {
      const data = await readAuth(await mfaPost(path, body, expected));
      isCurrent(expected);
      leave?.();
      accept(data, expected);
      channel?.postMessage('session-changed');
    });
    await verifyProfile(expected);
  },
  // enrollMFA stores the first second factor of a user that must have one
  // and returns the recovery codes. The server has started the session, but
  // the tab stays signed out until finishEnrollment: the codes are shown
  // only now and must be saved first.
  async enrollMFA(path: MFAEnrollPath, body: object): Promise<string[]> {
    const expected = generation;
    return withAuthLock(async () => {
      const { session, codes } = readEnrolled(await (await mfaPost(path, body, expected, true)).json() as unknown);
      isCurrent(expected);
      enrolledSession = { data: session, expected };
      return codes;
    });
  },
  // finishEnrollment signs in with the session enrollMFA held back, once
  // the user has saved the recovery codes. leave is as in verifyMFA.
  async finishEnrollment(leave?: () => void): Promise<void> {
    const held = enrolledSession;
    if (!held) throw new SupersededError();
    isCurrent(held.expected);
    leave?.();
    accept(held.data, held.expected);
    channel?.postMessage('session-changed');
    await verifyProfile(held.expected);
  },
  // cancelMFA drops the pending sign-in; the login form is shown again.
  cancelMFA(): void {
    if (!state.mfa) return;
    generation += 1;
    anonymous();
  },
  // reloadProfile reads /api/v1/me again, for example after the role changed.
  async reloadProfile(): Promise<void> {
    await verifyProfile(generation, false);
  },
  async logout(): Promise<void> {
    const expected = ++generation;
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

// request sends a private API request. The browser adds the session cookie
// (same origin); nothing else identifies the user. A 401 means the session
// ended, at its limits, by a logout in another tab or by the operator, and
// signs the tab out, back to the login screen.
export async function request<T>(path: `/api/v1/${string}`, init?: RequestInit): Promise<T> {
  if (state.busy === 'logout') throw new SupersededError();
  const expected = generation;
  const response = await fetch(path, { ...init, credentials: 'same-origin', cache: 'no-store' });
  isCurrent(expected);
  if (!response.ok) {
    if (response.status === 401) anonymous();
    throw new APIError(response.status, await errorCode(response));
  }
  // 204 No Content has no body to parse.
  if (response.status === 204) return undefined as T;
  return await response.json() as T;
}

// cookieMessage explains a sign-in whose session the next request did not
// carry: the browser did not keep the session cookie.
const cookieMessage = 'ログインを保持できませんでした。このブラウザーで Cookie を保存できるか確認して、もう一度ログインしてください。';

// verifyProfile reads /api/v1/me again. Right after a sign-in (signedIn), a
// 401 means that the browser dropped the session cookie the server just
// set; the tab goes back to the login form with cookieMessage.
async function verifyProfile(expected: number, signedIn = true): Promise<void> {
  try {
    const user = await request<unknown>('/api/v1/me');
    isCurrent(expected);
    if (!isUser(user)) throw new Error('Unexpected user response');
    publish({ ...state, user });
  } catch (error) {
    if (expected !== generation || error instanceof SupersededError) return;
    if (error instanceof APIError && error.status === 401) {
      if (signedIn) publish({ ...state, error: cookieMessage });
      return;
    }
    publish({ ...state, error: 'アカウント情報を確認できませんでした。接続を確認してページを再読み込みしてください。' });
  }
}

// inspectLink reads what an invitation or password reset link is for, before
// signing in. It does not touch the authentication state.
export async function inspectLink(path: '/auth/invitations/inspect' | '/auth/password-resets/inspect', token: string): Promise<unknown> {
  return await (await post(path, { token })).json() as unknown;
}
