import { useCallback, useEffect, useId, useRef, useState } from 'react';
import type { FormEvent, ReactNode } from 'react';
import { request } from './auth';
import { dateTime } from './format';
import { createMFAClient, credentialName, maxCredentialNameLength, maxCredentials, mfaEnabled, mfaErrorMessage } from './mfa';
import type { MFACredential, MFAStatus, TOTPSetup } from './mfa';
import { RecoveryCodes, TOTPCodeField, TOTPSetupDetails } from './MFAStep';
import Phrased from './Phrased';
import { createCredential, webauthnErrorMessage, webauthnSupported } from './webauthn';

const client = createMFAClient(request);

function formatDate(value: string): string {
  return dateTime(value, new Date());
}

// PasswordForm asks for the current password before a change of the second
// factors. onSubmit returns the message of a failure, or null. children are
// the fields above the password.
function PasswordForm({ submitLabel, busyLabel, danger = false, note, children, onSubmit, onCancel }: {
  submitLabel: string;
  busyLabel: string;
  danger?: boolean;
  note?: string;
  children?: ReactNode;
  onSubmit: (password: string) => Promise<string | null>;
  onCancel: () => void;
}) {
  const [password, setPassword] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const passwordID = useId();
  const errorID = useId();

  async function submit(event: FormEvent<HTMLFormElement>): Promise<void> {
    event.preventDefault();
    if (busy) return;
    if (!password) {
      setError('現在のパスワードを入力してください。');
      return;
    }
    if (new TextEncoder().encode(password).length > 72) {
      setError('パスワードは72バイト以内で入力してください。');
      return;
    }
    setBusy(true);
    setError(null);
    const failure = await onSubmit(password);
    // On success the parent closes this form.
    if (failure === null) return;
    setError(failure);
    setBusy(false);
  }

  return <form className="action-form mfa-form" onSubmit={(event) => { void submit(event); }} aria-busy={busy} noValidate>
    {note ? <p className="action-note mfa-form-note"><Phrased text={note} /></p> : null}
    {children ? <fieldset className="mfa-fields" disabled={busy}>{children}</fieldset> : null}
    <div className="field">
      <label htmlFor={passwordID}>現在のパスワード</label>
      <input id={passwordID} name="password" type="password" autoComplete="current-password" value={password} onChange={(e) => { setPassword(e.target.value); setError(null); }} disabled={busy} aria-describedby={error ? errorID : undefined} />
    </div>
    {error ? <p id={errorID} className="notice error" role="alert">{error}</p> : null}
    <div className="form-actions">
      <button className={`button ${danger ? 'secondary danger' : 'primary'}`} type="submit" disabled={busy}>{busy ? busyLabel : submitLabel}</button>
      <button className="button secondary" type="button" disabled={busy} onClick={onCancel}>やめる</button>
    </div>
  </form>;
}

// attempt runs a request that sends the current password and returns the
// message of its failure, or null.
async function attempt(operation: () => Promise<void>): Promise<string | null> {
  try {
    await operation();
    return null;
  } catch (failure) {
    return mfaErrorMessage(failure, 'password');
  }
}

function KeyRow({ credential, removing, onAsk, onCancel, onRemove }: {
  credential: MFACredential;
  removing: boolean;
  onAsk: () => void;
  onCancel: () => void;
  onRemove: (password: string) => Promise<string | null>;
}) {
  return <li className="action-item">
    <div className="action-body">
      <p className="action-content">{credential.name}</p>
      <dl className="action-meta">
        <div><dt>登録</dt><dd>{formatDate(credential.created_at)}</dd></div>
        <div><dt>最終使用</dt><dd>{credential.last_used_at ? formatDate(credential.last_used_at) : '未使用'}</dd></div>
      </dl>
    </div>
    {removing ? <div className="action-confirm">
      <PasswordForm submitLabel="削除する" busyLabel="削除中…" danger note="削除すると、このパスキー・セキュリティキーではログインできなくなります。" onSubmit={onRemove} onCancel={onCancel} />
    </div> : <div className="action-buttons">
      <button className="button secondary small" type="button" onClick={onAsk} aria-label={`${credential.name} を削除`}>削除</button>
    </div>}
  </li>;
}

// Open names the form that is open; only one is at a time.
type Open = { form: 'add-key' } | { form: 'remove-key'; id: string } | { form: 'totp' } | { form: 'remove-totp' } | { form: 'codes' } | null;

// SecuritySettings is the two-step verification page of the account screen
// (/account/security), shown with password sign-in only. The secrets it
// shows (the setup key and the recovery codes) live only in its state.
export default function SecuritySettings() {
  const [status, setStatus] = useState<MFAStatus | null>(null);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [open, setOpen] = useState<Open>(null);
  const [name, setName] = useState('');
  const [setup, setSetup] = useState<TOTPSetup | null>(null);
  const [code, setCode] = useState('');
  const [confirming, setConfirming] = useState(false);
  const [codeError, setCodeError] = useState<string | null>(null);
  const [issued, setIssued] = useState<string[] | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const issuedRef = useRef<HTMLDivElement>(null);
  const statusHeadingID = useId();
  const keysHeadingID = useId();
  const totpHeadingID = useId();
  const codesHeadingID = useId();
  const nameID = useId();
  const codeID = useId();
  const codeErrorID = useId();
  const supported = webauthnSupported();

  const load = useCallback(async () => {
    try {
      setStatus(await client.status());
      setLoadError(null);
    } catch {
      setLoadError('二段階認証の設定を読み込めませんでした。接続を確認して、もう一度お試しください。');
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => { void load(); }, [load]);

  // New recovery codes are brought into view: they are shown only once.
  useEffect(() => {
    if (issued) issuedRef.current?.scrollIntoView({ block: 'nearest' });
  }, [issued]);

  function close(): void {
    setOpen(null);
    setName('');
    setSetup(null);
    setCode('');
    setCodeError(null);
  }

  function show(next: Open): void {
    close();
    setNotice(null);
    setOpen(next);
  }

  // changed closes the open form after a change and reads the status again.
  function changed(message: string, codes: string[] | null): void {
    close();
    setNotice(message);
    if (codes) setIssued(codes);
    void load();
  }

  async function addKey(password: string): Promise<string | null> {
    const label = credentialName(name);
    if (!label) return `名前を 1〜${maxCredentialNameLength} 文字で入力してください。`;
    let begin: Awaited<ReturnType<typeof client.beginWebAuthn>>;
    try {
      begin = await client.beginWebAuthn(password);
    } catch (failure) {
      return mfaErrorMessage(failure, 'password');
    }
    // The password was right; what fails from here is the key.
    try {
      const credential = await createCredential(begin.options);
      const result = await client.finishWebAuthn(begin.state, credential, label);
      changed(`パスキー・セキュリティキー「${result.credential.name}」を登録しました。`, result.recoveryCodes);
      return null;
    } catch (failure) {
      return webauthnErrorMessage(failure) ?? mfaErrorMessage(failure, 'key');
    }
  }

  function removeKey(id: string, password: string): Promise<string | null> {
    return attempt(async () => {
      await client.deleteWebAuthn(id, password);
      changed('パスキー・セキュリティキーを削除しました。', null);
    });
  }

  function beginTOTP(password: string): Promise<string | null> {
    return attempt(async () => {
      setSetup(await client.beginTOTP(password));
    });
  }

  async function finishTOTP(event: FormEvent<HTMLFormElement>): Promise<void> {
    event.preventDefault();
    if (confirming || !setup) return;
    if (!code.trim()) {
      setCodeError('認証アプリのコードを入力してください。');
      return;
    }
    setConfirming(true);
    setCodeError(null);
    try {
      const codes = await client.finishTOTP(setup.setup_token, code);
      changed('認証アプリを登録しました。', codes);
    } catch (failure) {
      setCodeError(mfaErrorMessage(failure, 'code'));
      setCode('');
    } finally {
      setConfirming(false);
    }
  }

  function removeTOTP(password: string): Promise<string | null> {
    return attempt(async () => {
      await client.deleteTOTP(password);
      changed('認証アプリを削除しました。', null);
    });
  }

  function reissue(password: string): Promise<string | null> {
    return attempt(async () => {
      changed('リカバリーコードを再発行しました。', await client.reissueRecoveryCodes(password));
    });
  }

  if (!status) {
    return <section className="action-panel settings-panel" id="security" aria-labelledby={statusHeadingID}>
      <h2 id={statusHeadingID}>二段階認証</h2>
      {loadError ? <p className="notice error" role="alert">{loadError}</p> : null}
      {loadError ? <button className="button secondary" type="button" onClick={() => { setLoading(true); void load(); }}>もう一度読み込む</button> : null}
      {loading ? <p className="action-loading" aria-live="polite">読み込んでいます…</p> : null}
    </section>;
  }

  const enabled = mfaEnabled(status);
  const full = status.webauthn.length >= maxCredentials;
  const keyBlocked = !status.webauthn_available
    ? 'この Astovia のアドレスでは、パスキー・セキュリティキーを使えません。'
    : !supported ? 'このブラウザーはパスキー・セキュリティキーに対応していません。'
      : full ? `登録できるのは ${maxCredentials} 個までです。` : null;

  return <div className="settings-stack">
    <section className="action-panel settings-panel" id="security" aria-labelledby={statusHeadingID}>
      <h2 id={statusHeadingID}>二段階認証</h2>
      <p className="intro"><Phrased text="ログインのときに、パスワードに加えてパスキー・セキュリティキーか認証アプリで本人確認をします。設定の変更は、次回のログインから使われます。" /></p>
      {loadError ? <p className="notice error" role="alert">{loadError}</p> : null}
      {notice ? <p className="notice success" role="status">{notice}</p> : null}
      {status.required && !enabled ? <p className="notice warning"><Phrased text="この Astovia では二段階認証が必要です。次回のログインで登録が必要です。" /></p> : null}
      <dl className="settings-facts">
        <div><dt>状態</dt><dd>{enabled ? '有効' : '無効'}</dd></div>
        <div><dt>この Astovia</dt><dd>{status.required ? '二段階認証が必要' : '二段階認証は任意'}</dd></div>
      </dl>
    </section>

    <section className="action-panel settings-panel" id="security-keys" aria-labelledby={keysHeadingID}>
      <h2 id={keysHeadingID}>パスキー・セキュリティキー</h2>
      <p className="intro"><Phrased text="端末の指紋認証や顔認証、USB のセキュリティキーで確認します。10 個まで登録できます。" /></p>
      {status.webauthn.length > 0 ? <ul className="action-list">
        {status.webauthn.map((credential) => <KeyRow key={credential.id} credential={credential}
          removing={open?.form === 'remove-key' && open.id === credential.id}
          onAsk={() => show({ form: 'remove-key', id: credential.id })}
          onCancel={close}
          onRemove={(password) => removeKey(credential.id, password)} />)}
      </ul> : <p className="action-empty">パスキー・セキュリティキーは登録されていません。</p>}
      {open?.form === 'add-key' ? <PasswordForm submitLabel="登録する" busyLabel="登録中…" note="次に進むと、ブラウザーが登録の画面を出します。" onSubmit={addKey} onCancel={close}>
        <div className="field">
          <label htmlFor={nameID}>名前</label>
          <input id={nameID} name="name" type="text" autoComplete="off" maxLength={maxCredentialNameLength} placeholder="例: ノートパソコン" value={name} onChange={(e) => setName(e.target.value)} />
        </div>
      </PasswordForm> : <>
        {keyBlocked ? <p className="field-hint mfa-hint">{keyBlocked}</p> : null}
        <div className="form-actions"><button className="button primary" type="button" disabled={keyBlocked !== null} onClick={() => show({ form: 'add-key' })}>追加</button></div>
      </>}
    </section>

    <section className="action-panel settings-panel" id="security-totp" aria-labelledby={totpHeadingID}>
      <h2 id={totpHeadingID}>認証アプリ</h2>
      <p className="intro"><Phrased text="認証アプリに表示される 6 桁のコードで確認します。" /></p>
      {status.totp ? <dl className="settings-facts mfa-facts">
        <div><dt>状態</dt><dd>登録済み</dd></div>
        <div><dt>登録</dt><dd>{formatDate(status.totp.created_at)}</dd></div>
      </dl> : <p className="action-empty">認証アプリは登録されていません。</p>}
      {open?.form === 'totp' && !setup ? <PasswordForm submitLabel="次へ" busyLabel="準備中…" note={status.totp ? '登録し直すと、前の認証アプリのコードは使えなくなります。リカバリーコードはそのまま使えます。' : undefined} onSubmit={beginTOTP} onCancel={close} /> : null}
      {open?.form === 'totp' && setup ? <form className="action-form mfa-form" onSubmit={(event) => { void finishTOTP(event); }} aria-busy={confirming} noValidate>
        <TOTPSetupDetails setup={setup} />
        <TOTPCodeField id={codeID} value={code} onChange={(value) => { setCode(value); setCodeError(null); }} disabled={confirming} describedBy={codeError ? codeErrorID : undefined} />
        {codeError ? <p id={codeErrorID} className="notice error" role="alert">{codeError}</p> : null}
        <div className="form-actions">
          <button className="button primary" type="submit" disabled={confirming}>{confirming ? '確認中…' : '確認して登録'}</button>
          <button className="button secondary" type="button" disabled={confirming} onClick={close}>やめる</button>
        </div>
      </form> : null}
      {open?.form === 'remove-totp' ? <PasswordForm submitLabel="削除する" busyLabel="削除中…" danger note="削除すると、認証アプリのコードではログインできなくなります。" onSubmit={removeTOTP} onCancel={close} /> : null}
      {open?.form !== 'totp' && open?.form !== 'remove-totp' ? <div className="form-actions">
        {status.totp ? <>
          <button className="button secondary" type="button" onClick={() => show({ form: 'totp' })}>登録し直す</button>
          <button className="button secondary danger" type="button" onClick={() => show({ form: 'remove-totp' })}>削除</button>
        </> : <button className="button primary" type="button" onClick={() => show({ form: 'totp' })}>登録</button>}
      </div> : null}
    </section>

    <section className="action-panel settings-panel" id="security-codes" aria-labelledby={codesHeadingID}>
      <h2 id={codesHeadingID}>リカバリーコード</h2>
      <p className="intro"><Phrased text="パスキー・セキュリティキーも認証アプリも使えないときに、パスワードと合わせてログインに使うコードです。最初の方法を登録したときに 10 個発行します。" /></p>
      {issued ? <div ref={issuedRef} className="mfa-issued"><RecoveryCodes codes={issued}>
        <button className="button secondary small" type="button" onClick={() => setIssued(null)}>閉じる</button>
      </RecoveryCodes></div> : null}
      {enabled ? <dl className="settings-facts mfa-facts">
        <div><dt>残り</dt><dd className={status.recovery_codes_remaining === 0 ? 'status-error' : undefined}>{status.recovery_codes_remaining} 個</dd></div>
      </dl> : <p className="action-empty">二段階認証を有効にすると発行します。</p>}
      {open?.form === 'codes' ? <PasswordForm submitLabel="再発行する" busyLabel="再発行中…" note="再発行すると、前のリカバリーコードはすべて使えなくなります。" onSubmit={reissue} onCancel={close} /> : null}
      {enabled && open?.form !== 'codes' ? <div className="form-actions"><button className="button secondary" type="button" onClick={() => show({ form: 'codes' })}>再発行</button></div> : null}
    </section>
  </div>;
}
