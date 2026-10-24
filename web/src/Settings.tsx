import { useCallback, useEffect, useId, useState, useSyncExternalStore } from 'react';
import { Link, Navigate, NavLink, Route, Routes, useLocation } from 'react-router';
import { auth, request } from './auth';
import APITokenList from './APITokenList';
import { createAPITokensClient } from './apiTokens';
import ConnectionList from './ConnectionList';
import GitHubSettings from './GitHubSettings';
import { BellIcon, ChevronLeftIcon, ChevronRightIcon, PlugIcon, SourceIcon, UserIcon } from './icons';
import { createIntegrationsClient } from './integrations';
import NotificationSettings from './NotificationSettings';
import { createOAuthClient } from './oauth';
import Phrased from './Phrased';
import { clientsStatus, devicesStatus, githubStatus, settingsPageAt, settingsPages } from './settingsPages';
import type { SettingsPage } from './settingsPages';

const integrations = createIntegrationsClient(request);
const oauth = createOAuthClient(request);
const apiTokens = createAPITokensClient(request);

const pageIcons: Record<SettingsPage, () => React.JSX.Element> = {
  integrations: () => <SourceIcon size={20} />,
  notifications: () => <BellIcon />,
  'ai-clients': () => <PlugIcon />,
  account: () => <UserIcon />,
};

const phoneQuery = '(max-width: 767px)';

function subscribePhone(onChange: () => void): () => void {
  const list = window.matchMedia?.(phoneQuery);
  list?.addEventListener('change', onChange);
  return () => list?.removeEventListener('change', onChange);
}

function usePhone(): boolean {
  return useSyncExternalStore(subscribePhone, useCallback(() => window.matchMedia?.(phoneQuery).matches ?? false, []));
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

// SettingsIndex lists the pages on phones, with the state of each so that
// what needs attention is visible before opening it.
function SettingsIndex({ email }: { email: string }) {
  const [status, setStatus] = useState<Partial<Record<SettingsPage, string>>>({});

  useEffect(() => {
    let active = true;
    void Promise.allSettled([integrations.getGitHub(), integrations.listPushDevices(), oauth.listConnections(), apiTokens.listAPITokens()])
      .then(([github, devices, connections, tokens]) => {
        if (!active) return;
        // A page whose state could not be read keeps its summary.
        setStatus({
          integrations: github.status === 'fulfilled' ? githubStatus(github.value) : undefined,
          notifications: devices.status === 'fulfilled' ? devicesStatus(devices.value) : undefined,
          'ai-clients': connections.status === 'fulfilled' && tokens.status === 'fulfilled' ? clientsStatus(connections.value, tokens.value) : undefined,
        });
      });
    return () => { active = false; };
  }, []);

  return <ul className="settings-list">
    {settingsPages.map((p) => <li key={p.value}>
      <Link className="settings-link" to={`/settings/${p.path}`}>
        <span className="settings-link-icon">{pageIcons[p.value]()}</span>
        <span className="settings-link-body">
          <span className="settings-link-label">{p.label}</span>
          <span className="settings-link-status">{p.value === 'account' ? email : status[p.value] ?? p.summary}</span>
        </span>
        <ChevronRightIcon />
      </Link>
    </li>)}
  </ul>;
}

// Settings has a page per errand. Wider screens switch pages with tabs and
// open on the first one; phones open on a list and each page links back.
export default function Settings({ email }: { email: string }) {
  const location = useLocation();
  const page = settingsPageAt(location.pathname);
  const phone = usePhone();

  return <>
    <header className="page-header settings-header" data-page={page ?? undefined}>
      {page ? <Link className="settings-back" to="/settings"><ChevronLeftIcon />設定</Link> : null}
      <h1>設定</h1>
      <p className="page-lead"><Phrased text="連携するサービス、通知、AI クライアントからの接続を管理します。" /></p>
    </header>
    <nav className="settings-tabs" aria-label="設定の項目">
      {settingsPages.map((p) => <NavLink key={p.value} className="settings-tab" to={`/settings/${p.path}`}>{pageIcons[p.value]()}{p.label}</NavLink>)}
    </nav>
    <Routes>
      <Route index element={phone ? <SettingsIndex email={email} /> : <Navigate to="/settings/integrations" replace />} />
      <Route path="integrations" element={<GitHubSettings />} />
      <Route path="notifications" element={<NotificationSettings />} />
      <Route path="ai-clients" element={<div className="settings-stack"><ConnectionList /><APITokenList /></div>} />
      <Route path="account" element={<AccountSettings email={email} />} />
      <Route path="*" element={<Navigate to="/settings" replace />} />
    </Routes>
  </>;
}
