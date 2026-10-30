import { useCallback, useEffect, useId, useState } from 'react';
import { Link, useLocation } from 'react-router';
import { aiNeedsSetup, createAIClient } from './ai';
import { APIError, request } from './auth';
import { gmailErrorMessage, gmailStatusMessage } from './automation';
import CopyField from './CopyField';
import { dateTime } from './format';
import { createIntegrationsClient } from './integrations';
import type { GmailAccount, GmailState } from './integrations';
import Phrased from './Phrased';

const client = createIntegrationsClient(request);
const ai = createAIClient(request);

// connectedEmail reads the address the Google callback screen connected,
// passed in the navigation state.
function connectedEmail(state: unknown): string | null {
  if (typeof state !== 'object' || state === null || !('gmailConnected' in state) || typeof state.gmailConnected !== 'string') return null;
  return state.gmailConnected;
}

function AccountRow({ account, busy, confirming, onToggle, onReconnect, onAsk, onCancel, onDisconnect }: {
  account: GmailAccount;
  busy: boolean;
  confirming: boolean;
  onToggle: (value: boolean) => void;
  onReconnect: () => void;
  onAsk: () => void;
  onCancel: () => void;
  onDisconnect: () => void;
}) {
  const toggleID = useId();
  const failing = account.status === 'error' || account.last_error !== null;
  return <li className="action-item">
    <div className="action-body">
      <p className="action-content">{account.email}</p>
      <dl className="action-meta">
        <div><dt>状態</dt><dd className={failing ? 'status-error' : undefined}>{gmailStatusMessage(account.status, account.last_error)}</dd></div>
        <div><dt>最終同期</dt><dd>{account.last_synced_at ? dateTime(account.last_synced_at, new Date()) : 'まだ同期していません'}</dd></div>
      </dl>
      <div className="toggle-field">
        <input id={toggleID} type="checkbox" checked={account.immediate_notify} disabled={busy} onChange={(e) => onToggle(e.target.checked)} aria-describedby={`${toggleID}-hint`} />
        <label htmlFor={toggleID}>検出したらすぐ通知する</label>
      </div>
      <p id={`${toggleID}-hint`} className="field-hint">重要度が高いものは、オフでも通知します。</p>
    </div>
    <div className="action-buttons">
      {account.status === 'error' ? <button className="button secondary small" type="button" disabled={busy} onClick={onReconnect}>再接続</button> : null}
      {confirming ? <>
        <button className="button secondary small danger" type="button" disabled={busy} onClick={onDisconnect}>本当に解除</button>
        <button className="button secondary small" type="button" disabled={busy} onClick={onCancel}>やめる</button>
      </> : <button className="button secondary small" type="button" disabled={busy} onClick={onAsk}>接続を解除</button>}
    </div>
  </li>;
}

// GmailSettings connects Google accounts (Gmail and Google Workspace) with
// Google's consent screen. The tokens never reach the browser.
export default function GmailSettings() {
  const location = useLocation();
  const [state, setState] = useState<GmailState | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [confirmingID, setConfirmingID] = useState<string | null>(null);
  // aiMissing is set when nothing can be detected for want of AI settings;
  // it stays unset when they could not be read.
  const [aiMissing, setAIMissing] = useState(false);
  const connected = connectedEmail(location.state);
  const headingID = useId();

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      setState(await client.getGmail());
    } catch {
      setError('メール連携の状態を読み込めませんでした。接続を確認して、もう一度お試しください。');
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => { void load(); }, [load]);

  useEffect(() => {
    let active = true;
    ai.getAI().then((state) => { if (active) setAIMissing(aiNeedsSetup(state)); }, () => {});
    return () => { active = false; };
  }, []);

  // authorize leaves for Google's consent screen.
  async function authorize(accountId?: string): Promise<void> {
    setBusy(true);
    setError(null);
    try {
      window.location.assign(await client.authorizeGmail(accountId));
    } catch (failure) {
      setError(gmailErrorMessage(failure instanceof APIError ? failure.code : null));
      setBusy(false);
    }
  }

  async function toggle(account: GmailAccount, value: boolean): Promise<void> {
    setBusy(true);
    setError(null);
    try {
      const updated = await client.updateGmailAccount(account.id, value);
      setState((current) => current && { ...current, accounts: current.accounts.map((a) => a.id === updated.id ? updated : a) });
    } catch {
      setError('設定を変更できませんでした。接続を確認して、もう一度お試しください。');
    } finally {
      setBusy(false);
    }
  }

  async function disconnect(account: GmailAccount): Promise<void> {
    setBusy(true);
    setError(null);
    try {
      await client.disconnectGmail(account.id);
      setState((current) => current && { ...current, accounts: current.accounts.filter((a) => a.id !== account.id) });
      setConfirmingID(null);
    } catch {
      setError('接続を解除できませんでした。接続を確認して、もう一度お試しください。');
    } finally {
      setBusy(false);
    }
  }

  const full = state ? state.accounts.length >= state.max_accounts : false;

  return <section className="action-panel settings-panel" id="gmail" aria-labelledby={headingID}>
    <h2 id={headingID}>メール連携（Gmail・Google Workspace）</h2>
    <p className="intro"><Phrased text="Google アカウントのメールを AI が読んで、自分がやるべきことがあればタスクにします。自分が返信して用件が済んだと AI が判断したら、自動で完了にします。メールを既読にしたり、送ったりはしません。" /></p>
    {aiMissing ? <p className="notice">メールは AI の設定がないと、検出も自動完了も行いません。<Link to="/settings/ai">AI を設定する</Link></p> : null}
    {connected ? <p className="notice" role="status">{connected} を接続しました</p> : null}
    {error ? <p className="notice error" role="alert">{error}</p> : null}
    {loading ? <p className="action-loading" aria-live="polite">読み込んでいます…</p> : null}
    {!loading && state && !state.available ? <>
      <p className="notice"><Phrased text="メール連携を使うには、Astovia の運用者が Google Cloud で OAuth クライアントを作り、環境変数を設定する必要があります。" /></p>
      <ol className="steps">
        <li>Google Cloud でプロジェクトを作り、Gmail API を有効にします。</li>
        <li>OAuth 同意画面を作り、スコープに gmail.readonly を足します（gmail.com も使うなら公開ステータスを「本番環境」にします）。</li>
        <li>「ウェブアプリケーション」の OAuth クライアントを作り、承認済みのリダイレクト URI に下の URI を登録します。</li>
        <li>クライアント ID とシークレットを ASTOVIA_GOOGLE_CLIENT_ID と ASTOVIA_GOOGLE_CLIENT_SECRET に設定して、Astovia を再起動します。</li>
      </ol>
      <div className="copy-stack">
        <CopyField label="承認済みのリダイレクト URI" value={state.redirect_uri} />
      </div>
      <p className="field-hint">詳しくは README の「メール連携（Gmail・Google Workspace）」を参照してください。</p>
    </> : null}
    {!loading && state && state.accounts.length > 0 ? <ul className="action-list token-list">
      {state.accounts.map((account) => <AccountRow key={account.id} account={account} busy={busy} confirming={confirmingID === account.id}
        onToggle={(value) => { void toggle(account, value); }}
        onReconnect={() => { void authorize(account.id); }}
        onAsk={() => setConfirmingID(account.id)}
        onCancel={() => setConfirmingID(null)}
        onDisconnect={() => { void disconnect(account); }} />)}
    </ul> : null}
    {!loading && state?.available ? <>
      {state.accounts.length === 0 ? <p className="action-empty"><Phrased text="接続している Google アカウントはありません。" /></p> : null}
      <p className="field-hint"><Phrased text="接続の途中で『このアプリは Google で確認されていません』と表示されたら、『詳細』から進んでください（運用者が自分で作ったクライアントのため）。" /></p>
      <div className="form-actions">
        <button className="button primary" type="button" disabled={busy || full} onClick={() => { void authorize(); }}>Google アカウントを追加</button>
      </div>
      {full ? <p className="field-hint">{state.max_accounts} 件まで接続できます。</p> : null}
    </> : null}
  </section>;
}
