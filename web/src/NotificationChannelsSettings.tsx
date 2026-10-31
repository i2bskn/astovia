import { useCallback, useEffect, useId, useState } from 'react';
import type { FormEvent } from 'react';
import { APIError, request } from './auth';
import { channelErrorMessage, channelLabel, createNotificationChannelsClient, kindLabel, notificationChannels, notificationKinds, routeLabel, slackLastErrorMessage, slackReady, toggleRoute } from './notificationChannels';
import type { NotificationChannel, NotificationChannelsState, NotificationKind, NotificationRoutes } from './notificationChannels';
import Phrased from './Phrased';

const client = createNotificationChannelsClient(request);

type Message = { error: boolean; text: string } | null;

function failureMessage(failure: unknown, webhook = false): string {
  return failure instanceof APIError ? channelErrorMessage(failure.status, failure.code, webhook) : channelErrorMessage(0, null, webhook);
}

function StatusMessage({ message }: { message: Message }) {
  if (!message) return null;
  return <p className={message.error ? 'notice error' : 'notice success'} role={message.error ? 'alert' : 'status'}>{message.text}</p>;
}

// NotificationChannelsSettings chooses which kinds of notifications go to Web
// Push, Slack (an Incoming Webhook) and e-mail, and manages the Slack
// webhook and the test sends of Slack and e-mail.
export default function NotificationChannelsSettings() {
  const [state, setState] = useState<NotificationChannelsState | null>(null);
  const [routes, setRoutes] = useState<NotificationRoutes>({ webpush: [], slack: [], email: [] });
  const [loadError, setLoadError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [routesMessage, setRoutesMessage] = useState<Message>(null);
  const [webhookURL, setWebhookURL] = useState('');
  const [replacing, setReplacing] = useState(false);
  const [slackBusy, setSlackBusy] = useState(false);
  const [slackMessage, setSlackMessage] = useState<Message>(null);
  const [emailBusy, setEmailBusy] = useState(false);
  const [emailMessage, setEmailMessage] = useState<Message>(null);
  const webhookID = useId();
  const webhookHintID = useId();

  const apply = useCallback((next: NotificationChannelsState) => {
    setState(next);
    setRoutes(next.routes);
  }, []);

  const load = useCallback(async () => {
    setLoadError(null);
    try {
      apply(await client.get());
    } catch {
      setLoadError('送り先の設定を読み込めませんでした。接続を確認して、もう一度お試しください。');
    }
  }, [apply]);

  useEffect(() => { void load(); }, [load]);

  async function saveRoutes(event: FormEvent<HTMLFormElement>): Promise<void> {
    event.preventDefault();
    if (saving) return;
    setSaving(true);
    setRoutesMessage(null);
    try {
      apply(await client.saveRoutes(routes));
      setRoutesMessage({ error: false, text: '保存しました。' });
    } catch (failure) {
      setRoutesMessage({ error: true, text: failureMessage(failure) });
    } finally {
      setSaving(false);
    }
  }

  async function registerWebhook(event: FormEvent<HTMLFormElement>): Promise<void> {
    event.preventDefault();
    if (slackBusy) return;
    setSlackBusy(true);
    setSlackMessage(null);
    try {
      const next = await client.setSlackWebhook(webhookURL);
      setState(next);
      setWebhookURL('');
      setReplacing(false);
      setSlackMessage({ error: false, text: 'Webhook を登録しました。' });
    } catch (failure) {
      setSlackMessage({ error: true, text: failureMessage(failure, true) });
    } finally {
      setSlackBusy(false);
    }
  }

  async function deleteWebhook(): Promise<void> {
    if (slackBusy) return;
    setSlackBusy(true);
    setSlackMessage(null);
    try {
      await client.deleteSlackWebhook();
      setState((previous) => previous ? { ...previous, slack: { configured: false, last_error: null } } : previous);
      setReplacing(false);
      setSlackMessage({ error: false, text: 'Webhook を削除しました。' });
    } catch (failure) {
      setSlackMessage({ error: true, text: failureMessage(failure) });
    } finally {
      setSlackBusy(false);
    }
  }

  async function sendTest(channel: Exclude<NotificationChannel, 'webpush'>): Promise<void> {
    const setBusy = channel === 'slack' ? setSlackBusy : setEmailBusy;
    const setMessage = channel === 'slack' ? setSlackMessage : setEmailMessage;
    setBusy(true);
    setMessage(null);
    try {
      await client.sendTest(channel);
      setMessage({ error: false, text: 'テスト通知を送りました。少しで届きます。' });
    } catch (failure) {
      setMessage({ error: true, text: failureMessage(failure) });
    } finally {
      setBusy(false);
    }
  }

  function toggle(channel: NotificationChannel, kind: NotificationKind, on: boolean): void {
    setRoutes((previous) => toggleRoute(previous, channel, kind, on));
    setRoutesMessage(null);
  }

  if (loadError) return <><h3 className="settings-subheading">送り先</h3><p className="notice error" role="alert">{loadError}</p></>;
  if (!state) return <><h3 className="settings-subheading">送り先</h3><p className="action-loading" aria-live="polite">読み込んでいます…</p></>;

  const mailAvailable = state.email.available;
  const slackLastError = slackLastErrorMessage(state.slack.last_error);
  const showWebhookForm = !state.slack.configured || replacing;

  return <>
    <h3 className="settings-subheading">送り先</h3>
    <p className="field-hint"><Phrased text="通知の種類ごとに、送り先を選びます。" /></p>
    <form className="action-form settings-form" onSubmit={(event) => { void saveRoutes(event); }} aria-busy={saving}>
      <div className="route-table-wrap">
        <table className="route-table">
          <thead>
            <tr>
              <th scope="col"><span className="sr-only">通知の種類</span></th>
              {notificationChannels.map((channel) => <th key={channel} scope="col">{channelLabel(channel)}</th>)}
            </tr>
          </thead>
          <tbody>
            {notificationKinds.map((kind) => <tr key={kind}>
              <th scope="row">{kindLabel(kind)}</th>
              {notificationChannels.map((channel) => {
                const checked = routes[channel].includes(kind);
                // Without a mail service, chosen kinds can only be removed.
                const disabled = saving || (channel === 'email' && !mailAvailable && !checked);
                return <td key={channel}>
                  <input type="checkbox" aria-label={routeLabel(kind, channel)} checked={checked} disabled={disabled} onChange={(e) => toggle(channel, kind, e.target.checked)} />
                </td>;
              })}
            </tr>)}
          </tbody>
        </table>
      </div>
      {!slackReady(state) ? <p className="field-hint route-note"><Phrased text="Slack: Webhook を登録すると送られます。" /></p> : null}
      {!mailAvailable ? <p className="field-hint route-note"><Phrased text="メール: サーバーでメールが設定されていません。" /></p> : null}
      <StatusMessage message={routesMessage} />
      <div className="form-actions"><button className="button primary" type="submit" disabled={saving}>{saving ? '保存中…' : '保存'}</button></div>
    </form>

    <h3 className="settings-subheading">Slack</h3>
    {state.slack.configured
      ? <>
        <p className={slackLastError ? 'notice error' : 'notice'} role={slackLastError ? 'alert' : undefined}>{slackLastError ?? '登録済み'}</p>
        {!replacing ? <div className="form-actions">
          <button className="button secondary" type="button" disabled={slackBusy || slackLastError !== null} onClick={() => { void sendTest('slack'); }}>テスト送信</button>
          <button className="button secondary" type="button" disabled={slackBusy} onClick={() => { setReplacing(true); setSlackMessage(null); }}>登録し直す</button>
          <button className="button secondary danger" type="button" disabled={slackBusy} onClick={() => { void deleteWebhook(); }}>削除</button>
        </div> : null}
      </>
      : <p className="field-hint"><Phrased text="Slack アプリの「Incoming Webhooks」を有効にし、通知を送るチャンネルを選んで、作られた Webhook URL をコピーして貼り付けます。" /></p>}
    {showWebhookForm ? <form className="action-form settings-form" onSubmit={(event) => { void registerWebhook(event); }} aria-busy={slackBusy}>
      <div className="field">
        <label htmlFor={webhookID}>Webhook URL</label>
        <input id={webhookID} type="url" inputMode="url" autoComplete="off" autoCapitalize="none" spellCheck={false} required maxLength={512} placeholder="https://hooks.slack.com/services/…" value={webhookURL} onChange={(e) => setWebhookURL(e.target.value)} disabled={slackBusy} aria-describedby={webhookHintID} />
      </div>
      <p className="field-hint" id={webhookHintID}><Phrased text="URL は暗号化して保存し、画面には表示しません。" /></p>
      <div className="form-actions">
        {replacing ? <button className="button secondary" type="button" disabled={slackBusy} onClick={() => { setReplacing(false); setWebhookURL(''); }}>やめる</button> : null}
        <button className="button primary" type="submit" disabled={slackBusy || webhookURL.trim() === ''}>{slackBusy ? '登録中…' : '登録'}</button>
      </div>
    </form> : null}
    <StatusMessage message={slackMessage} />

    <h3 className="settings-subheading">メール</h3>
    {mailAvailable
      ? <>
        <p className="field-hint"><Phrased text={`送り先: ${state.email.address ?? ''}（ログインのメールアドレス）`} /></p>
        <div className="form-actions"><button className="button secondary" type="button" disabled={emailBusy} onClick={() => { void sendTest('email'); }}>テスト送信</button></div>
      </>
      : <p className="field-hint"><Phrased text="このサーバーではメール通知を設定していません。使うには、運用者がサーバーにメールの送信サービスを設定します。" /></p>}
    <StatusMessage message={emailMessage} />
  </>;
}
