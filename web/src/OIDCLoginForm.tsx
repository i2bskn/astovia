import { markOIDCLogin, oidcLoginPath } from './oidcLogin.ts';

// OIDCLoginForm starts the single sign-on (OIDC) sign-in with a form POST
// to the server, which redirects to the provider; afterwards the server
// opens returnTo. It is only submitted from its button: an automatic
// submission could loop on a failure.
export default function OIDCLoginForm({ returnTo, label }: { returnTo: string; label: string }) {
  return <form method="post" action={oidcLoginPath} onSubmit={markOIDCLogin}>
    <input type="hidden" name="return_to" value={returnTo} />
    <button className="button primary block" type="submit">{label}</button>
  </form>;
}
