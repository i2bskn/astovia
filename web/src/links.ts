// Invitation and password reset links carry their token in the URL fragment
// (/invite#inv_…, /reset-password#pwr_…), so that it never reaches server
// logs or a Referer. The token is read once when the page loads, before the
// router is created, and removed from the URL at once.

export type LinkPage = 'invite' | 'reset';

// linkPageAt returns the link screen of a pathname, with or without one
// trailing slash, or null.
export function linkPageAt(pathname: string): LinkPage | null {
  const path = pathname.endsWith('/') ? pathname.slice(0, -1) : pathname;
  if (path === '/invite') return 'invite';
  if (path === '/reset-password') return 'reset';
  return null;
}

const tokenPatterns: Record<LinkPage, RegExp> = {
  invite: /^inv_[A-Za-z0-9_-]{43}$/,
  reset: /^pwr_[A-Za-z0-9_-]{43}$/,
};

// tokenFromHash returns the token of a location hash ("#inv_…") when it has
// the form of the page's tokens, or null.
export function tokenFromHash(hash: string, page: LinkPage): string | null {
  const value = hash.startsWith('#') ? hash.slice(1) : hash;
  return tokenPatterns[page].test(value) ? value : null;
}

interface LinkLocation {
  pathname: string;
  search: string;
  hash: string;
}

interface LinkHistory {
  state: unknown;
  replaceState(data: unknown, unused: string, url?: string | URL | null): void;
}

let captured: { page: LinkPage; token: string | null } | null = null;

// captureLinkToken keeps the token of a link screen in this module and
// removes the fragment from the address bar and the history entry. Only the
// first call reads the URL; later calls (a second render, a sign-out) keep
// the token already read.
export function captureLinkToken(location: LinkLocation, history: LinkHistory): void {
  if (captured) return;
  const page = linkPageAt(location.pathname);
  if (!page) return;
  captured = { page, token: tokenFromHash(location.hash, page) };
  if (location.hash) history.replaceState(history.state, '', location.pathname + location.search);
}

// linkToken returns the token read for page, or null when the page was not
// opened with a valid token.
export function linkToken(page: LinkPage): string | null {
  return captured?.page === page ? captured.token : null;
}

// LinkFailure classifies a failed request of the link screens.
export type LinkFailure = 'invalid' | 'email_taken' | 'rate_limited' | 'password' | 'unavailable' | 'network';

// linkFailure reads the status and error code of a failed request. auth.ts's
// APIError is matched by shape so that this module does not import auth.ts
// at runtime.
export function linkFailure(error: unknown): LinkFailure {
  if (!(error instanceof Error) || !('status' in error) || typeof error.status !== 'number') return 'network';
  const code = 'code' in error && typeof error.code === 'string' ? error.code : null;
  if (error.status === 404 && (code === 'invitation_invalid' || code === 'reset_invalid')) return 'invalid';
  if (error.status === 409 && code === 'email_taken') return 'email_taken';
  if (error.status === 429) return 'rate_limited';
  if (error.status === 400) return 'password';
  if (error.status === 503) return 'unavailable';
  return 'network';
}

// passwordProblem checks a new password and its confirmation the way the
// server does (12–72 bytes of UTF-8), and returns what is wrong or null.
export function passwordProblem(password: string, confirmation: string): string | null {
  if (!password || !confirmation) return '新しいパスワードを2回入力してください。';
  const bytes = new TextEncoder().encode(password).length;
  if (bytes < 12 || bytes > 72) return 'パスワードは12〜72バイトで入力してください（英数字なら12〜72文字）。';
  if (password !== confirmation) return '確認用のパスワードが一致しません。';
  return null;
}
