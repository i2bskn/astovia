import { useCallback, useEffect, useId, useMemo, useState } from 'react';
import type { FormEvent } from 'react';
import { APIError, request } from './auth';
import { createIntegrationsClient } from './integrations';
import type { PushDevice } from './integrations';
import { base64URLToBytes, bytesToBase64URL, deviceLabel, isIOS, pushSupport, sameKey } from './notifications';
import type { PushSupport } from './notifications';

const client = createIntegrationsClient(request);

function formatDate(value: string): string {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString('ja-JP');
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

const timezones: readonly string[] = (() => {
  try {
    return Intl.supportedValuesOf('timeZone');
  } catch {
    return [];
  }
})();

// subscribe registers this browser for Web Push. It must be called from a
// user action: Notification.requestPermission is called first, before any
// await, so that browsers treat it as user initiated.
async function subscribe(): Promise<'granted' | 'denied'> {
  const permission = await Notification.requestPermission();
  if (permission !== 'granted') return 'denied';
  const registration = await navigator.serviceWorker.register('/admin/sw.js', { scope: '/admin/' });
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

// NotificationSettings registers this device for Web Push and edits when
// notifications are sent.
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
  const [saving, setSaving] = useState(false);
  const [settingsMessage, setSettingsMessage] = useState<{ error: boolean; text: string } | null>(null);
  const headingID = useId();
  const tzID = useId();
  const tzListID = useId();
  const hourID = useId();
  const daysID = useId();
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
      const saved = await client.saveNotificationSettings({ timezone: timezone.trim(), digest_hour: digestHour, stale_days: staleDays });
      setTimezone(saved.timezone);
      setSettingsMessage({ error: false, text: '保存しました。' });
    } catch (failure) {
      setSettingsMessage({ error: true, text: failure instanceof APIError && failure.status === 400
        ? 'タイムゾーンは Asia/Tokyo のような名前で入力してください。'
        : '保存できませんでした。接続を確認して、もう一度お試しください。' });
    } finally {
      setSaving(false);
    }
  }

  return <section className="action-panel settings-panel" aria-labelledby={headingID}>
    <h2 id={headingID}>通知</h2>
    <p className="intro">新しく見つけた対応事項、未対応が続いている事項、朝のまとめをこの端末に通知します。</p>
    {support === 'supported'
      ? <div className="form-actions"><button className="button primary add-button" type="button" disabled={busy} onClick={() => { void enable(); }}>この端末で通知を受け取る</button></div>
      : <p className="notice">{supportMessages[support]}</p>}
    {notice ? <p className="notice success" role="status">{notice}</p> : null}
    {error ? <p className="notice error" role="alert">{error}</p> : null}
    <h3 className="settings-subheading">通知を受け取る端末</h3>
    {devices.length > 0 ? <ul className="action-list">
      {devices.map((device) => <DeviceRow key={device.id} device={device} busy={busy} onDelete={() => { void remove(device.id); }} />)}
    </ul> : null}
    {!loading && devices.length === 0 ? <p className="action-empty">登録された端末はありません。</p> : null}
    {loading ? <p className="action-loading" aria-live="polite">読み込んでいます…</p> : null}
    {devices.length > 0 ? <div className="form-actions"><button className="button secondary" type="button" disabled={busy} onClick={() => { void test(); }}>テスト通知を送る</button></div> : null}
    <h3 className="settings-subheading">通知のタイミング</h3>
    <form className="action-form settings-form" onSubmit={(event) => { void save(event); }} aria-busy={saving}>
      <div className="field">
        <label htmlFor={hourID}>朝のまとめの時刻</label>
        <select id={hourID} value={digestHour} onChange={(e) => setDigestHour(Number(e.target.value))} disabled={saving}>
          {hours.map((h) => <option key={h} value={h}>{h}時</option>)}
        </select>
      </div>
      <div className="field">
        <label htmlFor={daysID}>未対応を知らせるまでの日数</label>
        <input id={daysID} type="number" inputMode="numeric" min={1} max={30} required value={staleDays} onChange={(e) => setStaleDays(Math.min(30, Math.max(1, Math.trunc(Number(e.target.value)) || 1)))} disabled={saving} />
      </div>
      <div className="field">
        <label htmlFor={tzID}>タイムゾーン</label>
        <input id={tzID} list={tzListID} autoCapitalize="none" spellCheck={false} maxLength={64} required value={timezone} onChange={(e) => setTimezone(e.target.value)} disabled={saving} />
        <datalist id={tzListID}>{timezones.map((tz) => <option key={tz} value={tz} />)}</datalist>
      </div>
      {settingsMessage ? <p className={settingsMessage.error ? 'notice error' : 'notice success'} role={settingsMessage.error ? 'alert' : 'status'}>{settingsMessage.text}</p> : null}
      <button className="button primary add-button" type="submit" disabled={saving}>{saving ? '保存中…' : '保存'}</button>
    </form>
  </section>;
}
