import { useCallback, useEffect, useId, useState } from 'react';
import { Link } from 'react-router';
import { aiNeedsSetup, createAIClient } from './ai';
import { APIError, request } from './auth';
import { SLACK_TOKEN_PREFIX, httpsURL, slackErrorMessage, slackStatusMessage } from './automation';
import CopyField from './CopyField';
import { dateTime } from './format';
import { TokenForm } from './GitHubSettings';
import { createIntegrationsClient } from './integrations';
import type { SlackSettings as SlackOptions, SlackState } from './integrations';
import Phrased from './Phrased';
import { SLACK_APP_MANIFEST, SLACK_APP_URL } from './slackManifest';

const client = createIntegrationsClient(request);
const ai = createAIClient(request);

const tokenLabel = 'User OAuth Token';

// toggles are the settings of a connection, in the order shown. hint
// explains what turning one off changes.
const toggles: readonly { key: keyof SlackOptions; label: string; hint?: string }[] = [
  { key: 'detect_mentions', label: 'チャンネルでのメンションを検出する' },
  { key: 'detect_dms', label: 'DM を検出する', hint: '無効にすると、DM の中のメンションはメンションとして検出します。' },
  { key: 'detect_group_dms', label: 'グループ DM を検出する', hint: '参加中のグループ DM は 20 件まで読みます。無効にすると、グループ DM の中のメンションはメンションとして検出します。' },
  { key: 'immediate_notify', label: '検出したらすぐ通知する', hint: '重要度が高いものは、オフでも通知します。' },
];

// SlackSettings connects a Slack user token from an app the user creates
// with Astovia's manifest. The token is sent once and never shown again;
// only its last four characters are.
export default function SlackSettings() {
  const [state, setState] = useState<SlackState | null>(null);
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
      setState(await client.getSlack());
    } catch {
      setError('Slack 連携の状態を読み込めませんでした。接続を確認して、もう一度お試しください。');
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
    if (!token.startsWith(SLACK_TOKEN_PREFIX)) {
      setFormError('User OAuth Token（xoxp-）を貼ってください。');
      return;
    }
    setBusy(true);
    setFormError(null);
    try {
      setState(await client.connectSlack(token));
      setReplacing(false);
    } catch (failure) {
      setFormError(slackErrorMessage(failure instanceof APIError ? failure.code : null));
    } finally {
      setBusy(false);
    }
  }

  async function update(key: keyof SlackOptions, value: boolean): Promise<void> {
    setBusy(true);
    setError(null);
    try {
      setState(await client.updateSlackSettings({ [key]: value }));
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
      await client.disconnectSlack();
      setState({ connected: false });
      setReplacing(false);
    } catch {
      setError('接続を解除できませんでした。接続を確認して、もう一度お試しください。');
    } finally {
      setBusy(false);
    }
  }

  const teamURL = state?.connected ? httpsURL(state.team_url) : null;

  return <section className="action-panel settings-panel" id="slack" aria-labelledby={headingID}>
    <h2 id={headingID}>Slack 連携</h2>
    <p className="intro"><Phrased text="自分へのメンションや DM を AI が読んで、やることがあればタスクにします。そのあと自分が返信して用件が済んだと AI が判断したら、自動で完了にします。" /></p>
    {aiMissing ? <p className="notice">Slack は AI の設定がないと、検出も自動完了も行いません。<Link to="/settings/ai">AI を設定する</Link></p> : null}
    {error ? <p className="notice error" role="alert">{error}</p> : null}
    {loading ? <p className="action-loading" aria-live="polite">読み込んでいます…</p> : null}
    {!loading && state && !state.connected ? <>
      <ol className="steps">
        <li><a href={SLACK_APP_URL} rel="noopener noreferrer" target="_blank">Slack アプリを作る</a>ページを開き、「From a manifest」を選んでワークスペースを選びます。</li>
        <li>下のマニフェストを貼り付けて、アプリを作ります。</li>
        <li>アプリをワークスペースにインストールし、「OAuth &amp; Permissions」の User OAuth Token（<span className="inline-code">xoxp-</span> で始まる）を下に貼り付けて「接続する」を押します。</li>
      </ol>
      <CopyField label="マニフェスト（YAML）" value={SLACK_APP_MANIFEST} />
      <TokenForm label={tokenLabel} submitLabel="接続する" busy={busy} error={formError} onSubmit={(token) => { void connect(token); }} />
    </> : null}
    {!loading && state?.connected ? <>
      <dl className="settings-facts">
        <div><dt>ワークスペース</dt><dd>{teamURL ? <a href={teamURL} rel="noopener noreferrer" target="_blank">{state.team_name}</a> : state.team_name}</dd></div>
        <div><dt>ユーザー</dt><dd>{state.user_name}</dd></div>
        <div><dt>トークン</dt><dd>…{state.token_last4}</dd></div>
        <div><dt>状態</dt><dd className={state.status === 'error' || state.last_error ? 'status-error' : undefined}>{slackStatusMessage(state.status, state.last_error)}</dd></div>
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
        <button className="button secondary danger" type="button" disabled={busy} onClick={() => { void disconnect(); }}>Slack との接続を解除</button>
      </div>}
    </> : null}
  </section>;
}
