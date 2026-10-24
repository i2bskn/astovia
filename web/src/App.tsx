import { useEffect, useId, useState, useSyncExternalStore } from 'react';
import type { FormEvent } from 'react';
import { Link, Navigate, NavLink, Route, Routes, useLocation, useMatch } from 'react-router';
import { auth } from './auth';
import ActionItemList from './ActionItemList';
import APITokenList from './APITokenList';
import AutoItemList from './AutoItemList';
import ConnectionList from './ConnectionList';
import ConsentPage from './ConsentPage';
import GitHubSettings from './GitHubSettings';
import { AutoIcon, BellIcon, KeyIcon, PlugIcon, SettingsIcon, SidebarIcon, SourceIcon, TasksIcon, UserIcon } from './icons';
import NotificationSettings from './NotificationSettings';
import Phrased from './Phrased';
import { views } from './view';
import type { View } from './view';

const viewIcons: Record<View, () => React.JSX.Element> = {
  tasks: () => <TasksIcon />,
  auto: () => <AutoIcon />,
  settings: () => <SettingsIcon />,
};

// The wordmark has a dark-ink and a light-ink variant for the color schemes.
function Logo({ width = 132 }: { width?: number }) {
  return <picture className="logo">
    <source srcSet="/admin/astovia-logo-dark.svg" media="(prefers-color-scheme: dark)" />
    <img src="/admin/astovia-logo.svg" alt="Astovia" width={width} height={width / 3} />
  </picture>;
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

  return <section className="gate-card" aria-labelledby="login-heading">
    <h1 id="login-heading">おかえりなさい</h1>
    <p className="gate-lead"><Phrased text="ログインして、今日のタスクを確認しましょう。" /></p>
    <form className="gate-form" onSubmit={submit} aria-busy={busy} noValidate>
      <div className="field">
        <label htmlFor="email">メールアドレス</label>
        <input id="email" name="email" type="email" autoComplete="username" autoCapitalize="none" spellCheck={false} required maxLength={254} value={email} onChange={(e) => { setEmail(e.target.value); setValidation(null); }} disabled={busy} aria-describedby={error ? errorID : undefined} />
      </div>
      <div className="field">
        <label htmlFor="password">パスワード</label>
        <input id="password" name="password" type="password" autoComplete="current-password" required value={password} onChange={(e) => { setPassword(e.target.value); setValidation(null); }} disabled={busy} aria-describedby={error ? errorID : undefined} />
      </div>
      {error ? <p id={errorID} className="notice error" role="alert">{error}</p> : null}
      <button className="button primary block" type="submit" disabled={busy}>{busy ? 'ログイン中…' : 'ログイン'}</button>
    </form>
    <p className="gate-help"><Phrased text="アカウントの作成やパスワードの再設定は、この Astovia の運用者に連絡してください。" /></p>
  </section>;
}

// Gate frames the screens shown before the workspace: sign-in, the session
// check and the OAuth consent page.
function Gate({ children }: { children: React.ReactNode }) {
  return <div className="gate">
    <header className="gate-header"><Logo width={144} /></header>
    <main className="gate-main">{children}</main>
    <footer className="gate-footer">「忘れてた！」を防ぐタスク管理 · セルフホスト版</footer>
  </div>;
}

const sidebarKey = 'astovia.sidebar';

function initialCollapsed(): boolean {
  try {
    const stored = localStorage.getItem(sidebarKey);
    if (stored) return stored === 'collapsed';
  } catch {
    // Storage can be unavailable (private mode); fall back to the width.
  }
  return window.matchMedia?.('(max-width: 1199px)').matches ?? false;
}

// Sign-out lives in Settings > Account, one step away, so that it is not
// pressed by accident next to the menu toggle.
function Sidebar({ email, collapsed, onToggle }: { email: string; collapsed: boolean; onToggle: () => void }) {
  return <aside className="sidebar" aria-label="メニュー">
    <Link to="/" className="sidebar-brand" aria-label="Astovia タスク">
      {collapsed ? <img className="sidebar-mark" src="/admin/favicon.svg" alt="" width={28} height={28} /> : <Logo />}
    </Link>
    <nav className="sidebar-nav" aria-label="画面">
      {views.map((v) => <NavLink key={v.value} to={v.path} end className="nav-item" title={collapsed ? v.label : undefined}>
        {viewIcons[v.value]()}<span className="nav-label">{collapsed ? v.short : v.label}</span>
      </NavLink>)}
    </nav>
    <div className="sidebar-foot">
      <Link to="/settings#account" className="nav-item sidebar-account" title={email}>
        <span className="avatar" aria-hidden="true">{email.slice(0, 1).toUpperCase()}</span>
        {collapsed ? <span className="nav-label">アカウント</span> : <span className="nav-label account-email"><span className="sr-only">アカウント: </span>{email}</span>}
      </Link>
      <button className="nav-item sidebar-toggle" type="button" aria-expanded={!collapsed} title={collapsed ? 'メニューを広げる' : undefined} onClick={onToggle}>
        <SidebarIcon /><span className="nav-label">{collapsed ? '広げる' : 'メニューを畳む'}</span>{collapsed ? <span className="sr-only">（メニュー）</span> : null}
      </button>
    </div>
  </aside>;
}

// TabBar replaces the sidebar on phones, where it sits under the thumb.
function TabBar() {
  return <nav className="tabbar" aria-label="画面">
    {views.map((v) => <NavLink key={v.value} to={v.path} end className="tab-item">
      {viewIcons[v.value]()}<span>{v.short}</span>
    </NavLink>)}
  </nav>;
}

function AccountSettings({ email }: { email: string }) {
  const state = useSyncExternalStore(auth.subscribe, auth.getSnapshot);
  const headingID = useId();
  return <section className="action-panel settings-panel" id="account" aria-labelledby={headingID}>
    <h2 id={headingID}>アカウント</h2>
    <div className="account-row">
      <span className="avatar large" aria-hidden="true">{email.slice(0, 1).toUpperCase()}</span>
      <p className="account-row-email"><span className="sr-only">ログイン中: </span>{email}</p>
      <button className="button secondary" type="button" disabled={state.busy === 'logout'} onClick={() => { void auth.logout(); }}>{state.busy === 'logout' ? 'ログアウト中…' : 'ログアウト'}</button>
    </div>
  </section>;
}

function PageHeader({ title, lead }: { title: string; lead: string }) {
  return <header className="page-header"><h1>{title}</h1><p className="page-lead"><Phrased text={lead} /></p></header>;
}

const settingsSections = [
  { id: 'github', label: 'GitHub 連携', icon: <SourceIcon size={16} /> },
  { id: 'notifications', label: '通知', icon: <BellIcon size={16} /> },
  { id: 'connections', label: 'AI クライアント', icon: <PlugIcon size={16} /> },
  { id: 'tokens', label: 'API トークン', icon: <KeyIcon size={16} /> },
  { id: 'account', label: 'アカウント', icon: <UserIcon size={16} /> },
];

const titles: Record<string, string> = { '/': 'タスク', '/auto': '自動で処理したもの', '/settings': '設定' };

function Workspace({ email, error }: { email: string; error: string | null }) {
  const [collapsed, setCollapsed] = useState(initialCollapsed);
  const location = useLocation();

  // A route change starts at the top of the new screen, or at the section a
  // hash names, with the screen's name in the tab title.
  useEffect(() => {
    const target = location.hash ? document.getElementById(decodeURIComponent(location.hash.slice(1))) : null;
    if (target) target.scrollIntoView();
    else window.scrollTo(0, 0);
    const title = titles[location.pathname];
    document.title = title ? `${title} – Astovia` : 'Astovia';
  }, [location.pathname, location.hash]);

  function toggle(): void {
    setCollapsed((current) => {
      try {
        localStorage.setItem(sidebarKey, current ? 'expanded' : 'collapsed');
      } catch {
        // Not remembered; the toggle still works for this page.
      }
      return !current;
    });
  }

  return <div className="workspace" data-sidebar={collapsed ? 'collapsed' : 'expanded'}>
    <a className="skip-link" href="#main">本文へ移動</a>
    <Sidebar email={email} collapsed={collapsed} onToggle={toggle} />
    <header className="topbar"><Link to="/" aria-label="Astovia タスク"><Logo width={112} /></Link></header>
    <main className="workspace-main" id="main" tabIndex={-1}>
      <div className="page">
        {error ? <p className="notice error" role="alert">{error}</p> : null}
        <Routes>
          <Route index element={<>
            <PageHeader title="タスク" lead="自分で追加したものと、連携先から見つけたものを、まとめて確認できます。" />
            <ActionItemList />
          </>} />
          <Route path="auto" element={<>
            <PageHeader title="自動で処理したもの" lead="連携先の状態を見て、Astovia が自動で片付けたタスクです。間違っていたら未完了に戻してください。戻したものは、以後自動では処理しません。" />
            <AutoItemList />
          </>} />
          <Route path="settings" element={<>
            <PageHeader title="設定" lead="連携するサービス、通知、AI クライアントからの接続を管理します。" />
            <nav className="settings-nav" aria-label="設定の項目">
              {settingsSections.map((s) => <Link key={s.id} to={`#${s.id}`}>{s.icon}{s.label}</Link>)}
            </nav>
            <div className="settings-stack">
              <GitHubSettings />
              <NotificationSettings />
              <ConnectionList />
              <APITokenList />
              <AccountSettings email={email} />
            </div>
          </>} />
          <Route path="*" element={<Navigate to="/" replace />} />
        </Routes>
      </div>
    </main>
    <TabBar />
  </div>;
}

export default function App() {
  const state = useSyncExternalStore(auth.subscribe, auth.getSnapshot);
  const signedIn = state.status === 'authenticated' && state.user !== null;
  // The OAuth consent page replaces the workspace on this path.
  const consentPage = useMatch('/oauth/authorize') !== null;

  if (signedIn && !consentPage) return <Workspace email={state.user?.email ?? ''} error={state.error} />;

  return <Gate>
    {state.status === 'loading' ? <section className="gate-card status-card" aria-live="polite" aria-busy="true"><span className="loading-dot" aria-hidden="true" /><p>ログイン状態を確認しています…</p></section> : null}
    {state.status === 'unavailable' ? <section className="gate-card status-card" aria-labelledby="connection-heading"><h1 id="connection-heading">接続を確認してください</h1><p className="gate-lead" role="alert">{state.error}</p><button className="button primary block" onClick={() => auth.retry()}>もう一度接続する</button></section> : null}
    {state.status === 'anonymous' ? <LoginForm /> : null}
    {signedIn && consentPage ? <ConsentPage email={state.user?.email ?? ''} /> : null}
  </Gate>;
}
