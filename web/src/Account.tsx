import { useId, useSyncExternalStore } from 'react';
import { Navigate, NavLink, Route, Routes, useLocation } from 'react-router';
import { auth, logoutAccess } from './auth';
import type { Role } from './auth';
import { authConfig } from './authConfig';
import { UserIcon, UsersIcon } from './icons';
import Phrased from './Phrased';
import { accountPageAt, visibleAccountPages } from './settingsPages';
import type { AccountPage } from './settingsPages';
import UserSettings from './UserSettings';

const pageIcons: Record<AccountPage, () => React.JSX.Element> = {
  account: () => <UserIcon />,
  users: () => <UsersIcon />,
};

// Sign-out lives here, one step away from the menu, so that it is not
// pressed by accident.
// With Cloudflare Access, a second button also ends the Access session, so
// that another account can sign in.
function AccountSettings({ email }: { email: string }) {
  const state = useSyncExternalStore(auth.subscribe, auth.getSnapshot);
  const config = useSyncExternalStore(authConfig.subscribe, authConfig.getSnapshot).config;
  const headingID = useId();
  const busy = state.busy === 'logout';
  return <section className="action-panel settings-panel" id="account" aria-labelledby={headingID}>
    <h2 id={headingID}>アカウント</h2>
    <div className="account-row">
      <span className="avatar large" aria-hidden="true">{email.slice(0, 1).toUpperCase()}</span>
      <p className="account-row-email"><span className="sr-only">ログイン中: </span>{email}</p>
      <button className="button secondary" type="button" disabled={busy} onClick={() => { void auth.logout(); }}>{busy ? 'ログアウト中…' : 'ログアウト'}</button>
      {config?.mode === 'cloudflare_access'
        ? <button className="button secondary" type="button" disabled={busy} onClick={() => { void logoutAccess(config.logoutURL); }}>Cloudflare Access からもログアウト</button>
        : null}
    </div>
  </section>;
}

// Account holds the signed-in account and, for owners, user management.
// Members have one page and see no tabs; a member opening the user
// management URL goes back to /account.
export default function Account({ userID, email, role }: { userID: string; email: string; role: Role }) {
  const location = useLocation();
  const page = accountPageAt(location.pathname);
  const pages = visibleAccountPages(role);

  return <>
    <header className="page-header" data-page={page ?? undefined}>
      <h1>アカウント</h1>
      <p className="page-lead"><Phrased text={role === 'owner' ? 'ログイン中のアカウントと、この Astovia を使うユーザーを管理します。' : 'ログイン中のアカウントを確認します。'} /></p>
    </header>
    {pages.length > 1 ? <nav className="settings-tabs account-tabs" aria-label="アカウントの項目">
      {pages.map((p) => <NavLink key={p.value} className="settings-tab" to={p.path ? `/account/${p.path}` : '/account'} end>{pageIcons[p.value]()}{p.label}</NavLink>)}
    </nav> : null}
    <Routes>
      <Route index element={<AccountSettings email={email} />} />
      {role === 'owner' ? <Route path="users" element={<UserSettings userID={userID} />} /> : null}
      <Route path="*" element={<Navigate to="/account" replace />} />
    </Routes>
  </>;
}
