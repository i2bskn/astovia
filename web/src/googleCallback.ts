// Google sends the user back to /integrations/google/callback with the
// result of the authorization in the query (?state=&code= or
// ?state=&error=). The values are read once when the page loads, before the
// router is created, and removed from the URL at once, so that the code is
// not left in the address bar, the history or a Referer.

export const GOOGLE_CALLBACK_PATH = '/integrations/google/callback';

// GoogleCallback is what Google sent back: a code, or the error of an
// authorization that was not allowed.
export type GoogleCallback = { state: string; code: string } | { state: string; error: string };

// isGoogleCallbackPath reports whether pathname is the callback, with or
// without one trailing slash.
export function isGoogleCallbackPath(pathname: string): boolean {
  const path = pathname.endsWith('/') ? pathname.slice(0, -1) : pathname;
  return path === GOOGLE_CALLBACK_PATH;
}

const statePattern = /^gst_[A-Za-z0-9_-]{43}$/;
const errorPattern = /^[a-z_]{1,100}$/;

// validCode accepts 1–2048 printable ASCII characters without spaces.
function validCode(value: string): boolean {
  if (value.length < 1 || value.length > 2048) return false;
  for (let i = 0; i < value.length; i++) {
    const c = value.charCodeAt(i);
    if (c < 0x21 || c > 0x7e) return false;
  }
  return true;
}

// callbackFromSearch returns the result in a location search when it has
// the form Astovia expects: a state and either a code or an error. Other
// parameters Google adds (scope, authuser…) are ignored.
export function callbackFromSearch(search: string): GoogleCallback | null {
  const params = new URLSearchParams(search);
  const state = params.get('state');
  const code = params.get('code');
  const error = params.get('error');
  if (state === null || !statePattern.test(state) || params.getAll('state').length !== 1) return null;
  if (code !== null && error === null && params.getAll('code').length === 1 && validCode(code)) return { state, code };
  if (error !== null && code === null && params.getAll('error').length === 1 && errorPattern.test(error)) return { state, error };
  return null;
}

interface CallbackLocation {
  pathname: string;
  search: string;
  hash: string;
}

interface CallbackHistory {
  state: unknown;
  replaceState(data: unknown, unused: string, url?: string | URL | null): void;
}

let captured: { value: GoogleCallback | null } | null = null;

// captureGoogleCallback keeps the result of the callback in this module and
// removes the query from the address bar and the history entry. Only the
// first call reads the URL; later calls keep what was read.
export function captureGoogleCallback(location: CallbackLocation, history: CallbackHistory): void {
  if (captured || !isGoogleCallbackPath(location.pathname)) return;
  captured = { value: callbackFromSearch(location.search) };
  if (location.search) history.replaceState(history.state, '', location.pathname + location.hash);
}

// googleCallback returns the result read from the callback URL, or null
// when the page was not opened from Google with a valid one (opened
// directly, or reloaded).
export function googleCallback(): GoogleCallback | null {
  return captured?.value ?? null;
}

let completion: { value: GoogleCallback; promise: Promise<unknown> } | null = null;

// completeOnce completes a result once: calls with the same result (a
// second effect run, a new render) get the first call's promise, so that
// the request is sent once.
export function completeOnce<T>(value: GoogleCallback, complete: (value: GoogleCallback) => Promise<T>): Promise<T> {
  if (completion?.value !== value) completion = { value, promise: complete(value) };
  return completion.promise as Promise<T>;
}
