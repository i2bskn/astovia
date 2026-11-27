// Two-step verification: the responses of its endpoints, the client of the
// account screen and the texts of its errors. The request function is
// injected (normally auth.ts's request) so that this module holds no
// authentication state and can be tested with a fake. The requests before
// the first session go through auth.ts, which holds the MFA token.
import qrcode from 'qrcode-generator';
import type { RequestFunction } from './actionItems';

export const maxCredentialNameLength = 64;
export const maxCredentials = 10;

// TOTPSetup starts the enrollment of an authenticator app: the secret for
// typing, its otpauth URL for the QR code and the token that carries the
// secret back to the finish request.
export interface TOTPSetup {
  secret: string;
  otpauth_url: string;
  setup_token: string;
}

// WebAuthnBegin starts a WebAuthn ceremony: the options in the JSON form of
// the specification. The server keeps the ceremony behind a short-lived
// HttpOnly cookie, which the browser sends back with the finish request.
export interface WebAuthnBegin {
  options: Record<string, unknown>;
}

export interface MFACredential {
  id: string;
  name: string;
  created_at: string;
  last_used_at: string | null;
}

// MFAStatus is the two-step verification of the signed-in user.
export interface MFAStatus {
  required: boolean;
  totp: { created_at: string } | null;
  webauthn: MFACredential[];
  recovery_codes_remaining: number;
  webauthn_available: boolean;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

export function readTOTPSetup(data: unknown): TOTPSetup {
  if (!isRecord(data) || typeof data.secret !== 'string' || !/^[A-Z2-7]{16,}$/.test(data.secret) ||
    typeof data.otpauth_url !== 'string' || !data.otpauth_url.startsWith('otpauth://totp/') ||
    typeof data.setup_token !== 'string' || !data.setup_token) throw new Error('Unexpected TOTP setup response');
  return { secret: data.secret, otpauth_url: data.otpauth_url, setup_token: data.setup_token };
}

export function readWebAuthnBegin(data: unknown): WebAuthnBegin {
  if (!isRecord(data) || !isRecord(data.options) || typeof data.options.challenge !== 'string' || !data.options.challenge) {
    throw new Error('Unexpected WebAuthn response');
  }
  return { options: data.options };
}

function isCodes(value: unknown): value is string[] {
  return Array.isArray(value) && value.length > 0 && value.every((code) => typeof code === 'string' && code !== '');
}

// readRecoveryCodes checks new recovery codes. optional allows null: only
// the first factor of a user issues codes.
export function readRecoveryCodes(data: unknown, optional: boolean): string[] | null {
  if (!isRecord(data) || !('recovery_codes' in data)) throw new Error('Unexpected recovery codes response');
  if (data.recovery_codes === null && optional) return null;
  if (!isCodes(data.recovery_codes)) throw new Error('Unexpected recovery codes response');
  return data.recovery_codes;
}

function isCredential(value: unknown): value is MFACredential {
  return isRecord(value) && typeof value.id === 'string' && value.id !== '' && typeof value.name === 'string' &&
    typeof value.created_at === 'string' && (value.last_used_at === null || typeof value.last_used_at === 'string');
}

export function readMFAStatus(data: unknown): MFAStatus {
  if (!isRecord(data) || typeof data.required !== 'boolean' || typeof data.webauthn_available !== 'boolean' ||
    typeof data.recovery_codes_remaining !== 'number' || !Number.isInteger(data.recovery_codes_remaining) || data.recovery_codes_remaining < 0 ||
    !Array.isArray(data.webauthn) || !data.webauthn.every(isCredential) ||
    !(data.totp === null || (isRecord(data.totp) && typeof data.totp.created_at === 'string'))) {
    throw new Error('Unexpected MFA status response');
  }
  return {
    required: data.required,
    totp: data.totp === null ? null : { created_at: (data.totp as { created_at: string }).created_at },
    webauthn: data.webauthn,
    recovery_codes_remaining: data.recovery_codes_remaining,
    webauthn_available: data.webauthn_available,
  };
}

// mfaEnabled reports whether the user has a second factor. Recovery codes
// are not one.
export function mfaEnabled(status: MFAStatus): boolean {
  return status.totp !== null || status.webauthn.length > 0;
}

// credentialName returns a name for a passkey or security key as the server
// accepts it (1-64 characters, trimmed), or null.
export function credentialName(value: string): string | null {
  const name = value.trim();
  const length = [...name].length;
  return length >= 1 && length <= maxCredentialNameLength && !/[\u0000-\u001f\u007f-\u009f]/.test(name) ? name : null;
}

const json = { 'Content-Type': 'application/json' };

// createMFAClient is the client of the account screen. Starting an
// enrollment, removing a factor and reissuing the recovery codes take the
// current password; the finish requests stand on the begin request instead
// (the setup token, or the ceremony cookie of WebAuthn).
export function createMFAClient(request: RequestFunction) {
  const post = (path: `/api/v1/${string}`, body: object) => request<unknown>(path, { method: 'POST', headers: json, body: JSON.stringify(body) });
  return {
    async status(): Promise<MFAStatus> {
      return readMFAStatus(await request<unknown>('/api/v1/mfa'));
    },
    async beginTOTP(password: string): Promise<TOTPSetup> {
      return readTOTPSetup(await post('/api/v1/mfa/totp/begin', { password }));
    },
    // finishTOTP returns the recovery codes when the authenticator app is
    // the first factor, and null otherwise.
    async finishTOTP(setupToken: string, code: string): Promise<string[] | null> {
      return readRecoveryCodes(await post('/api/v1/mfa/totp/finish', { setup_token: setupToken, code }), true);
    },
    async deleteTOTP(password: string): Promise<void> {
      await post('/api/v1/mfa/totp/delete', { password });
    },
    async beginWebAuthn(password: string): Promise<WebAuthnBegin> {
      return readWebAuthnBegin(await post('/api/v1/mfa/webauthn/begin', { password }));
    },
    async finishWebAuthn(credential: object, name: string): Promise<{ credential: MFACredential; recoveryCodes: string[] | null }> {
      const data = await post('/api/v1/mfa/webauthn/finish', { credential, name });
      if (!isRecord(data) || !isCredential(data.credential)) throw new Error('Unexpected WebAuthn response');
      return { credential: data.credential, recoveryCodes: readRecoveryCodes(data, true) };
    },
    async deleteWebAuthn(id: string, password: string): Promise<void> {
      await post(`/api/v1/mfa/webauthn/${encodeURIComponent(id)}/delete`, { password });
    },
    async reissueRecoveryCodes(password: string): Promise<string[]> {
      const codes = readRecoveryCodes(await post('/api/v1/mfa/recovery-codes', { password }), false);
      if (!codes) throw new Error('Unexpected recovery codes response');
      return codes;
    },
  };
}

export type MFAClient = ReturnType<typeof createMFAClient>;

// MFAStepKind is what a failed request was checking: a code (of the
// authenticator app or a recovery code), a passkey or security key, or the
// current password.
export type MFAStepKind = 'code' | 'key' | 'password';

// mfaErrorMessage describes a failed request of two-step verification.
// auth.ts's APIError is matched by shape so that this module does not import
// auth.ts at runtime. The attempts at a password are limited for 5 minutes,
// those at a code for longer.
export function mfaErrorMessage(error: unknown, step: MFAStepKind = 'code'): string {
  const status = error instanceof Error && 'status' in error && typeof error.status === 'number' ? error.status : null;
  const code = error instanceof Error && 'code' in error && typeof error.code === 'string' ? error.code : null;
  switch (code) {
    case 'mfa_code_invalid': return step === 'key' ? 'パスキー・セキュリティキーを確認できませんでした。もう一度お試しください。' : 'コードが正しくありません。';
    case 'password_invalid': return 'パスワードが正しくありません。';
    case 'mfa_required': return 'この Astovia では二段階認証が必要なため、最後の方法は削除できません。';
    case 'mfa_state_invalid': return '時間が切れました。最初からやり直してください。';
    case 'mfa_limit': return `登録できるのは ${maxCredentials} 個までです。`;
    case 'mfa_credential_exists': return 'このパスキーは登録済みです。';
    case 'mfa_token_invalid': return '時間が切れました。もう一度ログインしてください。';
    case 'mfa_already_enrolled': return '二段階認証は登録済みです。ログインをやり直してください。';
    case 'mfa_not_required': return '二段階認証の登録は、ログイン後の「二段階認証」の画面で行ってください。';
    case 'mfa_not_enrolled': return '二段階認証が有効ではないため、リカバリーコードを発行できません。';
    case 'webauthn_unavailable': return 'この Astovia のアドレスでは、パスキー・セキュリティキーを使えません。';
    case 'password_auth_disabled': return 'この Astovia はシングルサインオンでログインするため、二段階認証は使えません。';
    default: break;
  }
  if (status === 429) {
    return step === 'password'
      ? '試行回数が上限に達しました。5分ほどしてから、もう一度お試しください。'
      : '試行回数が上限に達しました。しばらくしてから、もう一度お試しください。';
  }
  if (status === 404) return '対象が見つかりませんでした。画面を読み込み直してください。';
  if (status === 400) return '入力内容を確認してください。';
  return '通信に失敗しました。接続を確認して、もう一度お試しください。';
}

// qrModules returns the modules of the QR code of text, row by row (true is
// dark), or null when text does not fit a QR code.
export function qrModules(text: string): boolean[][] | null {
  try {
    const code = qrcode(0, 'M');
    code.addData(text);
    code.make();
    const size = code.getModuleCount();
    return Array.from({ length: size }, (_row, y) => Array.from({ length: size }, (_cell, x) => code.isDark(y, x)));
  } catch {
    return null;
  }
}

// qrQuietZone is the margin around a QR code, in modules, that readers need.
export const qrQuietZone = 4;

// qrPath draws the dark modules as the path of an SVG whose viewBox is
// qrSize(modules) wide and high: one rectangle per horizontal run, moved in
// by the quiet zone.
export function qrPath(modules: readonly (readonly boolean[])[]): string {
  let path = '';
  modules.forEach((row, y) => {
    let start = -1;
    for (let x = 0; x <= row.length; x += 1) {
      const dark = x < row.length && row[x] === true;
      if (dark && start < 0) start = x;
      if (!dark && start >= 0) {
        path += `M${start + qrQuietZone} ${y + qrQuietZone}h${x - start}v1h-${x - start}z`;
        start = -1;
      }
    }
  });
  return path;
}

export function qrSize(modules: readonly (readonly boolean[])[]): number {
  return modules.length + 2 * qrQuietZone;
}
