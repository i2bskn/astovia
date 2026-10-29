// The server signs users in either with passwords or with Cloudflare Access
// (GET /auth/config). The screens read the mode before showing a sign-in
// form; when it cannot be read they do not assume passwords.

export type AuthConfig =
  | { mode: 'password' }
  | { mode: 'cloudflare_access'; logoutURL: string };

export interface AuthConfigState {
  status: 'loading' | 'ready' | 'failed';
  config: AuthConfig | null;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

// isAccessLogoutURL accepts only the logout URL of an Access team domain:
// https://<team>.cloudflareaccess.com/cdn-cgi/access/logout.
function isAccessLogoutURL(value: string): boolean {
  let url: URL;
  try {
    url = new URL(value);
  } catch {
    return false;
  }
  return url.protocol === 'https:' && url.hostname.endsWith('.cloudflareaccess.com') && url.port === '' &&
    url.username === '' && url.password === '' && url.pathname === '/cdn-cgi/access/logout' &&
    url.search === '' && url.hash === '' && url.href === value;
}

// parseAuthConfig reads the body of /auth/config, or returns null when it is
// not one of the two modes.
export function parseAuthConfig(data: unknown): AuthConfig | null {
  if (!isRecord(data)) return null;
  if (data.mode === 'password') return { mode: 'password' };
  if (data.mode === 'cloudflare_access' && typeof data.logout_url === 'string' && isAccessLogoutURL(data.logout_url)) {
    return { mode: 'cloudflare_access', logoutURL: data.logout_url };
  }
  return null;
}

let state: AuthConfigState = { status: 'loading', config: null };
const listeners = new Set<() => void>();

function publish(next: AuthConfigState): void {
  state = next;
  for (const listener of listeners) listener();
}

export const authConfig = {
  subscribe(listener: () => void): () => void {
    listeners.add(listener);
    return () => listeners.delete(listener);
  },
  getSnapshot: (): AuthConfigState => state,
  // load reads the mode once per page load; a failure is shown with a
  // reload button.
  async load(): Promise<void> {
    publish({ status: 'loading', config: null });
    try {
      const response = await fetch('/auth/config', { credentials: 'same-origin', cache: 'no-store' });
      if (!response.ok) throw new Error('Unexpected status');
      const config = parseAuthConfig(await response.json() as unknown);
      if (!config) throw new Error('Unexpected sign-in configuration');
      publish({ status: 'ready', config });
    } catch {
      publish({ status: 'failed', config: null });
    }
  },
};

// isSigninErrorPath reports whether pathname is the sign-in error screen,
// with or without one trailing slash.
export function isSigninErrorPath(pathname: string): boolean {
  return pathname === '/signin-error' || pathname === '/signin-error/';
}

const failedMessage = 'ログインできませんでした。もう一度お試しください。';

const signinMessages: Record<string, string> = {
  failed: failedMessage,
  denied: 'Cloudflare Access でログインが許可されませんでした。許可されたアカウントでログインしているか確認してください。',
  unavailable: 'Cloudflare に接続できませんでした。時間をおいて、もう一度お試しください。',
  disabled: 'このアカウントは無効化されています。この Astovia のオーナーに連絡してください。',
  return_too_long: 'ログイン後に元の画面に戻れませんでした。もう一度ログインしてから、元の画面を開き直してください。',
};

// signinReason returns the reason of the error screen's query, or null when
// it has none or more than one.
export function signinReason(search: string): string | null {
  const reasons = new URLSearchParams(search).getAll('reason');
  return reasons.length === 1 ? reasons[0] ?? null : null;
}

// signinErrorMessage describes a reason of the error screen. Unknown
// reasons get the message of failed.
export function signinErrorMessage(reason: string | null): string {
  if (reason !== null && Object.hasOwn(signinMessages, reason)) return signinMessages[reason] ?? failedMessage;
  return failedMessage;
}
