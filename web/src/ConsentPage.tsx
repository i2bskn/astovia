import { useEffect, useId, useState } from 'react';
import { request } from './auth';
import { createOAuthClient, isBadRequest } from './oauth';
import type { Consent } from './oauth';

const client = createOAuthClient(request);

const invalidMessage = 'この接続リクエストは無効か期限切れです。AIクライアントから接続をやり直してください。';
const connectionMessage = 'サーバーに接続できませんでした。少し待ってから、もう一度お試しください。';

function authorizationRequest(): string {
  return new URLSearchParams(location.search).get('request') ?? '';
}

// ConsentPage asks the signed-in user to approve an AI client's
// authorization request (/admin/oauth/authorize?request=...).
export default function ConsentPage({ email }: { email: string }) {
  const [value] = useState(authorizationRequest);
  const [consent, setConsent] = useState<Consent | null>(null);
  const [error, setError] = useState<string | null>(value ? null : invalidMessage);
  const [submitting, setSubmitting] = useState(false);
  const headingID = useId();

  useEffect(() => {
    if (!value) return;
    let active = true;
    client.getConsent(value).then((data) => {
      if (active) setConsent(data);
    }, (failure: unknown) => {
      if (active) setError(isBadRequest(failure) ? invalidMessage : connectionMessage);
    });
    return () => { active = false; };
  }, [value]);

  async function decide(approve: boolean): Promise<void> {
    if (submitting) return;
    setSubmitting(true);
    setError(null);
    try {
      const url = await client.decide(value, approve);
      // The buttons stay disabled while the browser leaves the page.
      window.location.assign(url);
    } catch (failure) {
      setSubmitting(false);
      setError(isBadRequest(failure) ? invalidMessage : connectionMessage);
    }
  }

  const offline = consent?.scopes.includes('offline_access') ?? false;

  return <section className="gate-card consent-card" aria-labelledby={headingID} aria-busy={!consent && !error}>
    <p className="eyebrow">AI クライアントの接続</p>
    {consent ? <>
      <h1 id={headingID}><span className="consent-client">{consent.client_name}</span> が Astovia への接続を求めています</h1>
      <dl className="consent-facts">
        {consent.client_kind === 'cimd' ? <div><dt>発行元</dt><dd>{consent.client_host}</dd></div> : null}
        <div><dt>許可後の戻り先</dt><dd>{consent.redirect_host}{consent.redirect_loopback ? <span className="consent-note">このコンピューター上のアプリに戻ります</span> : null}</dd></div>
      </dl>
      {consent.client_kind === 'dcr' ? <p className="consent-warning">このアプリ名は相手が名乗ったもので、Astovia では確認していません。</p> : null}
      <p className="consent-warning">表示しているドメインをよく確認してください。</p>
      <h2 className="consent-subheading">許可される内容</h2>
      <ul className="consent-scopes">
        <li>タスクの閲覧・作成・更新</li>
        <li>{offline ? '再ログインなしで接続を維持（使うたびに最長90日延長）' : '7日後に再接続が必要'}</li>
      </ul>
      <p className="consent-account"><span className="user-email">{email}</span> としてログイン中</p>
    </> : <h1 id={headingID}>AI クライアントの接続</h1>}
    {error ? <p className="notice error" role="alert">{error}</p> : null}
    {!consent && !error ? <p className="action-loading" aria-live="polite">読み込んでいます…</p> : null}
    {consent ? <div className="consent-actions">
      <button className="button primary" type="button" disabled={submitting} onClick={() => { void decide(true); }}>許可する</button>
      <button className="button secondary" type="button" disabled={submitting} onClick={() => { void decide(false); }}>拒否する</button>
    </div> : null}
  </section>;
}
