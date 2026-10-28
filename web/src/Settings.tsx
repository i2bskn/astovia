import { useCallback, useEffect, useState, useSyncExternalStore } from 'react';
import { Link, Navigate, NavLink, Route, Routes, useLocation } from 'react-router';
import { aiStatus, createAIClient } from './ai';
import AISettings from './AISettings';
import { request } from './auth';
import APITokenList from './APITokenList';
import { createAPITokensClient } from './apiTokens';
import ChatworkSettings from './ChatworkSettings';
import ConnectionList from './ConnectionList';
import GitHubSettings from './GitHubSettings';
import GmailSettings from './GmailSettings';
import { BellIcon, ChevronLeftIcon, ChevronRightIcon, PlugIcon, SourceIcon, SparkleIcon } from './icons';
import { createIntegrationsClient } from './integrations';
import NotificationSettings from './NotificationSettings';
import { createOAuthClient } from './oauth';
import Phrased from './Phrased';
import SlackSettings from './SlackSettings';
import { clientsStatus, devicesStatus, integrationsStatus, settingsPageAt, settingsPages } from './settingsPages';
import type { SettingsPage } from './settingsPages';

const integrations = createIntegrationsClient(request);
const ai = createAIClient(request);
const oauth = createOAuthClient(request);
const apiTokens = createAPITokensClient(request);

const pageIcons: Record<SettingsPage, () => React.JSX.Element> = {
  integrations: () => <SourceIcon size={20} />,
  ai: () => <SparkleIcon />,
  notifications: () => <BellIcon />,
  'ai-clients': () => <PlugIcon />,
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

// SettingsIndex lists the pages on phones, with the state of each so that
// what needs attention is visible before opening it.
function SettingsIndex() {
  const [status, setStatus] = useState<Partial<Record<SettingsPage, string>>>({});

  useEffect(() => {
    let active = true;
    void Promise.allSettled([integrations.getGitHub(), integrations.getSlack(), integrations.getChatwork(), integrations.getGmail(), ai.getAI(), integrations.listPushDevices(), oauth.listConnections(), apiTokens.listAPITokens()])
      .then(([github, slack, chatwork, gmail, aiState, devices, connections, tokens]) => {
        if (!active) return;
        // A page whose state could not be read keeps its summary.
        setStatus({
          integrations: integrationsStatus(github.status === 'fulfilled' ? github.value : undefined, slack.status === 'fulfilled' ? slack.value : undefined, chatwork.status === 'fulfilled' ? chatwork.value : undefined, gmail.status === 'fulfilled' ? gmail.value : undefined),
          ai: aiState.status === 'fulfilled' ? aiStatus(aiState.value) : undefined,
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
          <span className="settings-link-status">{status[p.value] ?? p.summary}</span>
        </span>
        <ChevronRightIcon />
      </Link>
    </li>)}
  </ul>;
}

// Settings has a page per errand. Wider screens switch pages with tabs and
// open on the first one; phones open on a list and each page links back.
// The account pages moved to /account; their old URLs lead there.
export default function Settings() {
  const location = useLocation();
  const page = settingsPageAt(location.pathname);
  const phone = usePhone();

  return <>
    <header className="page-header settings-header" data-page={page ?? undefined}>
      {page ? <Link className="settings-back" to="/settings"><ChevronLeftIcon />設定</Link> : null}
      <h1>設定</h1>
      <p className="page-lead"><Phrased text="連携するサービス、AI、通知、AI クライアントからの接続を管理します。" /></p>
    </header>
    <nav className="settings-tabs" aria-label="設定の項目">
      {settingsPages.map((p) => <NavLink key={p.value} className="settings-tab" to={`/settings/${p.path}`}>{pageIcons[p.value]()}{p.label}</NavLink>)}
    </nav>
    <Routes>
      <Route index element={phone ? <SettingsIndex /> : <Navigate to="/settings/integrations" replace />} />
      <Route path="integrations" element={<div className="settings-stack"><GitHubSettings /><SlackSettings /><ChatworkSettings /><GmailSettings /></div>} />
      <Route path="ai" element={<AISettings />} />
      <Route path="notifications" element={<NotificationSettings />} />
      <Route path="ai-clients" element={<div className="settings-stack"><ConnectionList /><APITokenList /></div>} />
      <Route path="account" element={<Navigate to="/account" replace />} />
      <Route path="users" element={<Navigate to="/account/users" replace />} />
      <Route path="*" element={<Navigate to="/settings" replace />} />
    </Routes>
  </>;
}
