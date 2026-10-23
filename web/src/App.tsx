import { useId, useState, useSyncExternalStore } from 'react';
import type { FormEvent } from 'react';
import { auth } from './auth';
import ActionItemList from './ActionItemList';
import APITokenList from './APITokenList';

function Brand() {
  return <div className="brand" aria-label="Astovia"><span className="brand-mark" aria-hidden="true">a</span><span>Astovia</span></div>;
}

function LoginForm() {
  const state = useSyncExternalStore(auth.subscribe, auth.getSnapshot);
  const [email, setEmail] = useState('');
  const [password, setPassword] = useState('');
  const [validation, setValidation] = useState<string | null>(null);
  const errorID = useId();
  const busy = state.busy === 'login';
  const error = validation ?? state.error;

  function submit(event: FormEvent<HTMLFormElement>): void {
    event.preventDefault();
    if (busy) return;
    if (!email.trim() || !password) {
      setValidation('メールアドレスとパスワードを入力してください。');
      return;
    }
    if (new TextEncoder().encode(password).length > 72) {
      setValidation('パスワードは72バイト以内で入力してください。');
      return;
    }
    setValidation(null);
    void auth.login(email, password);
  }

  return <section className="login-card" aria-labelledby="login-heading">
    <div className="section-label">YOUR WORKSPACE</div>
    <h1 id="login-heading">おかえりなさい</h1>
    <p className="intro">自分のAstoviaにログイン。</p>
    <form onSubmit={submit} aria-busy={busy} noValidate>
      <div className="field">
        <label htmlFor="email">メールアドレス</label>
        <input id="email" name="email" type="email" autoComplete="username" autoCapitalize="none" spellCheck={false} required maxLength={254} value={email} onChange={(e) => { setEmail(e.target.value); setValidation(null); }} disabled={busy} aria-describedby={error ? errorID : undefined} />
      </div>
      <div className="field">
        <label htmlFor="password">パスワード</label>
        <input id="password" name="password" type="password" autoComplete="current-password" required value={password} onChange={(e) => { setPassword(e.target.value); setValidation(null); }} disabled={busy} aria-describedby={error ? errorID : undefined} />
      </div>
      {error ? <p id={errorID} className="notice error" role="alert">{error}</p> : null}
      <button className="button primary" type="submit" disabled={busy}>{busy ? 'ログイン中…' : 'ログイン'}<span aria-hidden="true">→</span></button>
    </form>
    <p className="form-help">アカウントの作成・パスワードの再設定は、<br className="desktop-break" />このAstoviaの運用者に連絡してください。</p>
  </section>;
}

export default function App() {
  const state = useSyncExternalStore(auth.subscribe, auth.getSnapshot);
  const signedIn = state.status === 'authenticated' && state.user !== null;

  return <div className="app-shell">
    <header className="site-header"><Brand /><span className="instance-label">あなたの環境、あなたのInbox。</span></header>
    <main className={signedIn ? 'main-content dashboard' : 'main-content'}>
      {state.status === 'loading' ? <section className="status-card" aria-live="polite" aria-busy="true"><span className="loading-dot" aria-hidden="true" /><p>ログイン状態を確認しています…</p></section> : null}
      {state.status === 'unavailable' ? <section className="status-card" aria-labelledby="connection-heading"><h1 id="connection-heading">接続を確認してください</h1><p className="intro" role="alert">{state.error}</p><button className="button primary" onClick={() => auth.retry()}>もう一度接続する<span aria-hidden="true">↻</span></button></section> : null}
      {state.status === 'anonymous' ? <LoginForm /> : null}
      {signedIn ? <>
        <div className="dashboard-heading"><div><div className="section-label">YOUR WORKSPACE</div><h1>こんにちは、<span className="user-email">{state.user?.email}</span></h1><p className="intro">Astoviaにログインしています。</p></div><button className="button secondary" disabled={state.busy === 'logout'} onClick={() => { void auth.logout(); }}>{state.busy === 'logout' ? 'ログアウト中…' : 'ログアウト'}</button></div>
        {state.error ? <p className="notice error" role="alert">{state.error}</p> : null}
        <ActionItemList />
        <APITokenList />
        <p className="privacy-note"><span aria-hidden="true">◈</span> この画面は、ログインした本人のためのスペースです。</p>
      </> : null}
    </main>
    <footer className="site-footer"><span>会話から生まれる、小さな用事の置き場。</span><span>Astovia · Self-hosted</span></footer>
  </div>;
}
