import { useEffect, useId, useState } from 'react';
import { request } from './auth';
import { consentErrorMessage, createOAuthClient, needsReload } from './oauth';
import type { Authorization, ConsentDecision } from './oauth';

const client = createOAuthClient(request);

// authorizationQuery is the query string of the consent screen as the AI
// client sent it, without the leading ?.
function authorizationQuery(): string {
  return location.search.replace(/^\?/, '');
}

// ConsentPage asks the signed-in user to approve an AI client's
// authorization request (/oauth/authorize?...). The request is checked by the
// server when the screen opens and again with the decision.
export default function ConsentPage({ email }: { email: string }) {
  const [query] = useState(authorizationQuery);
  const [authorization, setAuthorization] = useState<Authorization | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [reload, setReload] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const headingID = useId();

  useEffect(() => {
    let active = true;
    client.getAuthorization(query).then((data) => {
      if (active) setAuthorization(data);
    }, (failure: unknown) => {
      if (active) setError(consentErrorMessage(failure));
    });
    return () => { active = false; };
  }, [query]);

  const consent = authorization?.status === 'ok' ? authorization : null;
  const rejection = authorization?.status === 'error' ? authorization : null;

  async function decide(decision: ConsentDecision): Promise<void> {
    if (submitting || !consent) return;
    setSubmitting(true);
    setError(null);
    try {
      const url = await client.decide(query, decision, consent);
      // The buttons stay disabled while the browser leaves the page.
      window.location.assign(url);
    } catch (failure) {
      setSubmitting(false);
      setError(consentErrorMessage(failure));
      // The decision was refused for the user or the client shown; the
      // screen is read again for the ones of now.
      if (needsReload(failure)) setReload(true);
    }
  }

  return <section className="gate-card consent-card" aria-labelledby={headingID} aria-busy={!authorization && !error}>
    <p className="eyebrow">AI クライアントの接続</p>
    {consent ? <>
      <h1 id={headingID}><span className="consent-client">{consent.client_name}</span> が Astovia への接続を求めています</h1>
      <dl className="consent-facts">
        {consent.client_host ? <div><dt>発行元</dt><dd>{consent.client_host}</dd></div> : null}
        <div><dt>許可後の戻り先</dt><dd>{consent.redirect_host}{consent.redirect_loopback ? <span className="consent-note">このコンピューター上のアプリに戻ります</span> : null}</dd></div>
      </dl>
      {consent.client_host ? null : <p className="consent-warning">このアプリ名は相手が名乗ったもので、Astovia では確認していません。</p>}
      <p className="consent-warning">表示しているドメインをよく確認してください。</p>
      <h2 className="consent-subheading">許可される内容</h2>
      <ul className="consent-scopes">
        <li>タスクの閲覧・作成・更新</li>
        <li>連携中の Chatwork・Slack の会話と Gmail のメールの閲覧</li>
        <li>再ログインなしで接続を維持（しばらく使わないと切れます）</li>
      </ul>
      <p className="consent-account"><span className="user-email">{email}</span> としてログイン中</p>
    </> : <h1 id={headingID}>AI クライアントの接続</h1>}
    {rejection ? <p className="notice error" role="alert">この接続リクエストは受け付けられません（{rejection.error}）。AI クライアントから接続をやり直してください。</p> : null}
    {error ? <p className="notice error" role="alert">{error}</p> : null}
    {!authorization && !error ? <p className="action-loading" aria-live="polite">読み込んでいます…</p> : null}
    {consent && !reload ? <div className="consent-actions">
      <button className="button primary" type="button" disabled={submitting} onClick={() => { void decide('approve'); }}>許可する</button>
      <button className="button secondary" type="button" disabled={submitting} onClick={() => { void decide('deny'); }}>拒否する</button>
    </div> : null}
    {reload ? <div className="consent-actions">
      <button className="button primary" type="button" onClick={() => window.location.reload()}>読み込み直す</button>
    </div> : null}
    {rejection ? <div className="consent-actions">
      <button className="button secondary" type="button" onClick={() => window.location.assign(rejection.redirect_url)}>{rejection.redirect_host || 'AI クライアント'} に戻る</button>
    </div> : null}
  </section>;
}
