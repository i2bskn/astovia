import { useCallback, useEffect, useId, useMemo, useState } from 'react';
import type { FormEvent } from 'react';
import { APIError, request } from './auth';
import { dateTime } from './format';
import { createIntegrationsClient } from './integrations';
import type { PushDevice } from './integrations';
import NotificationChannelsSettings from './NotificationChannelsSettings';
import { base64URLToBytes, bytesToBase64URL, deviceLabel, isIOS, pushSupport, sameKey } from './notifications';
import type { PushSupport } from './notifications';
import Phrased from './Phrased';

const client = createIntegrationsClient(request);

function formatDate(value: string): string {
  return dateTime(value, new Date());
}

function browserTimezone(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC';
  } catch {
    return 'UTC';
  }
}

function currentSupport(): PushSupport {
  const standalone = window.matchMedia?.('(display-mode: standalone)').matches === true ||
    ('standalone' in navigator && (navigator as { standalone?: boolean }).standalone === true);
  return pushSupport({
    hasServiceWorker: 'serviceWorker' in navigator,
    hasPushManager: 'PushManager' in window,
    hasNotification: 'Notification' in window,
    isIOS: isIOS(navigator.userAgent, navigator.platform, navigator.maxTouchPoints),
    standalone,
    permission: 'Notification' in window ? Notification.permission : 'unsupported',
  });
}

const supportMessages: Record<Exclude<PushSupport, 'supported'>, string> = {
  'ios-home-screen': 'iPhone・iPad では、共有メニューの「ホーム画面に追加」で追加してから開いてください。',
  denied: '通知が拒否されています。ブラウザ（またはスマートフォン）の設定で、このサイトの通知を許可してください。',
  unsupported: 'このブラウザは通知に対応していません。',
};

// candidateExpireChoices are the days offered for dismissing task
// candidates; 0 keeps them.
const candidateExpireChoices: readonly number[] = [1, 3, 7, 14, 0];

function candidateExpireLabel(days: number): string {
  return days === 0 ? 'しない' : `${days}日`;
}

const timezones: readonly string[] = (() => {
  try {
    return Intl.supportedValuesOf('timeZone');
  } catch {
    return [];
  }
})();

// SupportMessage puts the first sentence (the state) in bold and leaves
// the instructions after it in the normal weight.
function SupportMessage({ text }: { text: string }) {
  const end = text.indexOf('。') + 1;
  return end > 0 && end < text.length ? <><strong>{text.slice(0, end)}</strong><Phrased text={text.slice(end)} /></> : <Phrased text={text} />;
}

// removeLegacyRegistration removes the service worker registered with the
// /admin/ scope before the UI moved to /, with its push subscription, so that
// this browser does not receive each notification twice. Failures are
// ignored: the server drops the old subscription once it is gone.
async function removeLegacyRegistration(): Promise<void> {
  try {
    const legacyScope = `${location.origin}/admin/`;
    for (const registration of await navigator.serviceWorker.getRegistrations()) {
      if (registration.scope !== legacyScope) continue;
      try {
        await (await registration.pushManager.getSubscription())?.unsubscribe();
      } catch {
        // Ignored; unregister below still stops the old worker.
      }
      await registration.unregister();
    }
  } catch {
    // Ignored; see above.
  }
}

// subscribe registers this browser for Web Push. It must be called from a
// user action: Notification.requestPermission is called first, before any
// await, so that browsers treat it as user initiated.
async function subscribe(): Promise<'granted' | 'denied'> {
  const permission = await Notification.requestPermission();
  if (permission !== 'granted') return 'denied';
  await removeLegacyRegistration();
  const registration = await navigator.serviceWorker.register('/sw.js', { scope: '/' });
  const key = base64URLToBytes(await client.getPushPublicKey());
  let subscription = await registration.pushManager.getSubscription();
  // A subscription made for another server key (after a key change) cannot
  // receive messages; replace it.
  if (subscription && !sameKey(subscription.options.applicationServerKey, key)) {
    await subscription.unsubscribe();
    subscription = null;
  }
  subscription ??= await registration.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: key });
  const p256dh = subscription.getKey('p256dh');
  const auth = subscription.getKey('auth');
  if (!p256dh || !auth) throw new Error('Push subscription has no keys');
  // Registering an existing subscription again restores it when the server
  // removed it.
  await client.registerPushDevice({
    endpoint: subscription.endpoint,
    keys: { p256dh: bytesToBase64URL(p256dh), auth: bytesToBase64URL(auth) },
    label: deviceLabel(navigator.userAgent),
    timezone: browserTimezone(),
  });
  return 'granted';
}

function DeviceRow({ device, busy, onDelete }: { device: PushDevice; busy: boolean; onDelete: () => void }) {
  return <li className="action-item">
    <div className="action-body">
      <p className="action-content">{device.label || '名前のない端末'}</p>
      <dl className="action-meta">
        <div><dt>登録</dt><dd>{formatDate(device.created_at)}</dd></div>
        <div><dt>最終送信</dt><dd>{device.last_used_at ? formatDate(device.last_used_at) : 'まだありません'}</dd></div>
      </dl>
    </div>
    <div className="action-buttons">
      <button className="button secondary small" type="button" disabled={busy} onClick={onDelete}>削除</button>
    </div>
  </li>;
}

// NotificationSettings registers this device for Web Push, chooses the
// channels of each kind of notification and edits when notifications are
// sent.
export default function NotificationSettings() {
  const [support, setSupport] = useState<PushSupport>(() => currentSupport());
  const [devices, setDevices] = useState<PushDevice[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [timezone, setTimezone] = useState(browserTimezone());
  const [digestHour, setDigestHour] = useState(8);
  const [staleDays, setStaleDays] = useState(3);
  const [candidateExpireDays, setCandidateExpireDays] = useState(3);
  const [saving, setSaving] = useState(false);
  const [settingsMessage, setSettingsMessage] = useState<{ error: boolean; text: string } | null>(null);
  const headingID = useId();
  const tzID = useId();
  const hourID = useId();
  const daysID = useId();
  const expireID = useId();
  const expireHintID = useId();
  const hours = useMemo(() => Array.from({ length: 24 }, (_, i) => i), []);

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const [list, settings] = await Promise.all([client.listPushDevices(), client.getNotificationSettings()]);
      setDevices(list);
      // Before settings are saved the server reports UTC; show the
      // browser's zone instead.
      setTimezone(list.length === 0 && settings.timezone === 'UTC' ? browserTimezone() : settings.timezone);
      setDigestHour(settings.digest_hour);
      setStaleDays(settings.stale_days);
      setCandidateExpireDays(settings.candidate_expire_days);
    } catch {
      setError('通知の設定を読み込めませんでした。接続を確認して、もう一度お試しください。');
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => { void load(); }, [load]);

  async function enable(): Promise<void> {
    if (busy) return;
    setBusy(true);
    setError(null);
    setNotice(null);
    try {
      const result = await subscribe();
      if (result === 'denied') {
        setSupport(currentSupport());
        setNotice(supportMessages.denied);
        return;
      }
      setDevices(await client.listPushDevices());
      setNotice('この端末で通知を受け取れるようになりました。');
    } catch (failure) {
      setError(failure instanceof APIError && failure.status === 409
        ? '通知を受け取る端末は20台までです。不要な端末を削除してください。'
        : 'この端末を登録できませんでした。もう一度お試しください。');
    } finally {
      setBusy(false);
    }
  }

  async function remove(id: string): Promise<void> {
    setBusy(true);
    setError(null);
    try {
      await client.deletePushDevice(id);
      setDevices((previous) => previous.filter((d) => d.id !== id));
    } catch {
      setError('端末を削除できませんでした。接続を確認して、もう一度お試しください。');
    } finally {
      setBusy(false);
    }
  }

  async function test(): Promise<void> {
    setBusy(true);
    setError(null);
    setNotice(null);
    try {
      await client.sendTestNotification();
      setNotice('テスト通知を送りました。数秒で届きます。');
    } catch (failure) {
      const status = failure instanceof APIError ? failure.status : 0;
      setError(status === 409 ? '通知を受け取る端末が登録されていません。'
        : status === 429 ? 'テスト通知は1分に3回までです。少し待ってからお試しください。'
          : 'テスト通知を送れませんでした。接続を確認して、もう一度お試しください。');
    } finally {
      setBusy(false);
    }
  }

  async function save(event: FormEvent<HTMLFormElement>): Promise<void> {
    event.preventDefault();
    if (saving) return;
    setSaving(true);
    setSettingsMessage(null);
    try {
      const saved = await client.saveNotificationSettings({ timezone: timezone.trim(), digest_hour: digestHour, stale_days: staleDays, candidate_expire_days: candidateExpireDays });
      setTimezone(saved.timezone);
      setCandidateExpireDays(saved.candidate_expire_days);
      setSettingsMessage({ error: false, text: '保存しました。' });
    } catch (failure) {
      setSettingsMessage({ error: true, text: failure instanceof APIError && failure.status === 400
        ? '設定を保存できませんでした。入力内容を確認してください。'
        : '保存できませんでした。接続を確認して、もう一度お試しください。' });
    } finally {
      setSaving(false);
    }
  }

  return <section className="action-panel settings-panel" id="notifications" aria-labelledby={headingID}>
    <h2 id={headingID}>通知</h2>
    <p className="intro"><Phrased text="新しく見つけたタスク、未対応が続いているタスク、期限、朝のまとめを通知します。送り先はこの端末（Web Push）、Slack、メールから選べます。" /></p>
    {support === 'supported'
      ? <div className="form-actions"><button className="button primary add-button" type="button" disabled={busy} onClick={() => { void enable(); }}>この端末で通知を受け取る</button></div>
      : <p className={support === 'denied' ? 'notice warning' : 'notice'} role={support === 'denied' ? 'alert' : undefined}><SupportMessage text={supportMessages[support]} /></p>}
    {support !== 'ios-home-screen' ? <p className="field-hint"><Phrased text="iPhone・iPad では、Safari の共有メニューから「ホーム画面に追加」した Astovia を開いて登録します。" /></p> : null}
    {notice ? <p className="notice success" role="status">{notice}</p> : null}
    {error ? <p className="notice error" role="alert">{error}</p> : null}
    <h3 className="settings-subheading">通知を受け取る端末</h3>
    {devices.length > 0 ? <ul className="action-list">
      {devices.map((device) => <DeviceRow key={device.id} device={device} busy={busy} onDelete={() => { void remove(device.id); }} />)}
    </ul> : null}
    {!loading && devices.length === 0 ? <p className="action-empty">登録された端末はありません。{support === 'supported' ? '上の「この端末で通知を受け取る」から登録できます。' : null}</p> : null}
    {loading ? <p className="action-loading" aria-live="polite">読み込んでいます…</p> : null}
    {devices.length > 0 ? <div className="form-actions"><button className="button secondary" type="button" disabled={busy} onClick={() => { void test(); }}>テスト通知を送る</button></div> : null}
    <NotificationChannelsSettings />
    <h3 className="settings-subheading">通知のタイミング</h3>
    <form className="action-form settings-form" onSubmit={(event) => { void save(event); }} aria-busy={saving}>
      <div className="field-grid">
      <div className="field">
        <label htmlFor={hourID}>朝のまとめの時刻</label>
        <select id={hourID} value={digestHour} onChange={(e) => setDigestHour(Number(e.target.value))} disabled={saving}>
          {hours.map((h) => <option key={h} value={h}>{h}時</option>)}
        </select>
      </div>
      <div className="field">
        <label htmlFor={daysID}>未対応を知らせるまで（日）</label>
        <input id={daysID} type="number" inputMode="numeric" min={1} max={30} required value={staleDays} onChange={(e) => setStaleDays(Math.min(30, Math.max(1, Math.trunc(Number(e.target.value)) || 1)))} disabled={saving} />
      </div>
      <div className="field">
        <label htmlFor={tzID}>タイムゾーン</label>
        {timezones.length > 0
          ? <select id={tzID} value={timezone} onChange={(e) => setTimezone(e.target.value)} disabled={saving}>
            {timezones.includes(timezone) ? null : <option value={timezone}>{timezone}</option>}
            {timezones.map((tz) => <option key={tz} value={tz}>{tz}</option>)}
          </select>
          : <input id={tzID} autoCapitalize="none" spellCheck={false} maxLength={64} required value={timezone} onChange={(e) => setTimezone(e.target.value)} disabled={saving} />}
      </div>
      </div>
      <div className="field">
        <label htmlFor={expireID}>タスク候補を自動で対象外にするまで</label>
        <select id={expireID} value={candidateExpireDays} onChange={(e) => setCandidateExpireDays(Number(e.target.value))} disabled={saving} aria-describedby={expireHintID}>
          {candidateExpireChoices.includes(candidateExpireDays) ? null : <option value={candidateExpireDays}>{candidateExpireLabel(candidateExpireDays)}</option>}
          {candidateExpireChoices.map((d) => <option key={d} value={d}>{candidateExpireLabel(d)}</option>)}
        </select>
      </div>
      <p className="field-hint" id={expireHintID}><Phrased text="そのままの候補は、この日数が過ぎると対象外に移します。" /></p>
      {settingsMessage ? <p className={settingsMessage.error ? 'notice error' : 'notice success'} role={settingsMessage.error ? 'alert' : 'status'}>{settingsMessage.text}</p> : null}
      <div className="form-actions"><button className="button primary" type="submit" disabled={saving}>{saving ? '保存中…' : '通知のタイミングを保存'}</button></div>
    </form>
  </section>;
}
