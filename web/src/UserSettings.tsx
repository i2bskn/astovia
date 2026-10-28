import { useCallback, useEffect, useId, useRef, useState } from 'react';
import type { FormEvent } from 'react';
import { auth, request } from './auth';
import { dateTime } from './format';
import Phrased from './Phrased';
import { createUsersClient, isForbidden, roleLabels, userActions, usersErrorMessage } from './users';
import type { Invitation, ManagedUser, Role, UserChange } from './users';

const client = createUsersClient(request);

function formatDate(value: string): string {
  return dateTime(value, new Date());
}

// forbidden refreshes the signed-in user after an owner request was refused,
// so that a role changed elsewhere hides this page.
function forbidden(error: unknown): void {
  if (isForbidden(error)) void auth.reloadProfile();
}

// LinkPanel shows a link the server returns only once. It lives only in the
// state of this page and is gone after a reload or navigation. When the
// clipboard is unavailable the link stays selectable for copying by hand.
function LinkPanel({ title, url, note, onClose }: { title: string; url: string; note: string; onClose: () => void }) {
  const [copied, setCopied] = useState(false);
  const [copyError, setCopyError] = useState(false);
  const timer = useRef<number | undefined>(undefined);

  useEffect(() => () => window.clearTimeout(timer.current), []);

  async function copy(): Promise<void> {
    try {
      await navigator.clipboard.writeText(url);
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
    <p className="token-created-lead">{title}</p>
    <p className="token-created-label"><Phrased text={note} /></p>
    <code className="token-secret">{url}</code>
    <div className="token-created-actions">
      <button className="button secondary small" type="button" onClick={() => { void copy(); }}>{copied ? 'コピーしました' : 'コピー'}</button>
      <button className="button secondary small" type="button" onClick={onClose}>閉じる</button>
    </div>
    {copyError ? <p className="notice error" role="alert">コピーできませんでした。リンクを選択して、手動でコピーしてください。</p> : null}
  </div>;
}

function InviteSection({ onCreated }: { onCreated: () => void }) {
  const [email, setEmail] = useState('');
  const [role, setRole] = useState<Role>('member');
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [created, setCreated] = useState<{ email: string; url: string } | null>(null);
  const headingID = useId();
  const emailID = useId();
  const roleID = useId();
  const errorID = useId();

  async function submit(event: FormEvent<HTMLFormElement>): Promise<void> {
    event.preventDefault();
    if (submitting) return;
    if (!email.trim()) {
      setError('メールアドレスを入力してください。');
      return;
    }
    setError(null);
    setSubmitting(true);
    try {
      const invitation = await client.createInvitation(email, role);
      setCreated({ email: invitation.email, url: invitation.url });
      setEmail('');
      onCreated();
    } catch (failure) {
      setError(usersErrorMessage(failure));
      forbidden(failure);
    } finally {
      setSubmitting(false);
    }
  }

  return <section className="action-panel settings-panel" id="invite" aria-labelledby={headingID}>
    <h2 id={headingID}>招待</h2>
    <p className="intro"><Phrased text="招待するメールアドレスと権限を決めて、招待リンクを作ります。メールは送らないので、リンクをコピーして本人に渡してください。リンクを開いた人がパスワードを決めると、アカウントができます。" /></p>
    <form className="action-form" onSubmit={(event) => { void submit(event); }} aria-busy={submitting} noValidate>
      <div className="field">
        <label htmlFor={emailID}>メールアドレス</label>
        <input id={emailID} name="email" type="email" autoComplete="off" autoCapitalize="none" spellCheck={false} maxLength={254} value={email} onChange={(e) => { setEmail(e.target.value); setError(null); }} disabled={submitting} aria-describedby={error ? errorID : undefined} />
      </div>
      <div className="field">
        <label htmlFor={roleID}>権限</label>
        <select id={roleID} name="role" value={role} onChange={(e) => setRole(e.target.value === 'owner' ? 'owner' : 'member')} disabled={submitting}>
          <option value="member">メンバー</option>
          <option value="owner">オーナー（ユーザー管理もできる）</option>
        </select>
      </div>
      {error ? <p id={errorID} className="notice error" role="alert">{error}</p> : null}
      <button className="button primary add-button" type="submit" disabled={submitting}>{submitting ? '作成中…' : '招待リンクを作成'}</button>
    </form>
    {created ? <LinkPanel title={`${created.email} の招待リンク`} url={created.url} note="このリンクは 7 日間有効で、表示はこの 1 回だけです。" onClose={() => setCreated(null)} /> : null}
  </section>;
}

function InvitationRow({ invitation, confirming, busy, onAsk, onCancel, onRevoke }: {
  invitation: Invitation;
  confirming: boolean;
  busy: boolean;
  onAsk: () => void;
  onCancel: () => void;
  onRevoke: () => void;
}) {
  return <li className="action-item">
    <div className="action-body">
      <p className="action-content">{invitation.email}</p>
      <dl className="action-meta">
        <div><dt>権限</dt><dd>{roleLabels[invitation.role]}</dd></div>
        <div><dt>期限</dt><dd>{formatDate(invitation.expires_at)}</dd></div>
        <div><dt>招待した人</dt><dd>{invitation.invited_by_email || '不明'}</dd></div>
      </dl>
    </div>
    <div className="action-buttons">
      {confirming ? <>
        <button className="button secondary small danger" type="button" disabled={busy} onClick={onRevoke}>本当に取り消す</button>
        <button className="button secondary small" type="button" disabled={busy} onClick={onCancel}>やめる</button>
      </> : <button className="button secondary small" type="button" disabled={busy} onClick={onAsk}>取り消す</button>}
    </div>
  </li>;
}

// Confirm names the operation of a row waiting for its second press.
type Confirm = 'disable' | 'demote-self' | 'reset';

function UserRow({ user, actions, confirming, busy, onAsk, onCancel, onChange, onReset }: {
  user: ManagedUser;
  actions: ReturnType<typeof userActions>;
  confirming: Confirm | null;
  busy: boolean;
  onAsk: (confirm: Confirm) => void;
  onCancel: () => void;
  onChange: (change: UserChange) => void;
  onReset: () => void;
}) {
  let buttons: React.ReactNode;
  if (confirming === 'disable') {
    buttons = <>
      <button className="button secondary small danger" type="button" disabled={busy} onClick={() => onChange({ disabled: true })}>本当に無効にする</button>
      <button className="button secondary small" type="button" disabled={busy} onClick={onCancel}>やめる</button>
    </>;
  } else if (confirming === 'demote-self') {
    buttons = <>
      <button className="button secondary small danger" type="button" disabled={busy} onClick={() => onChange({ role: 'member' })}>本当にメンバーになる</button>
      <button className="button secondary small" type="button" disabled={busy} onClick={onCancel}>やめる</button>
    </>;
  } else if (confirming === 'reset') {
    buttons = <>
      <button className="button secondary small" type="button" disabled={busy} onClick={onReset}>発行する</button>
      <button className="button secondary small" type="button" disabled={busy} onClick={onCancel}>やめる</button>
    </>;
  } else {
    buttons = <>
      {actions.promote ? <button className="button secondary small" type="button" disabled={busy} onClick={() => onChange({ role: 'owner' })}>オーナーにする</button> : null}
      {actions.demote ? <button className="button secondary small" type="button" disabled={busy} onClick={() => (actions.self ? onAsk('demote-self') : onChange({ role: 'member' }))}>メンバーにする</button> : null}
      {actions.reset ? <button className="button secondary small" type="button" disabled={busy} onClick={() => onAsk('reset')}>パスワード再設定リンク</button> : null}
      {actions.disable ? <button className="button secondary small danger" type="button" disabled={busy} onClick={() => onAsk('disable')}>無効にする</button> : null}
      {actions.enable ? <button className="button secondary small" type="button" disabled={busy} onClick={() => onChange({ disabled: false })}>再開する</button> : null}
    </>;
  }
  return <li className="action-item">
    <div className="action-body">
      <p className="action-content">{user.email}{actions.self ? <> <span className="chip">あなた</span></> : null}</p>
      <dl className="action-meta">
        <div><dt>権限</dt><dd>{roleLabels[user.role]}</dd></div>
        <div><dt>状態</dt><dd className={user.disabled ? 'status-error' : undefined}>{user.disabled ? '無効' : '有効'}</dd></div>
        <div><dt>作成</dt><dd>{formatDate(user.created_at)}</dd></div>
      </dl>
      {confirming === 'disable' ? <p className="action-note"><Phrased text="無効にすると、すぐにログアウトされ、API トークン、AI クライアントの接続、GitHub と Slack の連携、AI の設定、通知を受け取る端末の登録を削除します。タスクと通知の設定は残ります。再開しても、削除したものは戻りません。" /></p> : null}
      {confirming === 'demote-self' ? <p className="action-note"><Phrased text="メンバーになると、このユーザー管理の画面はすぐに使えなくなります。" /></p> : null}
      {confirming === 'reset' ? <p className="action-note"><Phrased text="リンクを開くとパスワードを決め直して、このユーザーとしてログインできます。以前に発行したこのユーザーのリンクは使えなくなります。" /></p> : null}
    </div>
    <div className="action-buttons">{buttons}</div>
  </li>;
}

function UsersSection({ userID, users, loading, loadError, onChanged }: {
  userID: string;
  users: ManagedUser[];
  loading: boolean;
  loadError: string | null;
  onChanged: (user: ManagedUser) => void;
}) {
  const [confirming, setConfirming] = useState<{ id: string; confirm: Confirm } | null>(null);
  const [busyID, setBusyID] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [created, setCreated] = useState<{ email: string; url: string } | null>(null);
  const headingID = useId();

  async function change(user: ManagedUser, next: UserChange): Promise<void> {
    if (busyID) return;
    setBusyID(user.id);
    setError(null);
    try {
      const updated = await client.updateUser(user.id, next);
      setConfirming(null);
      onChanged(updated);
      // Demoting oneself ends the owner role; read it again so that this
      // page goes away.
      if (updated.id === userID && updated.role !== 'owner') void auth.reloadProfile();
    } catch (failure) {
      setError(usersErrorMessage(failure));
      forbidden(failure);
    } finally {
      setBusyID(null);
    }
  }

  async function reset(user: ManagedUser): Promise<void> {
    if (busyID) return;
    setBusyID(user.id);
    setError(null);
    try {
      const link = await client.createPasswordReset(user.id);
      setConfirming(null);
      setCreated({ email: user.email, url: link.url });
    } catch (failure) {
      setError(usersErrorMessage(failure));
      forbidden(failure);
    } finally {
      setBusyID(null);
    }
  }

  return <section className="action-panel settings-panel" id="users" aria-labelledby={headingID}>
    <h2 id={headingID}>ユーザー</h2>
    <p className="intro"><Phrased text="権限の変更、ログインの停止（無効化）と再開、パスワード再設定リンクの発行ができます。タスクや連携はユーザーごとに別で、オーナーも通常の画面では他のユーザーのタスクを見られません。ただし、パスワード再設定リンクを自分で開けば、そのユーザーとしてログインできます。リンクでパスワードを再設定したことは、本人の画面に 14 日間表示されます。" /></p>
    {created ? <LinkPanel title={`${created.email} のパスワード再設定リンク`} url={created.url} note="このリンクは 24 時間有効で、表示はこの 1 回だけです。本人以外に渡さないでください。" onClose={() => setCreated(null)} /> : null}
    {error ? <p className="notice error" role="alert">{error}</p> : null}
    {loadError ? <p className="notice error" role="alert">{loadError}</p> : null}
    {users.length > 0 ? <ul className="action-list">
      {users.map((user) => <UserRow key={user.id} user={user}
        actions={userActions(user, users, userID)}
        confirming={confirming?.id === user.id ? confirming.confirm : null}
        busy={busyID === user.id}
        onAsk={(confirm) => { setConfirming({ id: user.id, confirm }); setError(null); }}
        onCancel={() => setConfirming(null)}
        onChange={(next) => { void change(user, next); }}
        onReset={() => { void reset(user); }} />)}
    </ul> : null}
    {loading ? <p className="action-loading" aria-live="polite">読み込んでいます…</p> : null}
  </section>;
}

// UserSettings is the user management page of Settings, for owners.
export default function UserSettings({ userID }: { userID: string }) {
  const [users, setUsers] = useState<ManagedUser[]>([]);
  const [invitations, setInvitations] = useState<Invitation[]>([]);
  const [loading, setLoading] = useState(true);
  const [usersError, setUsersError] = useState<string | null>(null);
  const [invitationsError, setInvitationsError] = useState<string | null>(null);
  const [confirmingID, setConfirmingID] = useState<string | null>(null);
  const [revokingID, setRevokingID] = useState<string | null>(null);
  const invitationsHeadingID = useId();

  const loadInvitations = useCallback(async () => {
    try {
      setInvitations(await client.listInvitations());
      setInvitationsError(null);
    } catch (failure) {
      setInvitationsError(isForbidden(failure) ? usersErrorMessage(failure) : '招待の一覧を読み込めませんでした。接続を確認して、もう一度お試しください。');
      forbidden(failure);
    }
  }, []);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      setUsers(await client.listUsers());
      setUsersError(null);
    } catch (failure) {
      setUsersError(isForbidden(failure) ? usersErrorMessage(failure) : 'ユーザーの一覧を読み込めませんでした。接続を確認して、もう一度お試しください。');
      forbidden(failure);
    } finally {
      setLoading(false);
    }
    await loadInvitations();
  }, [loadInvitations]);

  useEffect(() => { void load(); }, [load]);

  async function revoke(id: string): Promise<void> {
    if (revokingID) return;
    setRevokingID(id);
    setInvitationsError(null);
    try {
      await client.revokeInvitation(id);
      setInvitations((previous) => previous.filter((i) => i.id !== id));
      setConfirmingID(null);
    } catch (failure) {
      setInvitationsError(usersErrorMessage(failure));
      forbidden(failure);
    } finally {
      setRevokingID(null);
    }
  }

  return <div className="settings-stack">
    <InviteSection onCreated={() => { void loadInvitations(); }} />
    <section className="action-panel settings-panel" id="invitations" aria-labelledby={invitationsHeadingID}>
      <h2 id={invitationsHeadingID}>有効な招待</h2>
      {invitationsError ? <p className="notice error" role="alert">{invitationsError}</p> : null}
      {invitations.length > 0 ? <ul className="action-list">
        {invitations.map((invitation) => <InvitationRow key={invitation.id} invitation={invitation}
          confirming={confirmingID === invitation.id}
          busy={revokingID === invitation.id}
          onAsk={() => setConfirmingID(invitation.id)}
          onCancel={() => setConfirmingID(null)}
          onRevoke={() => { void revoke(invitation.id); }} />)}
      </ul> : null}
      {!loading && !invitationsError && invitations.length === 0 ? <p className="action-empty">有効な招待はありません。</p> : null}
    </section>
    <UsersSection userID={userID} users={users} loading={loading} loadError={usersError}
      onChanged={(updated) => {
        setUsers((previous) => previous.map((u) => (u.id === updated.id ? updated : u)));
        // Demoting or disabling a user revokes the invitations it issued.
        void loadInvitations();
      }} />
  </div>;
}
