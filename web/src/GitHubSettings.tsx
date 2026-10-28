import { useCallback, useEffect, useId, useState } from 'react';
import type { FormEvent } from 'react';
import { Link } from 'react-router';
import { aiNeedsSetup, createAIClient } from './ai';
import { APIError, request } from './auth';
import { dateTime } from './format';
import { GITHUB_TOKEN_URL, githubErrorMessage, githubStatusMessage } from './automation';
import { createIntegrationsClient } from './integrations';
import type { GitHubState } from './integrations';
import Phrased from './Phrased';

const client = createIntegrationsClient(request);
const ai = createAIClient(request);

function formatDate(value: string): string {
  return dateTime(value, new Date());
}

// TokenForm asks for a token. label names the kind of token (a GitHub
// classic personal access token unless given).
export function TokenForm({ label = 'Personal access token (classic)', submitLabel, busy, error, onSubmit, onCancel }: {
  label?: string;
  submitLabel: string;
  busy: boolean;
  error: string | null;
  onSubmit: (token: string) => void;
  onCancel?: () => void;
}) {
  const [token, setToken] = useState('');
  const [validation, setValidation] = useState<string | null>(null);
  const inputID = useId();
  const errorID = useId();
  const message = validation ?? error;

  function submit(event: FormEvent<HTMLFormElement>): void {
    event.preventDefault();
    if (busy) return;
    const value = token.trim();
    if (!value) {
      setValidation('トークンを入力してください。');
      return;
    }
    setValidation(null);
    onSubmit(value);
  }

  return <form className="action-form" onSubmit={submit} aria-busy={busy}>
    <div className="field">
      <label htmlFor={inputID}>{label}</label>
      <input id={inputID} name="token" type="password" autoComplete="off" autoCapitalize="none" spellCheck={false} maxLength={255} value={token} onChange={(e) => { setToken(e.target.value); setValidation(null); }} disabled={busy} aria-describedby={message ? errorID : undefined} />
    </div>
    {message ? <p id={errorID} className="notice error" role="alert">{message}</p> : null}
    <div className="form-actions">
      {onCancel ? <button className="button secondary" type="button" disabled={busy} onClick={onCancel}>やめる</button> : null}
      <button className="button primary add-button" type="submit" disabled={busy}>{busy ? '確認中…' : submitLabel}</button>
    </div>
  </form>;
}

// GitHubSettings connects a GitHub classic personal access token. The token
// is sent once and never shown again; only its last four characters are.
export default function GitHubSettings() {
  const [state, setState] = useState<GitHubState | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [formError, setFormError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [replacing, setReplacing] = useState(false);
  // aiMissing is set when mentions cannot be detected for want of AI
  // settings; it stays unset when they could not be read.
  const [aiMissing, setAIMissing] = useState(false);
  const headingID = useId();
  const toggleID = useId();

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      setState(await client.getGitHub());
    } catch {
      setError('GitHub 連携の状態を読み込めませんでした。接続を確認して、もう一度お試しください。');
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

  async function connect(token: string): Promise<void> {
    setBusy(true);
    setFormError(null);
    try {
      setState(await client.connectGitHub(token));
      setReplacing(false);
    } catch (failure) {
      setFormError(githubErrorMessage(failure instanceof APIError ? failure.code : null));
    } finally {
      setBusy(false);
    }
  }

  async function toggle(immediate: boolean): Promise<void> {
    setBusy(true);
    setError(null);
    try {
      setState(await client.setGitHubImmediateNotify(immediate));
    } catch {
      setError('設定を変更できませんでした。接続を確認して、もう一度お試しください。');
    } finally {
      setBusy(false);
    }
  }

  async function disconnect(): Promise<void> {
    setBusy(true);
    setError(null);
    try {
      await client.disconnectGitHub();
      setState({ connected: false });
      setReplacing(false);
    } catch {
      setError('接続を解除できませんでした。接続を確認して、もう一度お試しください。');
    } finally {
      setBusy(false);
    }
  }

  return <section className="action-panel settings-panel" id="github" aria-labelledby={headingID}>
    <h2 id={headingID}>GitHub 連携</h2>
    <p className="intro"><Phrased text="自分宛てのレビュー依頼と割り当てを見つけてタスクにし、PR や Issue が片付いたら自動で完了にします。自分へのメンションは AI が読んで、やることがあればタスクにします。" /></p>
    {aiMissing ? <p className="notice">メンションの検出には AI の設定が必要です。<Link to="/settings/ai">AI を設定する</Link></p> : null}
    {error ? <p className="notice error" role="alert">{error}</p> : null}
    {loading ? <p className="action-loading" aria-live="polite">読み込んでいます…</p> : null}
    {!loading && state && !state.connected ? <>
      <ol className="steps">
        <li><a href={GITHUB_TOKEN_URL} rel="noopener noreferrer" target="_blank">GitHub のトークン作成ページ</a>を開き、classic の personal access token を作ります。スコープは <span className="nowrap"><span className="inline-code">repo</span> と <span className="inline-code">notifications</span></span> を選びます。</li>
        <li>作ったトークンを下に貼り付けて「接続する」を押します。</li>
      </ol>
      <TokenForm submitLabel="接続する" busy={busy} error={formError} onSubmit={(token) => { void connect(token); }} />
    </> : null}
    {!loading && state?.connected ? <>
      <dl className="settings-facts">
        <div><dt>アカウント</dt><dd>{state.login}</dd></div>
        <div><dt>トークン</dt><dd>…{state.token_last4}</dd></div>
        <div><dt>スコープ</dt><dd>{state.scopes.join(', ') || 'なし'}</dd></div>
        <div><dt>状態</dt><dd className={state.status === 'error' || state.last_error ? 'status-error' : undefined}>{githubStatusMessage(state.status, state.last_error)}</dd></div>
        <div><dt>最終同期</dt><dd>{state.last_synced_at ? formatDate(state.last_synced_at) : 'まだ同期していません'}</dd></div>
      </dl>
      <div className="toggle-field">
        <input id={toggleID} type="checkbox" checked={state.immediate_notify} disabled={busy} onChange={(e) => { void toggle(e.target.checked); }} />
        <label htmlFor={toggleID}>検出したらすぐ通知する</label>
      </div>
      {replacing ? <TokenForm submitLabel="トークンを更新" busy={busy} error={formError} onSubmit={(token) => { void connect(token); }} onCancel={() => { setReplacing(false); setFormError(null); }} /> : <div className="form-actions">
        <button className="button secondary" type="button" disabled={busy} onClick={() => setReplacing(true)}>トークンを更新</button>
        <button className="button secondary danger" type="button" disabled={busy} onClick={() => { void disconnect(); }}>GitHub との接続を解除</button>
      </div>}
    </> : null}
  </section>;
}
