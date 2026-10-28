import { useCallback, useEffect, useId, useState } from 'react';
import { Link } from 'react-router';
import { aiNeedsSetup, createAIClient } from './ai';
import { APIError, request } from './auth';
import { chatworkErrorMessage, chatworkStatusMessage } from './automation';
import { dateTime } from './format';
import { TokenForm } from './GitHubSettings';
import { createIntegrationsClient } from './integrations';
import type { ChatworkSettings as ChatworkOptions, ChatworkState } from './integrations';
import Phrased from './Phrased';

const client = createIntegrationsClient(request);
const ai = createAIClient(request);

const tokenLabel = 'API トークン';

// toggles are the settings of a connection, in the order shown. hint
// explains what turning one off changes.
const toggles: readonly { key: keyof ChatworkOptions; label: string; hint?: string }[] = [
  { key: 'detect_mentions', label: 'グループチャットで自分宛ての TO・返信・TO ALL を検出する' },
  { key: 'detect_dms', label: 'ダイレクトチャットを検出する', hint: '無効にすると、ダイレクトチャットの中の TO と返信だけを検出します。' },
  { key: 'import_tasks', label: '自分に割り当てられたタスクを取り込む', hint: 'AI の設定がなくても取り込みます。Astovia で完了にしても、Chatwork のタスクは完了になりません。' },
  { key: 'immediate_notify', label: '検出したらすぐ通知する' },
];

// ChatworkSettings connects a Chatwork API token. The token is sent once
// and never shown again; only its last four characters are.
export default function ChatworkSettings() {
  const [state, setState] = useState<ChatworkState | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [formError, setFormError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [replacing, setReplacing] = useState(false);
  // aiMissing is set when nothing can be detected for want of AI settings;
  // it stays unset when they could not be read.
  const [aiMissing, setAIMissing] = useState(false);
  const headingID = useId();
  const toggleID = useId();

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      setState(await client.getChatwork());
    } catch {
      setError('Chatwork 連携の状態を読み込めませんでした。接続を確認して、もう一度お試しください。');
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
      setState(await client.connectChatwork(token));
      setReplacing(false);
    } catch (failure) {
      setFormError(chatworkErrorMessage(failure instanceof APIError ? failure.code : null));
    } finally {
      setBusy(false);
    }
  }

  async function update(key: keyof ChatworkOptions, value: boolean): Promise<void> {
    setBusy(true);
    setError(null);
    try {
      setState(await client.updateChatworkSettings({ [key]: value }));
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
      await client.disconnectChatwork();
      setState({ connected: false });
      setReplacing(false);
    } catch {
      setError('接続を解除できませんでした。接続を確認して、もう一度お試しください。');
    } finally {
      setBusy(false);
    }
  }

  return <section className="action-panel settings-panel" id="chatwork" aria-labelledby={headingID}>
    <h2 id={headingID}>Chatwork 連携</h2>
    <p className="intro"><Phrased text="自分宛ての TO・返信・TO ALL やダイレクトチャットを AI が読んで、やることがあればタスクにします。自分に割り当てられた Chatwork のタスクも取り込みます。" /></p>
    {aiMissing ? <p className="notice">Chatwork は AI の設定がないと、TO とダイレクトチャットの検出と自動完了を行いません（タスクの取り込みは行います）。<Link to="/settings/ai">AI を設定する</Link></p> : null}
    {error ? <p className="notice error" role="alert">{error}</p> : null}
    {loading ? <p className="action-loading" aria-live="polite">読み込んでいます…</p> : null}
    {!loading && state && !state.connected ? <>
      <ol className="steps">
        <li>Chatwork の右上のメニューから「サービス連携」を開き、「API トークン」でトークンを発行します。組織によっては、管理者による API の利用の許可が必要です。</li>
        <li>発行したトークンを下に貼り付けて「接続する」を押します。</li>
      </ol>
      <p className="notice">API トークンはアカウントに 1 つです。再発行すると、同じトークンを使っている他のツールも使えなくなります。</p>
      <TokenForm label={tokenLabel} submitLabel="接続する" busy={busy} error={formError} onSubmit={(token) => { void connect(token); }} />
    </> : null}
    {!loading && state?.connected ? <>
      <dl className="settings-facts">
        <div><dt>アカウント</dt><dd>{state.account_name}</dd></div>
        {state.organization_name ? <div><dt>組織</dt><dd>{state.organization_name}</dd></div> : null}
        <div><dt>トークン</dt><dd>…{state.token_last4}</dd></div>
        <div><dt>状態</dt><dd className={state.status === 'error' || state.last_error ? 'status-error' : undefined}>{chatworkStatusMessage(state.status, state.last_error)}</dd></div>
        <div><dt>最終同期</dt><dd>{state.last_synced_at ? dateTime(state.last_synced_at, new Date()) : 'まだ同期していません'}</dd></div>
      </dl>
      {toggles.map((t) => <div key={t.key}>
        <div className="toggle-field">
          <input id={`${toggleID}-${t.key}`} type="checkbox" checked={state[t.key]} disabled={busy} onChange={(e) => { void update(t.key, e.target.checked); }} aria-describedby={t.hint ? `${toggleID}-${t.key}-hint` : undefined} />
          <label htmlFor={`${toggleID}-${t.key}`}>{t.label}</label>
        </div>
        {t.hint ? <p id={`${toggleID}-${t.key}-hint`} className="field-hint">{t.hint}</p> : null}
      </div>)}
      {replacing ? <TokenForm label={tokenLabel} submitLabel="トークンを更新" busy={busy} error={formError} onSubmit={(token) => { void connect(token); }} onCancel={() => { setReplacing(false); setFormError(null); }} /> : <div className="form-actions">
        <button className="button secondary" type="button" disabled={busy} onClick={() => setReplacing(true)}>トークンを更新</button>
        <button className="button secondary danger" type="button" disabled={busy} onClick={() => { void disconnect(); }}>Chatwork との接続を解除</button>
      </div>}
    </> : null}
  </section>;
}
