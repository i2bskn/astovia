// WebAuthn (passkeys and security keys) in the browser. The server sends
// the argument of navigator.credentials, {publicKey: options}, in the JSON
// form of the specification, with binary members as base64url without
// padding; navigator.credentials takes and returns ArrayBuffers.
// The conversions are written here, so that they can be tested without a
// browser and do not depend on PublicKeyCredential.parseCreationOptionsFromJSON
// and toJSON, which older browsers lack.

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

// base64urlToBuffer decodes base64url, with or without padding.
export function base64urlToBuffer(value: string): ArrayBuffer {
  const text = value.replace(/=+$/, '');
  if (!/^[A-Za-z0-9_-]*$/.test(text) || text.length % 4 === 1) throw new Error('Invalid base64url value');
  const binary = atob(text.replace(/-/g, '+').replace(/_/g, '/'));
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i += 1) bytes[i] = binary.charCodeAt(i);
  return bytes.buffer;
}

type Binary = ArrayBuffer | ArrayBufferView;

function isBinary(value: unknown): value is Binary {
  return value instanceof ArrayBuffer || ArrayBuffer.isView(value);
}

// bufferToBase64url encodes bytes as base64url without padding.
export function bufferToBase64url(value: Binary): string {
  const bytes = value instanceof ArrayBuffer ? new Uint8Array(value) : new Uint8Array(value.buffer, value.byteOffset, value.byteLength);
  let binary = '';
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

// descriptors converts the credential lists of the options
// (excludeCredentials, allowCredentials).
function descriptors(value: unknown): Record<string, unknown>[] {
  if (!Array.isArray(value)) throw new Error('Unexpected WebAuthn options');
  return value.map((item: unknown) => {
    if (!isRecord(item) || item.type !== 'public-key' || typeof item.id !== 'string') throw new Error('Unexpected WebAuthn options');
    return { ...item, id: base64urlToBuffer(item.id) };
  });
}

// creationOptions converts the options of a registration from the server
// into the argument of navigator.credentials.create: challenge, user.id and
// excludeCredentials[].id become ArrayBuffers, the rest is passed on.
export function creationOptions(value: unknown): PublicKeyCredentialCreationOptions {
  if (!isRecord(value) || typeof value.challenge !== 'string' || !value.challenge || !isRecord(value.rp) ||
    !isRecord(value.user) || typeof value.user.id !== 'string' || !Array.isArray(value.pubKeyCredParams)) {
    throw new Error('Unexpected WebAuthn options');
  }
  const options: Record<string, unknown> = {
    ...value,
    challenge: base64urlToBuffer(value.challenge),
    user: { ...value.user, id: base64urlToBuffer(value.user.id) },
  };
  if (value.excludeCredentials !== undefined) options.excludeCredentials = descriptors(value.excludeCredentials);
  return options as unknown as PublicKeyCredentialCreationOptions;
}

// requestOptions converts the options of a sign-in from the server into the
// argument of navigator.credentials.get: challenge and allowCredentials[].id
// become ArrayBuffers.
export function requestOptions(value: unknown): PublicKeyCredentialRequestOptions {
  if (!isRecord(value) || typeof value.challenge !== 'string' || !value.challenge) throw new Error('Unexpected WebAuthn options');
  const options: Record<string, unknown> = { ...value, challenge: base64urlToBuffer(value.challenge) };
  if (value.allowCredentials !== undefined) options.allowCredentials = descriptors(value.allowCredentials);
  return options as unknown as PublicKeyCredentialRequestOptions;
}

// credentialBase returns the members both responses share. id is derived
// from rawId; the server rejects a response in which the two differ. No
// extension is requested, so no extension result is sent: some of them hold
// binary values that JSON cannot carry.
function credentialBase(credential: unknown): { base: Record<string, unknown>; response: Record<string, unknown> } {
  if (typeof credential !== 'object' || credential === null) throw new Error('Unexpected WebAuthn credential');
  const { rawId, type, response, authenticatorAttachment } = credential as Record<string, unknown>;
  if (!isBinary(rawId) || type !== 'public-key' || typeof response !== 'object' || response === null) throw new Error('Unexpected WebAuthn credential');
  const id = bufferToBase64url(rawId);
  const base: Record<string, unknown> = { id, rawId: id, type, clientExtensionResults: {} };
  if (typeof authenticatorAttachment === 'string') base.authenticatorAttachment = authenticatorAttachment;
  return { base, response: response as Record<string, unknown> };
}

// registrationJSON converts the result of navigator.credentials.create into
// the JSON the server verifies.
export function registrationJSON(credential: unknown): Record<string, unknown> {
  const { base, response } = credentialBase(credential);
  const { clientDataJSON, attestationObject } = response;
  if (!isBinary(clientDataJSON) || !isBinary(attestationObject)) throw new Error('Unexpected WebAuthn credential');
  const out: Record<string, unknown> = { clientDataJSON: bufferToBase64url(clientDataJSON), attestationObject: bufferToBase64url(attestationObject) };
  // getTransports is a method of the response object; older browsers lack it.
  const getTransports = (response as { getTransports?: unknown }).getTransports;
  if (typeof getTransports === 'function') {
    const transports: unknown = getTransports.call(response);
    if (Array.isArray(transports)) out.transports = transports.filter((t) => typeof t === 'string');
  }
  return { ...base, response: out };
}

// assertionJSON converts the result of navigator.credentials.get into the
// JSON the server verifies.
export function assertionJSON(credential: unknown): Record<string, unknown> {
  const { base, response } = credentialBase(credential);
  const { clientDataJSON, authenticatorData, signature, userHandle } = response;
  if (!isBinary(clientDataJSON) || !isBinary(authenticatorData) || !isBinary(signature)) throw new Error('Unexpected WebAuthn credential');
  const out: Record<string, unknown> = {
    clientDataJSON: bufferToBase64url(clientDataJSON),
    authenticatorData: bufferToBase64url(authenticatorData),
    signature: bufferToBase64url(signature),
  };
  if (isBinary(userHandle) && userHandle.byteLength > 0) out.userHandle = bufferToBase64url(userHandle);
  return { ...base, response: out };
}

// webauthnSupported reports whether this browser can use passkeys and
// security keys.
export function webauthnSupported(): boolean {
  return typeof globalThis.PublicKeyCredential === 'function' && typeof navigator !== 'undefined' &&
    typeof navigator.credentials?.create === 'function' && typeof navigator.credentials.get === 'function';
}

// credentialArgument converts the options of the server, the argument of
// navigator.credentials in JSON form ({publicKey: options}), with convert
// for its publicKey member; the other members are passed on.
function credentialArgument<T>(value: unknown, convert: (options: unknown) => T): Record<string, unknown> & { publicKey: T } {
  if (!isRecord(value) || !('publicKey' in value)) throw new Error('Unexpected WebAuthn options');
  return { ...value, publicKey: convert(value.publicKey) };
}

// createCredential asks the browser to create a credential for the options
// of the server and returns the response to send back.
export async function createCredential(options: unknown): Promise<Record<string, unknown>> {
  return registrationJSON(await navigator.credentials.create(credentialArgument(options, creationOptions)));
}

// getCredential asks the browser to sign in with a credential the options
// of the server allow and returns the response to send back.
export async function getCredential(options: unknown): Promise<Record<string, unknown>> {
  return assertionJSON(await navigator.credentials.get(credentialArgument(options, requestOptions)));
}

// webauthnErrorMessage describes an error of navigator.credentials, or
// returns null for other errors. The browser reports a cancelled prompt and
// a timeout alike.
export function webauthnErrorMessage(error: unknown): string | null {
  if (typeof error !== 'object' || error === null || !('name' in error)) return null;
  switch (error.name) {
    case 'NotAllowedError':
    case 'AbortError': return '確認がキャンセルされたか、時間が切れました。もう一度お試しください。';
    case 'InvalidStateError': return 'このパスキーは登録済みです。';
    case 'SecurityError': return 'この Astovia のアドレスでは、パスキー・セキュリティキーを使えません。';
    case 'NotSupportedError': return 'この端末では、パスキー・セキュリティキーを使えません。';
    default: return null;
  }
}
