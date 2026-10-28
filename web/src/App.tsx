import { useEffect, useId, useState, useSyncExternalStore } from 'react';
import type { FormEvent } from 'react';
import { Link, Navigate, NavLink, Route, Routes, useLocation, useMatch, useParams } from 'react-router';
import { auth } from './auth';
import ActionItemList from './ActionItemList';
import AutoItemList from './AutoItemList';
import CandidateList from './CandidateList';
import { candidates } from './candidates';
import ConsentPage from './ConsentPage';
import { AutoIcon, SettingsIcon, SidebarIcon, SparkleIcon, TasksIcon } from './icons';
import Phrased from './Phrased';
import Settings from './Settings';
import TaskDetail from './TaskDetail';
import { settingsPageAt, settingsPages } from './settingsPages';
import { countLabel, views } from './view';
import type { View } from './view';

const viewIcons: Record<View, () => React.JSX.Element> = {
  tasks: () => <TasksIcon />,
  candidates: () => <SparkleIcon />,
  auto: () => <AutoIcon />,
  settings: () => <SettingsIcon />,
};

// The wordmark has a dark-ink and a light-ink variant for the color schemes.
function Logo({ width = 132 }: { width?: number }) {
  return <picture className="logo">
    <source srcSet="/astovia-logo-dark.svg" media="(prefers-color-scheme: dark)" />
    <img src="/astovia-logo.svg" alt="Astovia" width={width} height={width / 3} />
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

// NavBadge shows how many task candidates wait, on the candidates entry of
// the menus, so that they are noticed from any screen.
function NavBadge({ view }: { view: View }) {
  const state = useSyncExternalStore(candidates.subscribe, candidates.getSnapshot);
  if (view !== 'candidates' || state.items.length === 0) return null;
  return <span className="nav-badge">{countLabel(state.items.length, state.more)}<span className="sr-only">件</span></span>;
}

// ViewLink is an entry of the menus. The tasks entry is current on the task
// list and on a task's detail screen, which NavLink cannot express, so it is
// a Link with aria-current set by hand; the others are NavLinks.
function ViewLink({ view, path, className, title, children }: { view: View; path: string; className: string; title?: string; children: React.ReactNode }) {
  const list = useMatch('/') !== null;
  const detail = useMatch('/tasks/:id') !== null;
  if (view === 'tasks') return <Link to={path} className={className} title={title} aria-current={list || detail ? 'page' : undefined}>{children}</Link>;
  return <NavLink to={path} className={className} title={title}>{children}</NavLink>;
}

// Sign-out lives in Settings > Account, one step away, so that it is not
// pressed by accident next to the menu toggle.
function Sidebar({ email, collapsed, onToggle }: { email: string; collapsed: boolean; onToggle: () => void }) {
  return <aside className="sidebar" aria-label="メニュー">
    <Link to="/" className="sidebar-brand" aria-label="Astovia タスク">
      {collapsed ? <img className="sidebar-mark" src="/favicon.svg" alt="" width={28} height={28} /> : <Logo />}
    </Link>
    <nav className="sidebar-nav" aria-label="画面">
      {views.map((v) => <ViewLink key={v.value} view={v.value} path={v.path} className="nav-item" title={collapsed ? v.label : undefined}>
        {viewIcons[v.value]()}<span className="nav-label">{collapsed ? v.short : v.label}</span><NavBadge view={v.value} />
      </ViewLink>)}
    </nav>
    <div className="sidebar-foot">
      <Link to="/settings/account" className="nav-item sidebar-account" title={email}>
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
    {views.map((v) => <ViewLink key={v.value} view={v.value} path={v.path} className="tab-item">
      {viewIcons[v.value]()}<span>{v.short}</span><NavBadge view={v.value} />
    </ViewLink>)}
  </nav>;
}

function PageHeader({ title, lead }: { title: string; lead: string }) {
  return <header className="page-header"><h1>{title}</h1><p className="page-lead"><Phrased text={lead} /></p></header>;
}

const titles: Record<string, string> = { '/': 'タスク', '/candidates': 'タスク候補', '/auto': '自動で処理したもの', '/settings': '設定' };

// titleAt is the default title of a screen. A task's detail screen replaces
// it with the task's content once the task is read.
function titleAt(pathname: string): string | undefined {
  if (pathname.startsWith('/tasks/')) return 'タスクの詳細';
  const page = settingsPages.find((p) => p.value === settingsPageAt(pathname));
  return page ? `${page.label}の設定` : titles[pathname];
}

// TaskDetailRoute creates the detail screen again for each id, so that no
// state is carried over from one task to another.
function TaskDetailRoute() {
  const { id = '' } = useParams();
  return <TaskDetail key={id} id={id} />;
}

function Workspace({ email, error }: { email: string; error: string | null }) {
  const [collapsed, setCollapsed] = useState(initialCollapsed);
  const location = useLocation();

  // A route change starts at the top of the new screen, or at the section a
  // hash names, with the screen's name in the tab title.
  useEffect(() => {
    const target = location.hash ? document.getElementById(decodeURIComponent(location.hash.slice(1))) : null;
    if (target) target.scrollIntoView();
    else window.scrollTo(0, 0);
    const title = titleAt(location.pathname);
    document.title = title ? `${title} – Astovia` : 'Astovia';
  }, [location.pathname, location.hash]);

  // The candidates are read again on every screen change, for the menu
  // badge, and forgotten on sign-out.
  useEffect(() => { void candidates.refresh(); }, [location.pathname]);
  useEffect(() => () => candidates.reset(), []);

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
          <Route path="candidates" element={<>
            <PageHeader title="タスク候補" lead="AI が自分宛てか判断しきれなかったものです。タスクにするか、不要かを選んでください。そのままにすると、「通知」の設定の日数が過ぎたときに自動で対象外にします。" />
            <CandidateList />
          </>} />
          <Route path="auto" element={<>
            <PageHeader title="自動で処理したもの" lead="連携先の状態を見て、Astovia が自動で片付けたタスクです。間違っていたら未完了に戻してください。戻したものは、以後自動では処理しません。" />
            <AutoItemList />
          </>} />
          <Route path="tasks/:id" element={<TaskDetailRoute />} />
          <Route path="settings/*" element={<Settings email={email} />} />
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
  const consentPage = useMatch('/consent') !== null;

  if (signedIn && !consentPage) return <Workspace email={state.user?.email ?? ''} error={state.error} />;

  return <Gate>
    {state.status === 'loading' ? <section className="gate-card status-card" aria-live="polite" aria-busy="true"><span className="loading-dot" aria-hidden="true" /><p>ログイン状態を確認しています…</p></section> : null}
    {state.status === 'unavailable' ? <section className="gate-card status-card" aria-labelledby="connection-heading"><h1 id="connection-heading">接続を確認してください</h1><p className="gate-lead" role="alert">{state.error}</p><button className="button primary block" onClick={() => auth.retry()}>もう一度接続する</button></section> : null}
    {state.status === 'anonymous' ? <LoginForm /> : null}
    {signedIn && consentPage ? <ConsentPage email={state.user?.email ?? ''} /> : null}
  </Gate>;
}
