// Browser capability checks and key conversions for Web Push. Pure
// functions: callers pass what they read from navigator and window.

export interface PushEnvironment {
  hasServiceWorker: boolean;
  hasPushManager: boolean;
  hasNotification: boolean;
  isIOS: boolean;
  // standalone is true when the page runs as an app added to the home screen.
  standalone: boolean;
  permission: 'default' | 'granted' | 'denied' | 'unsupported';
}

// PushSupport is what the settings screen can offer:
// supported: the button works; ios-home-screen: iOS supports Web Push only
// for apps added to the home screen; denied: the user must allow
// notifications in the browser settings; unsupported: no Web Push.
export type PushSupport = 'supported' | 'ios-home-screen' | 'denied' | 'unsupported';

export function pushSupport(env: PushEnvironment): PushSupport {
  if (!env.hasServiceWorker || !env.hasPushManager || !env.hasNotification) {
    return env.isIOS && !env.standalone ? 'ios-home-screen' : 'unsupported';
  }
  if (env.permission === 'denied') return 'denied';
  return 'supported';
}

// isIOS detects iPhone, iPod and iPad, including iPads that report a Mac
// user agent (a Mac has no touch points).
export function isIOS(userAgent: string, platform: string, maxTouchPoints: number): boolean {
  return /iPhone|iPad|iPod/.test(userAgent) || (platform === 'MacIntel' && maxTouchPoints > 1);
}

// deviceLabel names a device from its user agent, such as "iPhone Safari".
export function deviceLabel(userAgent: string): string {
  const os = /iPhone/.test(userAgent) ? 'iPhone'
    : /iPad/.test(userAgent) ? 'iPad'
      : /Android/.test(userAgent) ? 'Android'
        : /Windows/.test(userAgent) ? 'Windows'
          : /Mac OS X|Macintosh/.test(userAgent) ? 'Mac'
            : /Linux/.test(userAgent) ? 'Linux' : '';
  const browser = /Edg\//.test(userAgent) ? 'Edge'
    : /Firefox\/|FxiOS\//.test(userAgent) ? 'Firefox'
      : /Chrome\/|CriOS\//.test(userAgent) ? 'Chrome'
        : /Safari\//.test(userAgent) ? 'Safari' : '';
  return [os, browser].filter(Boolean).join(' ') || 'ブラウザ';
}

// base64URLToBytes decodes unpadded or padded base64url.
export function base64URLToBytes(value: string): Uint8Array<ArrayBuffer> {
  const base64 = value.replace(/-/g, '+').replace(/_/g, '/').replace(/=+$/, '');
  const binary = atob(base64 + '='.repeat((4 - (base64.length % 4)) % 4));
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i += 1) bytes[i] = binary.charCodeAt(i);
  return bytes;
}

// bytesToBase64URL encodes unpadded base64url.
export function bytesToBase64URL(buffer: ArrayBuffer): string {
  let binary = '';
  for (const byte of new Uint8Array(buffer)) binary += String.fromCharCode(byte);
  return btoa(binary).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

// sameKey reports whether a subscription's applicationServerKey is key.
export function sameKey(current: ArrayBuffer | null, key: Uint8Array): boolean {
  if (!current || current.byteLength !== key.length) return false;
  const bytes = new Uint8Array(current);
  return bytes.every((b, i) => b === key[i]);
}
