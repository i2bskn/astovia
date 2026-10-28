import { useEffect, useId, useState } from 'react';
import { Link, useNavigate } from 'react-router';
import { APIError, request } from './auth';
import { gmailErrorMessage } from './automation';
import { completeOnce, googleCallback } from './googleCallback';
import { createIntegrationsClient } from './integrations';
import Phrased from './Phrased';

const client = createIntegrationsClient(request);

// GoogleCallbackPage completes the connection of a Google account with what
// Google sent back to /integrations/google/callback (read and removed from
// the URL when the page loaded, see googleCallback.ts). The request is sent
// once per result, however often the screen renders. A user who was signed
// out signs in first and comes back here with the result kept in memory.
export default function GoogleCallbackPage() {
  const value = googleCallback();
  const navigate = useNavigate();
  const [error, setError] = useState<string | null>(null);
  const headingID = useId();

  useEffect(() => {
    if (!value) return;
    let active = true;
    completeOnce(value, (v) => client.completeGmail(v.state, 'code' in v ? v.code : { error: v.error })).then((account) => {
      if (!active) return;
      void navigate('/settings/integrations', { replace: true, state: { gmailConnected: account.email } });
    }, (failure: unknown) => {
      if (!active) return;
      setError(gmailErrorMessage(failure instanceof APIError ? failure.code : null));
    });
    return () => { active = false; };
  }, [value, navigate]);

  if (!value) {
    return <section className="action-panel settings-panel" aria-labelledby={headingID}>
      <h2 id={headingID}>接続の手続きが見つかりません</h2>
      <p className="intro"><Phrased text="Google アカウントの接続は、連携の設定の「Google アカウントを追加」から始めてください。" /></p>
      <p><Link to="/settings/integrations">連携の設定へ戻る</Link></p>
    </section>;
  }
  if (error) {
    return <section className="action-panel settings-panel" aria-labelledby={headingID}>
      <h2 id={headingID}>Google アカウントを接続できませんでした</h2>
      <p className="notice error" role="alert">{error}</p>
      <p><Link to="/settings/integrations">連携の設定へ戻る</Link></p>
    </section>;
  }
  return <section className="action-panel settings-panel" aria-labelledby={headingID} aria-busy="true">
    <h2 id={headingID}>メール連携</h2>
    <p className="action-loading" aria-live="polite">Google アカウントを接続しています…</p>
  </section>;
}
