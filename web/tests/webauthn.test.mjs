import assert from 'node:assert/strict';
import { test } from 'node:test';
import { assertionJSON, base64urlToBuffer, bufferToBase64url, createCredential, creationOptions, getCredential, registrationJSON, requestOptions, webauthnErrorMessage, webauthnSupported } from '../.test-build/webauthn.js';

const bytes = (...values) => new Uint8Array(values).buffer;
const list = (buffer) => [...new Uint8Array(buffer)];

test('base64url converts to bytes and back without padding', () => {
  assert.deepEqual(list(base64urlToBuffer('')), []);
  assert.deepEqual(list(base64urlToBuffer('AQID')), [1, 2, 3]);
  assert.deepEqual(list(base64urlToBuffer('-_8')), [0xfb, 0xff]);
  // Padding is accepted, as are both lengths that need it.
  assert.deepEqual(list(base64urlToBuffer('-_8=')), [0xfb, 0xff]);
  assert.deepEqual(list(base64urlToBuffer('_w')), [0xff]);
  assert.ok(base64urlToBuffer('AQID') instanceof ArrayBuffer);
  assert.equal(bufferToBase64url(bytes()), '');
  assert.equal(bufferToBase64url(bytes(1, 2, 3)), 'AQID');
  assert.equal(bufferToBase64url(bytes(0xfb, 0xff)), '-_8');
  assert.equal(bufferToBase64url(bytes(0xff)), '_w');
  // A view converts its own bytes, not the buffer under it.
  assert.equal(bufferToBase64url(new Uint8Array([9, 1, 2, 3, 9]).subarray(1, 4)), 'AQID');
  const all = new Uint8Array(256).map((_, i) => i);
  assert.deepEqual(list(base64urlToBuffer(bufferToBase64url(all))), [...all]);
  assert.doesNotMatch(bufferToBase64url(all), /[+/=]/);
});

test('values that are not base64url are rejected', () => {
  for (const bad of ['AQI+', 'AQI/', 'A', 'AQIDA', 'AQ ID', 'あ', 'AQ=D']) assert.throws(() => base64urlToBuffer(bad), /base64url/, bad);
});

const creation = {
  rp: { name: 'Astovia', id: 'astovia.test' },
  user: { name: 'alice@example.com', displayName: 'alice@example.com', id: 'YWxpY2UtaWQ' },
  challenge: 'AQID',
  pubKeyCredParams: [{ type: 'public-key', alg: -7 }, { type: 'public-key', alg: -257 }],
  timeout: 300000,
  excludeCredentials: [{ type: 'public-key', id: '-_8', transports: ['usb'] }],
  authenticatorSelection: { requireResidentKey: false, residentKey: 'discouraged', userVerification: 'preferred' },
  attestation: 'none',
};

test('creation options get binary members and keep the rest', () => {
  const options = creationOptions(creation);
  assert.deepEqual(list(options.challenge), [1, 2, 3]);
  assert.equal(new TextDecoder().decode(options.user.id), 'alice-id');
  assert.equal(options.user.name, 'alice@example.com');
  assert.equal(options.user.displayName, 'alice@example.com');
  assert.deepEqual(options.rp, creation.rp);
  assert.deepEqual(options.pubKeyCredParams, creation.pubKeyCredParams);
  assert.equal(options.timeout, 300000);
  assert.equal(options.attestation, 'none');
  assert.deepEqual(options.authenticatorSelection, creation.authenticatorSelection);
  assert.equal(options.excludeCredentials.length, 1);
  assert.deepEqual(list(options.excludeCredentials[0].id), [0xfb, 0xff]);
  assert.deepEqual(options.excludeCredentials[0].transports, ['usb']);
  assert.equal(options.excludeCredentials[0].type, 'public-key');
  // The input is not changed.
  assert.equal(creation.challenge, 'AQID');
  assert.equal(creation.excludeCredentials[0].id, '-_8');
  // Without registered credentials the server sends no list.
  const { excludeCredentials: _excluded, ...first } = creation;
  assert.equal('excludeCredentials' in creationOptions(first), false);
});

test('malformed creation options are rejected', () => {
  const { challenge: _c, ...withoutChallenge } = creation;
  const { rp: _rp, ...withoutRP } = creation;
  for (const bad of [
    null, [], 'x', withoutChallenge, withoutRP,
    { ...creation, challenge: '' },
    { ...creation, challenge: 1 },
    { ...creation, challenge: 'not base64!' },
    { ...creation, user: { ...creation.user, id: 5 } },
    { ...creation, user: null },
    { ...creation, pubKeyCredParams: null },
    { ...creation, excludeCredentials: {} },
    { ...creation, excludeCredentials: [{ type: 'public-key' }] },
    { ...creation, excludeCredentials: [{ type: 'other', id: 'AQID' }] },
    { ...creation, excludeCredentials: [{ type: 'public-key', id: '+' }] },
  ]) assert.throws(() => creationOptions(bad), undefined, JSON.stringify(bad));
});

const assertion = { challenge: 'AQID', timeout: 300000, rpId: 'astovia.test', allowCredentials: [{ type: 'public-key', id: 'AQID', transports: ['usb'] }, { type: 'public-key', id: '-_8' }], userVerification: 'preferred' };

test('request options get binary members and keep the rest', () => {
  const options = requestOptions(assertion);
  assert.deepEqual(list(options.challenge), [1, 2, 3]);
  assert.equal(options.rpId, 'astovia.test');
  assert.equal(options.userVerification, 'preferred');
  assert.equal(options.timeout, 300000);
  assert.deepEqual(options.allowCredentials.map((c) => list(c.id)), [[1, 2, 3], [0xfb, 0xff]]);
  assert.deepEqual(options.allowCredentials[0].transports, ['usb']);
  for (const bad of [null, {}, { ...assertion, challenge: '' }, { ...assertion, allowCredentials: 'x' }, { ...assertion, allowCredentials: [{ type: 'public-key', id: 1 }] }]) {
    assert.throws(() => requestOptions(bad), undefined, JSON.stringify(bad));
  }
});

const created = {
  id: 'ignored',
  rawId: bytes(0xfb, 0xff),
  type: 'public-key',
  authenticatorAttachment: 'cross-platform',
  getClientExtensionResults: () => ({ credProps: { rk: false }, prf: { results: { first: bytes(1) } } }),
  response: { clientDataJSON: bytes(1, 2, 3), attestationObject: bytes(4, 5), getTransports() { return this === created.response ? ['usb', 'nfc'] : ['wrong this']; } },
};

test('a registration becomes the JSON the server verifies', () => {
  assert.deepEqual(registrationJSON(created), {
    id: '-_8', rawId: '-_8', type: 'public-key', authenticatorAttachment: 'cross-platform', clientExtensionResults: {},
    response: { clientDataJSON: 'AQID', attestationObject: 'BAU', transports: ['usb', 'nfc'] },
  });
  // It survives JSON as it is.
  assert.deepEqual(JSON.parse(JSON.stringify(registrationJSON(created))), registrationJSON(created));
  // Older browsers have no getTransports and no attachment.
  const old = { rawId: new Uint8Array([1, 2, 3]), type: 'public-key', authenticatorAttachment: null, response: { clientDataJSON: bytes(1), attestationObject: bytes(2) } };
  assert.deepEqual(registrationJSON(old), { id: 'AQID', rawId: 'AQID', type: 'public-key', clientExtensionResults: {}, response: { clientDataJSON: 'AQ', attestationObject: 'Ag' } });
});

const signed = {
  rawId: bytes(1, 2, 3),
  type: 'public-key',
  response: { clientDataJSON: bytes(1), authenticatorData: bytes(2, 3), signature: bytes(0xff, 0xfe), userHandle: new TextEncoder().encode('alice-id').buffer },
};

test('an assertion becomes the JSON the server verifies', () => {
  assert.deepEqual(assertionJSON(signed), {
    id: 'AQID', rawId: 'AQID', type: 'public-key', clientExtensionResults: {},
    response: { clientDataJSON: 'AQ', authenticatorData: 'AgM', signature: '__4', userHandle: 'YWxpY2UtaWQ' },
  });
  // Security keys return no user handle, as null or as no bytes.
  for (const userHandle of [null, undefined, bytes()]) {
    assert.equal('userHandle' in assertionJSON({ ...signed, response: { ...signed.response, userHandle } }).response, false);
  }
});

test('results that are not public key credentials are rejected', () => {
  for (const bad of [null, undefined, 'x', {}, { ...created, rawId: 'AQID' }, { ...created, type: 'password' }, { ...created, response: null },
    { ...created, response: { clientDataJSON: bytes(1) } }, { ...created, response: { clientDataJSON: 'AQ', attestationObject: bytes(1) } }]) {
    assert.throws(() => registrationJSON(bad), /Unexpected WebAuthn credential/);
  }
  for (const bad of [null, { ...signed, response: { ...signed.response, signature: undefined } }, { ...signed, response: { ...signed.response, authenticatorData: 'x' } }]) {
    assert.throws(() => assertionJSON(bad), /Unexpected WebAuthn credential/);
  }
});

function browser(credentials) {
  Object.defineProperty(globalThis, 'navigator', { configurable: true, value: { credentials } });
}

test('the browser is asked with converted options and its result is converted back', async () => {
  const asked = [];
  browser({
    create: async (options) => { asked.push(['create', options]); return created; },
    get: async (options) => { asked.push(['get', options]); return signed; },
  });
  // The server sends the argument of the browser API as authlume encodes it.
  assert.deepEqual(await createCredential({ publicKey: creation }), registrationJSON(created));
  assert.deepEqual(await getCredential({ publicKey: assertion, mediation: 'optional' }), assertionJSON(signed));
  assert.deepEqual(asked.map(([kind, options]) => [kind, Object.keys(options)]), [['create', ['publicKey']], ['get', ['publicKey', 'mediation']]]);
  assert.deepEqual(list(asked[0][1].publicKey.challenge), [1, 2, 3]);
  assert.deepEqual(list(asked[1][1].publicKey.allowCredentials[1].id), [0xfb, 0xff]);
  assert.equal(asked[1][1].mediation, 'optional');
  // Options without the wrapper are not passed to the browser.
  for (const bad of [creation, null, 'x', [], { publickey: creation }]) {
    await assert.rejects(createCredential(bad), /Unexpected WebAuthn options/, JSON.stringify(bad));
  }
  await assert.rejects(getCredential(assertion), /Unexpected WebAuthn options/);
  assert.equal(asked.length, 2);
  // A cancelled prompt and a missing result are failures.
  browser({ create: async () => null, get: async () => { throw new DOMException('cancelled', 'NotAllowedError'); } });
  await assert.rejects(createCredential({ publicKey: creation }), /Unexpected WebAuthn credential/);
  await assert.rejects(getCredential({ publicKey: assertion }), (error) => error.name === 'NotAllowedError');
});

test('support needs PublicKeyCredential and navigator.credentials', () => {
  const had = globalThis.PublicKeyCredential;
  try {
    delete globalThis.PublicKeyCredential;
    browser({ create: async () => null, get: async () => null });
    assert.equal(webauthnSupported(), false);
    globalThis.PublicKeyCredential = function PublicKeyCredential() {};
    assert.equal(webauthnSupported(), true);
    browser(undefined);
    assert.equal(webauthnSupported(), false);
    browser({ create: async () => null });
    assert.equal(webauthnSupported(), false);
  } finally {
    if (had === undefined) delete globalThis.PublicKeyCredential;
    else globalThis.PublicKeyCredential = had;
  }
});

test('browser errors get a message and other errors none', () => {
  assert.match(webauthnErrorMessage(new DOMException('x', 'NotAllowedError')), /キャンセルされたか、時間が切れました/);
  assert.equal(webauthnErrorMessage(new DOMException('x', 'InvalidStateError')), 'このパスキーは登録済みです。');
  assert.match(webauthnErrorMessage(new DOMException('x', 'SecurityError')), /アドレスでは/);
  assert.match(webauthnErrorMessage(new DOMException('x', 'NotSupportedError')), /この端末では/);
  assert.equal(webauthnErrorMessage(Object.assign(new Error('Request failed'), { status: 400, code: 'mfa_code_invalid' })), null);
  assert.equal(webauthnErrorMessage(new TypeError('Failed to fetch')), null);
  assert.equal(webauthnErrorMessage(null), null);
  assert.equal(webauthnErrorMessage('NotAllowedError'), null);
});
