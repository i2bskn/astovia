import { useEffect, useId, useState, useSyncExternalStore } from 'react';
import type { FormEvent } from 'react';
import { useNavigate } from 'react-router';
import { auth, inspectLink } from './auth';
import { dateTime } from './format';
import { linkFailure, linkToken, passwordProblem } from './links';
import type { LinkFailure, LinkPage } from './links';
import Phrased from './Phrased';
import { roleLabels } from './users';
import type { Role } from './users';

interface Inspected {
  email: string;
  role: Role | null;
  expiresAt: string | null;
}

const texts = {
  invite: {
    inspect: '/auth/invitations/inspect',
    submit: '/auth/invitations/accept',
    title: 'アカウントを作成',
    lead: 'Astovia に招待されました。パスワードを決めて、アカウントを作成してください。',
    button: 'アカウントを作成',
    busy: '作成中…',
    invalid: '招待リンクが無効か、期限が切れています。招待したオーナーに、新しいリンクを頼んでください。',
    done: 'アカウントは作成されました。ログイン画面からログインしてください。',
  },
  reset: {
    inspect: '/auth/password-resets/inspect',
    submit: '/auth/password-resets/complete',
    title: 'パスワードの再設定',
    lead: '新しいパスワードを決めてください。',
    button: 'パスワードを再設定',
    busy: '再設定中…',
    invalid: 'パスワード再設定リンクが無効か、期限が切れています。オーナーに、新しいリンクを頼んでください。',
    done: 'パスワードは再設定されました。ログイン画面からログインしてください。',
  },
} as const;

function failureMessage(failure: LinkFailure, page: LinkPage): string {
  switch (failure) {
    case 'invalid': return texts[page].invalid;
    case 'email_taken': return 'このメールアドレスのアカウントは既にあります。ログイン画面からログインしてください。';
    case 'rate_limited': return '試行回数が上限に達しました。1分ほど待ってから、もう一度お試しください。';
    case 'password': return 'パスワードは12〜72バイトで入力してください（英数字なら12〜72文字）。';
    default: return 'サーバーに接続できませんでした。少し待ってから、もう一度お試しください。';
  }
}

function readInspected(data: unknown, page: LinkPage): Inspected {
  if (typeof data !== 'object' || data === null || !('email' in data) || typeof data.email !== 'string') throw new Error('Unexpected link response');
  if (page === 'reset') return { email: data.email, role: null, expiresAt: null };
  if (!('role' in data) || (data.role !== 'owner' && data.role !== 'member') || !('expires_at' in data) || typeof data.expires_at !== 'string') {
    throw new Error('Unexpected link response');
  }
  return { email: data.email, role: data.role, expiresAt: data.expires_at };
}

function StatusCard({ title, text, children }: { title: string; text: string; children?: React.ReactNode }) {
  const headingID = useId();
  return <section className="gate-card status-card" aria-labelledby={headingID}>
    <h1 id={headingID}>{title}</h1>
    <p className="gate-lead" role="alert"><Phrased text={text} /></p>
    {children}
  </section>;
}

// AccountLinkPage is the screen of an invitation (/invite) or password reset
// (/reset-password) link, shown before signing in. The token was taken out
// of the URL when the page loaded (links.ts).
export default function AccountLinkPage({ page }: { page: LinkPage }) {
  const state = useSyncExternalStore(auth.subscribe, auth.getSnapshot);
  const navigate = useNavigate();
  const token = linkToken(page);
  const text = texts[page];
  const [inspected, setInspected] = useState<Inspected | null>(null);
  const [phase, setPhase] = useState<'checking' | 'ready' | 'invalid' | 'failed' | 'done'>('checking');
  const [message, setMessage] = useState<string | null>(null);
  const [attempt, setAttempt] = useState(0);
  const [password, setPassword] = useState('');
  const [confirmation, setConfirmation] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const headingID = useId();
  const emailID = useId();
  const passwordID = useId();
  const confirmationID = useId();
  const errorID = useId();
  const hintID = useId();
  const anonymous = state.status === 'anonymous';

  useEffect(() => {
    if (!token || !anonymous || inspected) return;
    let active = true;
    setPhase('checking');
    inspectLink(text.inspect, token).then((data) => {
      if (!active) return;
      setInspected(readInspected(data, page));
      setPhase('ready');
    }).catch((error: unknown) => {
      if (!active) return;
      const failure = linkFailure(error);
      setMessage(failureMessage(failure, page));
      setPhase(failure === 'invalid' ? 'invalid' : 'failed');
    });
    return () => { active = false; };
  }, [token, anonymous, inspected, attempt, page, text.inspect]);

  async function submit(event: FormEvent<HTMLFormElement>): Promise<void> {
    event.preventDefault();
    if (submitting || !token) return;
    const problem = passwordProblem(password, confirmation);
    if (problem) {
      setMessage(problem);
      return;
    }
    setMessage(null);
    setSubmitting(true);
    try {
      await auth.completeLink(text.submit, token, password);
      void navigate('/', { replace: true });
      return;
    } catch (error) {
      const failure = linkFailure(error);
      if (failure === 'invalid') {
        setMessage(text.invalid);
        setPhase('invalid');
      } else if (failure === 'unavailable') {
        // The account may have been created (or the password reset) with
        // only the sign-in failing; the link is used up in that case.
        try {
          await inspectLink(text.inspect, token);
          setMessage(failureMessage('unavailable', page));
        } catch (again) {
          if (linkFailure(again) === 'invalid') setPhase('done');
          else setMessage('完了したかどうか確認できませんでした。ログイン画面からログインを試すか、少し待ってからもう一度お試しください。');
        }
      } else {
        setMessage(failureMessage(failure, page));
      }
    }
    setSubmitting(false);
  }

  if (!token) {
    return <StatusCard title="リンクが正しくありません" text="受け取ったリンクを、途中で切れないようにそのまま開いてください。">
      <button className="button secondary block" type="button" onClick={() => { void navigate('/', { replace: true }); }}>ログイン画面へ</button>
    </StatusCard>;
  }
  if (phase === 'done') {
    return <StatusCard title={text.title} text={text.done}>
      <button className="button primary block" type="button" onClick={() => { void navigate('/', { replace: true }); }}>ログイン画面へ</button>
    </StatusCard>;
  }
  if (state.status === 'authenticated' && !submitting) {
    return <StatusCard title="別のアカウントでログイン中です" text={`${state.user?.email ?? ''} でログインしています。続けるにはログアウトしてください。`}>
      {state.error ? <p className="notice error" role="alert">{state.error}</p> : null}
      <button className="button primary block" type="button" disabled={state.busy === 'logout'} onClick={() => { void auth.logout(); }}>{state.busy === 'logout' ? 'ログアウト中…' : 'ログアウトして続ける'}</button>
    </StatusCard>;
  }
  if (phase === 'invalid') {
    return <StatusCard title={text.title} text={text.invalid}>
      <button className="button secondary block" type="button" onClick={() => { void navigate('/', { replace: true }); }}>ログイン画面へ</button>
    </StatusCard>;
  }
  if (phase === 'failed') {
    return <StatusCard title={text.title} text={message ?? failureMessage('network', page)}>
      <button className="button primary block" type="button" onClick={() => { setMessage(null); setAttempt((n) => n + 1); }}>もう一度試す</button>
    </StatusCard>;
  }
  if (phase === 'checking' || !inspected) {
    return <section className="gate-card status-card" aria-live="polite" aria-busy="true"><span className="loading-dot" aria-hidden="true" /><p>リンクを確認しています…</p></section>;
  }

  return <section className="gate-card" aria-labelledby={headingID}>
    <h1 id={headingID}>{text.title}</h1>
    <p className="gate-lead"><Phrased text={text.lead} /></p>
    {page === 'invite' && inspected.role && inspected.expiresAt ? <dl className="settings-facts link-facts">
      <div><dt>権限</dt><dd>{roleLabels[inspected.role]}</dd></div>
      <div><dt>リンクの期限</dt><dd>{dateTime(inspected.expiresAt, new Date())}</dd></div>
    </dl> : null}
    {page === 'reset' ? <p className="notice warning"><Phrased text="再設定すると、他の端末からはログアウトされ、API トークンや AI クライアントの接続、AI の設定、通知を受け取る端末の登録は削除されます。" /></p> : null}
    <form className="gate-form" onSubmit={(event) => { void submit(event); }} aria-busy={submitting} noValidate>
      <div className="field">
        <label htmlFor={emailID}>メールアドレス</label>
        <input id={emailID} name="email" type="email" autoComplete="username" value={inspected.email} readOnly />
      </div>
      <div className="field">
        <label htmlFor={passwordID}>新しいパスワード</label>
        <input id={passwordID} name="password" type="password" autoComplete="new-password" required value={password} onChange={(e) => { setPassword(e.target.value); setMessage(null); }} disabled={submitting} aria-describedby={message ? `${hintID} ${errorID}` : hintID} />
      </div>
      <p className="field-hint" id={hintID}>12〜72バイト（英数字なら12〜72文字）</p>
      <div className="field">
        <label htmlFor={confirmationID}>新しいパスワード（確認）</label>
        <input id={confirmationID} name="password-confirmation" type="password" autoComplete="new-password" required value={confirmation} onChange={(e) => { setConfirmation(e.target.value); setMessage(null); }} disabled={submitting} aria-describedby={message ? errorID : undefined} />
      </div>
      {message ? <p id={errorID} className="notice error" role="alert">{message}</p> : null}
      <button className="button primary block" type="submit" disabled={submitting}>{submitting ? text.busy : text.button}</button>
    </form>
  </section>;
}
