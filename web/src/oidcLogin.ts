// The single sign-on (OIDC) sign-in form. This module has no state of its
// own, so that the form can be rendered and tested without auth.ts.

// oidcLoginFlag marks, in sessionStorage, a single sign-on (OIDC) sign-in
// started from this tab. The page loaded after the sign-in tells the other
// tabs, as a password login does (auth.ts).
export const oidcLoginFlag = 'astovia-oidc-login';

// oidcLoginPath is where the single sign-on (OIDC) sign-in form is posted.
// It is a real form POST, not a fetch: the server answers with a redirect
// to the provider, which the CSP form-action allows.
export const oidcLoginPath = '/auth/oidc/login';

// oidcReturnTo is the return_to field of the sign-in form: the screen at
// pathname and search, which the server opens again after the sign-in.
export function oidcReturnTo(pathname: string, search: string): string {
  return pathname + search;
}

// markOIDCLogin is called when the sign-in form is submitted, from a
// button: an automatic submission could loop on a failure.
export function markOIDCLogin(): void {
  try {
    sessionStorage.setItem(oidcLoginFlag, '1');
  } catch {
    // Without storage the other tabs notice the session on their next
    // request.
  }
}
