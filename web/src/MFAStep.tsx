import { useEffect, useId, useMemo, useRef, useState } from 'react';
import type { FormEvent, ReactNode } from 'react';
import { useLocation, useNavigate } from 'react-router';
import { auth, isSuperseded } from './auth';
import type { PendingMFA } from './auth';
import CopyField from './CopyField';
import { linkPageAt } from './links';
import { credentialName, maxCredentialNameLength, mfaErrorMessage, qrModules, qrPath, qrSize, readTOTPSetup, readWebAuthnBegin } from './mfa';
import type { TOTPSetup } from './mfa';
import Phrased from './Phrased';
import { createCredential, getCredential, webauthnErrorMessage, webauthnSupported } from './webauthn';

// QRCode draws text as an inline SVG: the content security policy allows
// neither data: images nor inline styles. It is black on white in every
// color scheme, with the quiet zone readers need.
export function QRCode({ text, label }: { text: string; label: string }) {
  const modules = useMemo(() => qrModules(text), [text]);
  if (!modules) return null;
  const size = qrSize(modules);
  return <svg className="qr-code" role="img" aria-label={label} viewBox={`0 0 ${size} ${size}`} width={200} height={200} shapeRendering="crispEdges">
    <title>{label}</title>
    <rect width={size} height={size} fill="#ffffff" />
    <path d={qrPath(modules)} fill="#000000" />
  </svg>;
}

// TOTPSetupDetails shows the secret of a new authenticator app as a QR code
// and as a key to type.
export function TOTPSetupDetails({ setup }: { setup: TOTPSetup }) {
  return <div className="totp-setup">
    <QRCode text={setup.otpauth_url} label="認証アプリで読み取る QR コード" />
    <div className="totp-setup-body">
      <p className="totp-setup-note"><Phrased text="認証アプリで QR コードを読み取ってください。読み取れないときは、次のキーを認証アプリに入力します。" /></p>
      <CopyField label="セットアップキー" value={setup.secret} />
    </div>
  </div>;
}

// TOTPCodeField is the input of a code of the authenticator app.
export function TOTPCodeField({ id, value, onChange, disabled, describedBy }: {
  id: string;
  value: string;
  onChange: (value: string) => void;
  disabled: boolean;
  describedBy?: string;
}) {
  return <div className="field">
    <label htmlFor={id}>認証アプリのコード（6 桁）</label>
    <input id={id} className="code-input" name="code" type="text" inputMode="numeric" autoComplete="one-time-code" pattern="[0-9 ]*" maxLength={12} autoCapitalize="none" spellCheck={false} value={value} onChange={(e) => onChange(e.target.value)} disabled={disabled} aria-describedby={describedBy} />
  </div>;
}

// RecoveryCodes shows new recovery codes once. They live only in the state
// of the screen that shows them and are gone after a reload. children are
// the buttons next to the copy button.
export function RecoveryCodes({ codes, children }: { codes: readonly string[]; children?: ReactNode }) {
  const [copied, setCopied] = useState(false);
  const [copyError, setCopyError] = useState(false);
  const timer = useRef<number | undefined>(undefined);

  useEffect(() => () => window.clearTimeout(timer.current), []);

  async function copy(): Promise<void> {
    try {
      await navigator.clipboard.writeText(codes.join('\n'));
      setCopyError(false);
      setCopied(true);
      window.clearTimeout(timer.current);
      timer.current = window.setTimeout(() => setCopied(false), 2000);
    } catch {
      setCopied(false);
      setCopyError(true);
    }
  }

  return <div className="token-created" role="status">
    <p className="token-created-lead">リカバリーコードは今だけ表示します。安全な場所に保管してください。</p>
    <p className="token-created-label"><Phrased text="パスキー・セキュリティキーも認証アプリも使えないときに、パスワードと合わせてログインに使います。1 個につき 1 回だけ使えます。" /></p>
    <ul className="recovery-codes">{codes.map((code) => <li key={code}><code>{code}</code></li>)}</ul>
    <div className="token-created-actions">
      <button className="button secondary small" type="button" onClick={() => { void copy(); }}>{copied ? 'コピーしました' : 'すべてコピー'}</button>
      {children}
    </div>
    {copyError ? <p className="notice error" role="alert">コピーできませんでした。コードを選択して、手動でコピーしてください。</p> : null}
  </div>;
}

function Restart({ disabled }: { disabled: boolean }) {
  return <button className="link-button" type="button" disabled={disabled} onClick={() => auth.cancelMFA()}>ログインをやり直す</button>;
}

// Verify asks an enrolled user for the second factor.
function Verify({ pending, leave }: { pending: PendingMFA; leave: () => void }) {
  const key = pending.methods.webauthn && webauthnSupported();
  const totp = pending.methods.totp;
  const recovery = pending.methods.recovery;
  const [useRecovery, setUseRecovery] = useState(!key && !totp && recovery);
  const [code, setCode] = useState('');
  const [recoveryCode, setRecoveryCode] = useState('');
  const [busy, setBusy] = useState<'key' | 'totp' | 'recovery' | null>(null);
  const [error, setError] = useState<string | null>(null);
  const headingID = useId();
  const codeID = useId();
  const recoveryID = useId();
  const errorID = useId();

  async function withKey(): Promise<void> {
    if (busy) return;
    setBusy('key');
    setError(null);
    try {
      const begin = readWebAuthnBegin(await auth.beginMFA('/auth/mfa/webauthn/begin'));
      const credential = await getCredential(begin.options);
      await auth.verifyMFA('/auth/mfa/webauthn/finish', { credential }, leave);
      return;
    } catch (failure) {
      if (isSuperseded(failure)) return;
      setError(webauthnErrorMessage(failure) ?? mfaErrorMessage(failure, 'key'));
    }
    setBusy(null);
  }

  async function submit(event: FormEvent<HTMLFormElement>, method: 'totp' | 'recovery'): Promise<void> {
    event.preventDefault();
    if (busy) return;
    const value = method === 'totp' ? code : recoveryCode;
    if (!value.trim()) {
      setError(method === 'totp' ? '認証アプリのコードを入力してください。' : 'リカバリーコードを入力してください。');
      return;
    }
    setBusy(method);
    setError(null);
    try {
      await auth.verifyMFA(method === 'totp' ? '/auth/mfa/totp' : '/auth/mfa/recovery', { code: value }, leave);
      return;
    } catch (failure) {
      if (isSuperseded(failure)) return;
      setError(mfaErrorMessage(failure, 'code'));
      // A code is spent or wrong either way; it is typed again.
      if (method === 'totp') setCode('');
    }
    setBusy(null);
  }

  const none = !key && !totp && !recovery;
  const waiting = busy !== null;

  return <section className="gate-card" aria-labelledby={headingID}>
    <h1 id={headingID}>二段階認証</h1>
    <p className="gate-lead"><Phrased text={none ? 'このブラウザーで使える確認の方法がありません。' : 'ログインを続けるには、登録した方法で本人確認をしてください。'} /></p>
    {none ? <p className="notice"><Phrased text={pending.methods.webauthn
      ? 'このブラウザーはパスキー・セキュリティキーに対応していません。対応しているブラウザーでログインしてください。'
      : '登録したパスキー・セキュリティキーは、この Astovia の今のアドレスでは使えません。この Astovia の運用者に連絡してください。'} /></p> : null}
    {error ? <p id={errorID} className="notice error" role="alert">{error}</p> : null}
    {key && !useRecovery ? <div className="gate-choices">
      <button className="button primary block" type="button" disabled={waiting} onClick={() => { void withKey(); }}>{busy === 'key' ? '確認中…' : 'パスキー・セキュリティキーで確認'}</button>
    </div> : null}
    {totp && !useRecovery ? <form className="gate-form" onSubmit={(event) => { void submit(event, 'totp'); }} aria-busy={busy === 'totp'} noValidate>
      <TOTPCodeField id={codeID} value={code} onChange={(value) => { setCode(value); setError(null); }} disabled={waiting} describedBy={error ? errorID : undefined} />
      <button className={`button ${key ? 'secondary' : 'primary'} block`} type="submit" disabled={waiting}>{busy === 'totp' ? '確認中…' : 'コードで確認'}</button>
    </form> : null}
    {useRecovery ? <form className="gate-form" onSubmit={(event) => { void submit(event, 'recovery'); }} aria-busy={busy === 'recovery'} noValidate>
      <div className="field">
        <label htmlFor={recoveryID}>リカバリーコード</label>
        <input id={recoveryID} className="code-input" name="recovery-code" type="text" autoComplete="off" autoCapitalize="none" spellCheck={false} maxLength={40} placeholder="xxxx-xxxx-xxxx-xxxx" value={recoveryCode} onChange={(e) => { setRecoveryCode(e.target.value); setError(null); }} disabled={waiting} aria-describedby={error ? errorID : undefined} />
      </div>
      <p className="field-hint">使ったコードは、もう使えなくなります。</p>
      <button className="button primary block" type="submit" disabled={waiting}>{busy === 'recovery' ? '確認中…' : 'リカバリーコードで確認'}</button>
    </form> : null}
    <div className="gate-links">
      {recovery && !useRecovery ? <button className="link-button" type="button" disabled={waiting} onClick={() => { setUseRecovery(true); setError(null); }}>リカバリーコードを使う</button> : null}
      {useRecovery && (key || totp) ? <button className="link-button" type="button" disabled={waiting} onClick={() => { setUseRecovery(false); setError(null); }}>他の方法で確認する</button> : null}
      <Restart disabled={waiting} />
    </div>
  </section>;
}

// Enroll makes a user that must have a second factor register the first
// one. The session the server starts is held back by auth.ts until the
// recovery codes are saved (onCodes).
function Enroll({ pending, onCodes }: { pending: PendingMFA; onCodes: (codes: string[]) => void }) {
  const supported = webauthnSupported();
  const [view, setView] = useState<'choose' | 'key' | 'totp'>('choose');
  const [setup, setSetup] = useState<TOTPSetup | null>(null);
  const [name, setName] = useState('');
  const [code, setCode] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const headingID = useId();
  const nameID = useId();
  const nameHintID = useId();
  const codeID = useId();
  const errorID = useId();

  function choose(): void {
    setView('choose');
    setSetup(null);
    setCode('');
    setError(null);
  }

  async function startTOTP(): Promise<void> {
    if (busy) return;
    setBusy(true);
    setError(null);
    try {
      setSetup(readTOTPSetup(await auth.beginMFA('/auth/mfa/enroll/totp/begin')));
      setView('totp');
    } catch (failure) {
      if (isSuperseded(failure)) return;
      setError(mfaErrorMessage(failure, 'code'));
    }
    setBusy(false);
  }

  async function finishTOTP(event: FormEvent<HTMLFormElement>): Promise<void> {
    event.preventDefault();
    if (busy || !setup) return;
    if (!code.trim()) {
      setError('認証アプリのコードを入力してください。');
      return;
    }
    setBusy(true);
    setError(null);
    try {
      onCodes(await auth.enrollMFA('/auth/mfa/enroll/totp/finish', { setup_token: setup.setup_token, code }));
      return;
    } catch (failure) {
      if (isSuperseded(failure)) return;
      setError(mfaErrorMessage(failure, 'code'));
      setCode('');
    }
    setBusy(false);
  }

  async function registerKey(event: FormEvent<HTMLFormElement>): Promise<void> {
    event.preventDefault();
    if (busy) return;
    const label = credentialName(name);
    if (!label) {
      setError(`名前を 1〜${maxCredentialNameLength} 文字で入力してください。`);
      return;
    }
    setBusy(true);
    setError(null);
    try {
      const begin = readWebAuthnBegin(await auth.beginMFA('/auth/mfa/enroll/webauthn/begin'));
      const credential = await createCredential(begin.options);
      onCodes(await auth.enrollMFA('/auth/mfa/enroll/webauthn/finish', { credential, name: label }));
      return;
    } catch (failure) {
      if (isSuperseded(failure)) return;
      setError(webauthnErrorMessage(failure) ?? mfaErrorMessage(failure, 'key'));
    }
    setBusy(false);
  }

  return <section className="gate-card" aria-labelledby={headingID}>
    <h1 id={headingID}>二段階認証の登録</h1>
    <p className="gate-lead"><Phrased text={pending.webauthnAvailable
      ? 'この Astovia では二段階認証が必要です。パスキー・セキュリティキーか認証アプリのどちらかを登録してください。'
      : 'この Astovia では二段階認証が必要です。認証アプリを登録してください。'} /></p>
    {error ? <p id={errorID} className="notice error" role="alert">{error}</p> : null}
    {view === 'choose' ? <div className="gate-choices">
      {pending.webauthnAvailable ? <button className="button primary block" type="button" disabled={busy || !supported} onClick={() => { setView('key'); setError(null); }}>パスキー・セキュリティキーを登録</button> : null}
      {pending.webauthnAvailable && !supported ? <p className="field-hint">このブラウザーはパスキー・セキュリティキーに対応していません。</p> : null}
      <button className={`button ${pending.webauthnAvailable && supported ? 'secondary' : 'primary'} block`} type="button" disabled={busy} onClick={() => { void startTOTP(); }}>{busy ? '準備中…' : '認証アプリを登録'}</button>
    </div> : null}
    {view === 'key' ? <form className="gate-form" onSubmit={(event) => { void registerKey(event); }} aria-busy={busy} noValidate>
      <div className="field">
        <label htmlFor={nameID}>パスキー・セキュリティキーの名前</label>
        <input id={nameID} name="name" type="text" autoComplete="off" maxLength={maxCredentialNameLength} placeholder="例: ノートパソコン" value={name} onChange={(e) => { setName(e.target.value); setError(null); }} disabled={busy} aria-describedby={error ? `${nameHintID} ${errorID}` : nameHintID} />
      </div>
      <p className="field-hint" id={nameHintID}>あとで見分けるための名前です。次に進むと、ブラウザーが登録の画面を出します。</p>
      <button className="button primary block" type="submit" disabled={busy}>{busy ? '登録中…' : '登録する'}</button>
    </form> : null}
    {view === 'totp' && setup ? <form className="gate-form" onSubmit={(event) => { void finishTOTP(event); }} aria-busy={busy} noValidate>
      <TOTPSetupDetails setup={setup} />
      <TOTPCodeField id={codeID} value={code} onChange={(value) => { setCode(value); setError(null); }} disabled={busy} describedBy={error ? errorID : undefined} />
      <button className="button primary block" type="submit" disabled={busy}>{busy ? '確認中…' : '確認して登録'}</button>
    </form> : null}
    <div className="gate-links">
      {view !== 'choose' ? <button className="link-button" type="button" disabled={busy} onClick={choose}>他の方法を選ぶ</button> : null}
      <Restart disabled={busy} />
    </div>
  </section>;
}

// SaveCodes shows the recovery codes of a new enrollment. Nothing else is
// offered: the sign-in is finished only once the user has saved them.
function SaveCodes({ codes, leave }: { codes: readonly string[]; leave: () => void }) {
  const [busy, setBusy] = useState(false);
  const headingID = useId();

  async function done(): Promise<void> {
    if (busy) return;
    setBusy(true);
    try {
      await auth.finishEnrollment(leave);
    } catch {
      // The sign-in ended in another tab; this screen is going away.
    }
  }

  return <section className="gate-card" aria-labelledby={headingID}>
    <h1 id={headingID}>リカバリーコードを保存</h1>
    <p className="gate-lead"><Phrased text="二段階認証を登録しました。リカバリーコードを保存してから、先へ進んでください。" /></p>
    <div className="gate-choices">
      <RecoveryCodes codes={codes} />
      <button className="button primary block" type="button" disabled={busy} onClick={() => { void done(); }}>保存した</button>
    </div>
  </section>;
}

// MFAStep replaces the login form while a password sign-in waits for its
// second factor, or for the enrollment of one where it is required. It is
// shown on the invitation and password reset screens and before the consent
// page as well. Nothing it holds is stored outside the memory of the page.
export default function MFAStep({ pending }: { pending: PendingMFA }) {
  const navigate = useNavigate();
  const pathname = useLocation().pathname;
  const [codes, setCodes] = useState<string[] | null>(null);

  // The invitation and password reset screens are for signed-out users;
  // they are left right before the tab becomes signed in.
  function leave(): void {
    if (linkPageAt(pathname)) void navigate('/', { replace: true });
  }

  if (codes) return <SaveCodes codes={codes} leave={leave} />;
  return pending.enrolled ? <Verify pending={pending} leave={leave} /> : <Enroll pending={pending} onCodes={setCodes} />;
}
