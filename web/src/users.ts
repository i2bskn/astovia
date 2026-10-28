// User management (owners only). The request function is injected
// (normally auth.ts's request) so that this module holds no authentication
// state and can be tested with a fake.
import type { RequestFunction } from './actionItems';

export type Role = 'owner' | 'member';

export const roleLabels: Record<Role, string> = { owner: 'オーナー', member: 'メンバー' };

export interface ManagedUser {
  id: string;
  email: string;
  role: Role;
  disabled: boolean;
  created_at: string;
}

export interface Invitation {
  id: string;
  email: string;
  role: Role;
  created_at: string;
  expires_at: string;
  invited_by_email: string;
}

// CreatedInvitation carries the link, which the server returns only once.
export interface CreatedInvitation {
  id: string;
  email: string;
  role: Role;
  created_at: string;
  expires_at: string;
  url: string;
}

// CreatedReset is a password reset link, returned only once.
export interface CreatedReset {
  url: string;
  expires_at: string;
}

// UserChange is one change of a user: its role or whether it is disabled.
export type UserChange = { role: Role } | { disabled: boolean };

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function isRole(value: unknown): value is Role {
  return value === 'owner' || value === 'member';
}

function isManagedUser(value: unknown): value is ManagedUser {
  return isRecord(value) && typeof value.id === 'string' && value.id !== '' && typeof value.email === 'string' &&
    isRole(value.role) && typeof value.disabled === 'boolean' && typeof value.created_at === 'string';
}

function isInvitation(value: unknown): value is Invitation {
  return isRecord(value) && typeof value.id === 'string' && value.id !== '' && typeof value.email === 'string' && isRole(value.role) &&
    typeof value.created_at === 'string' && typeof value.expires_at === 'string' && typeof value.invited_by_email === 'string';
}

// isLink checks that a link points at this origin's screen, so that the
// screen never offers to copy anything else.
function isLink(value: unknown, path: string, prefix: string): value is string {
  return typeof value === 'string' && value.startsWith(`${location.origin}${path}#${prefix}`);
}

function readUsers(data: unknown): ManagedUser[] {
  if (!isRecord(data) || !Array.isArray(data.users) || !data.users.every(isManagedUser)) throw new Error('Unexpected user list response');
  return data.users;
}

function readUser(data: unknown): ManagedUser {
  if (!isManagedUser(data)) throw new Error('Unexpected user response');
  return data;
}

function readInvitations(data: unknown): Invitation[] {
  if (!isRecord(data) || !Array.isArray(data.invitations) || !data.invitations.every(isInvitation)) throw new Error('Unexpected invitation list response');
  return data.invitations;
}

function readCreatedInvitation(data: unknown): CreatedInvitation {
  if (!isRecord(data) || typeof data.id !== 'string' || typeof data.email !== 'string' || !isRole(data.role) ||
    typeof data.created_at !== 'string' || typeof data.expires_at !== 'string' || !isLink(data.url, '/invite', 'inv_')) {
    throw new Error('Unexpected invitation response');
  }
  return data as unknown as CreatedInvitation;
}

function readCreatedReset(data: unknown): CreatedReset {
  if (!isRecord(data) || typeof data.expires_at !== 'string' || !isLink(data.url, '/reset-password', 'pwr_')) {
    throw new Error('Unexpected password reset response');
  }
  return data as unknown as CreatedReset;
}

const json = { 'Content-Type': 'application/json' };

export function createUsersClient(request: RequestFunction) {
  return {
    async listUsers(): Promise<ManagedUser[]> {
      return readUsers(await request<unknown>('/api/v1/users'));
    },
    async updateUser(id: string, change: UserChange): Promise<ManagedUser> {
      return readUser(await request<unknown>(`/api/v1/users/${encodeURIComponent(id)}`, { method: 'PATCH', headers: json, body: JSON.stringify(change) }));
    },
    async createPasswordReset(id: string): Promise<CreatedReset> {
      return readCreatedReset(await request<unknown>(`/api/v1/users/${encodeURIComponent(id)}/password-resets`, { method: 'POST' }));
    },
    async listInvitations(): Promise<Invitation[]> {
      return readInvitations(await request<unknown>('/api/v1/invitations'));
    },
    async createInvitation(email: string, role: Role): Promise<CreatedInvitation> {
      return readCreatedInvitation(await request<unknown>('/api/v1/invitations', { method: 'POST', headers: json, body: JSON.stringify({ email, role }) }));
    },
    async revokeInvitation(id: string): Promise<void> {
      await request<unknown>(`/api/v1/invitations/${encodeURIComponent(id)}`, { method: 'DELETE' });
    },
  };
}

export type UsersClient = ReturnType<typeof createUsersClient>;

// UserActions are the operations offered on a user's row. The server
// enforces the same rules; the screen only avoids offering what it refuses.
export interface UserActions {
  self: boolean;
  promote: boolean;
  demote: boolean;
  disable: boolean;
  enable: boolean;
  reset: boolean;
}

// userActions decides the operations of user's row for the signed-in owner
// meID: nobody disables itself, and the last enabled owner is neither
// demoted nor disabled.
export function userActions(user: ManagedUser, users: readonly ManagedUser[], meID: string): UserActions {
  const owners = users.filter((u) => u.role === 'owner' && !u.disabled).length;
  const lastOwner = user.role === 'owner' && !user.disabled && owners <= 1;
  const self = user.id === meID;
  return {
    self,
    promote: user.role === 'member',
    demote: user.role === 'owner' && !lastOwner,
    disable: !user.disabled && !self && !lastOwner,
    enable: user.disabled,
    reset: !user.disabled,
  };
}

function failure(error: unknown): { status: number | null; code: string | null } {
  if (!(error instanceof Error)) return { status: null, code: null };
  const status = 'status' in error && typeof error.status === 'number' ? error.status : null;
  const code = 'code' in error && typeof error.code === 'string' ? error.code : null;
  return { status, code };
}

// isForbidden reports whether an owner request was refused because the
// signed-in user is no longer an owner.
export function isForbidden(error: unknown): boolean {
  return failure(error).status === 403;
}

// usersErrorMessage describes a failed request of the user management
// screen. auth.ts's APIError is matched by shape so that this module does
// not import auth.ts at runtime.
export function usersErrorMessage(error: unknown): string {
  const { status, code } = failure(error);
  switch (code) {
    case 'last_owner': return '有効なオーナーがいなくなるため、この操作はできません。先に別のユーザーをオーナーにしてください。';
    case 'self': return '自分自身を無効にすることはできません。';
    case 'email_taken': return 'このメールアドレスのユーザーは既にいます。';
    case 'user_disabled': return '無効になっているユーザーには、パスワード再設定リンクを発行できません。先に再開してください。';
    case 'forbidden': return 'ユーザー管理はオーナーだけが使えます。別の画面で権限が変わった可能性があります。';
    default: break;
  }
  if (status === 403) return 'ユーザー管理はオーナーだけが使えます。別の画面で権限が変わった可能性があります。';
  if (status === 404) return '対象が見つかりませんでした。画面を読み込み直してください。';
  if (status === 400) return '入力内容を確認してください。メールアドレスの形式が正しいか確かめてください。';
  return '通信に失敗しました。接続を確認して、もう一度お試しください。';
}
