import assert from 'node:assert/strict';
import { test } from 'node:test';
import { createUsersClient, isForbidden, userActions, usersErrorMessage } from '../.test-build/users.js';

globalThis.location = { origin: 'https://astovia.test' };

const user = (id, role, disabled = false) => ({ id, email: `${id}@example.com`, role, disabled, created_at: '2026-10-23T01:02:03Z', mfa_enabled: false });

test('nobody disables itself and the last enabled owner stays an owner', () => {
  const me = user('me', 'owner');
  const other = user('other', 'owner');
  const member = user('member', 'member');
  const gone = user('gone', 'owner', true);
  // Two owners: both may be demoted; only the other may be disabled.
  assert.deepEqual(userActions(me, [me, other, member], 'me'), { self: true, promote: false, demote: true, disable: false, enable: false, reset: true });
  assert.deepEqual(userActions(other, [me, other, member], 'me'), { self: false, promote: false, demote: true, disable: true, enable: false, reset: true });
  assert.deepEqual(userActions(member, [me, other, member], 'me'), { self: false, promote: true, demote: false, disable: true, enable: false, reset: true });
  // The last enabled owner: a disabled owner does not count.
  assert.deepEqual(userActions(me, [me, member, gone], 'me'), { self: true, promote: false, demote: false, disable: false, enable: false, reset: true });
  // A disabled owner can be demoted and enabled, not reset or disabled.
  assert.deepEqual(userActions(gone, [me, member, gone], 'me'), { self: false, promote: false, demote: true, disable: false, enable: true, reset: false });
});

test('errors get a message per code', () => {
  const failure = (status, code = null) => Object.assign(new Error('x'), { status, code });
  assert.match(usersErrorMessage(failure(409, 'last_owner')), /オーナーがいなくなる/);
  assert.match(usersErrorMessage(failure(409, 'self')), /自分自身/);
  assert.match(usersErrorMessage(failure(409, 'email_taken')), /既にいます/);
  assert.match(usersErrorMessage(failure(409, 'user_disabled')), /再開/);
  assert.match(usersErrorMessage(failure(403, 'forbidden')), /オーナーだけ/);
  assert.match(usersErrorMessage(failure(404, 'not_found')), /見つかりません/);
  assert.match(usersErrorMessage(failure(400, 'invalid_request')), /入力内容/);
  assert.match(usersErrorMessage(new TypeError('Failed to fetch')), /通信に失敗/);
  assert.equal(isForbidden(failure(403, 'forbidden')), true);
  assert.equal(isForbidden(failure(409, 'self')), false);
});

test('the client sends the requests and checks the responses', async () => {
  const sent = [];
  const responses = {
    'GET /api/v1/users': { users: [user('a', 'owner'), user('b', 'member', true)] },
    'PATCH /api/v1/users/b': user('b', 'member'),
    'POST /api/v1/users/b/password-resets': { url: `https://astovia.test/reset-password#pwr_${'A'.repeat(43)}`, expires_at: 'x' },
    'GET /api/v1/invitations': { invitations: [{ id: 'i', email: 'c@example.com', role: 'member', created_at: 'x', expires_at: 'y', invited_by_email: 'a@example.com' }] },
    'POST /api/v1/invitations': { id: 'i', email: 'c@example.com', role: 'member', created_at: 'x', expires_at: 'y', url: `https://astovia.test/invite#inv_${'A'.repeat(43)}` },
    'DELETE /api/v1/invitations/i': undefined,
  };
  const client = createUsersClient(async (path, init) => {
    const key = `${init?.method ?? 'GET'} ${path}`;
    sent.push([key, init?.body ?? null]);
    return responses[key];
  });
  assert.equal((await client.listUsers()).length, 2);
  assert.equal((await client.updateUser('b', { disabled: false })).disabled, false);
  assert.match((await client.createPasswordReset('b')).url, /#pwr_/);
  assert.equal((await client.listInvitations())[0].invited_by_email, 'a@example.com');
  assert.match((await client.createInvitation('c@example.com', 'member', 'password')).url, /#inv_/);
  await client.revokeInvitation('i');
  assert.deepEqual(sent.map(([key, body]) => [key, body]), [
    ['GET /api/v1/users', null],
    ['PATCH /api/v1/users/b', '{"disabled":false}'],
    ['POST /api/v1/users/b/password-resets', null],
    ['GET /api/v1/invitations', null],
    ['POST /api/v1/invitations', '{"email":"c@example.com","role":"member"}'],
    ['DELETE /api/v1/invitations/i', null],
  ]);
});

test('unexpected responses are rejected', async () => {
  const reply = (data) => createUsersClient(async () => data);
  await assert.rejects(reply({ users: [{ ...user('a', 'admin') }] }).listUsers());
  await assert.rejects(reply({ users: [{ ...user('a', 'owner'), disabled: 0 }] }).listUsers());
  // mfa_enabled must be a boolean, in the list and in a changed user.
  const { mfa_enabled: _mfa, ...withoutMFA } = user('a', 'owner');
  await assert.rejects(reply({ users: [withoutMFA] }).listUsers());
  await assert.rejects(reply({ users: [{ ...user('a', 'owner'), mfa_enabled: 1 }] }).listUsers());
  await assert.rejects(reply(withoutMFA).updateUser('a', { disabled: false }));
  assert.equal((await reply({ users: [{ ...user('a', 'owner'), mfa_enabled: true }] }).listUsers())[0].mfa_enabled, true);
  await assert.rejects(reply({ url: 'https://evil.test/reset-password#pwr_x', expires_at: 'x' }).createPasswordReset('a'));
  await assert.rejects(reply({ url: 'https://astovia.test/invite#inv_x', expires_at: 'x' }).createPasswordReset('a'));
  await assert.rejects(reply({ id: 'i', email: 'c@example.com', role: 'member', created_at: 'x', expires_at: 'y', url: 'https://astovia.test/reset-password#pwr_x' }).createInvitation('c@example.com', 'member', 'password'));
  // With passwords an invitation without a link is not one.
  await assert.rejects(reply({ id: 'i', email: 'c@example.com', role: 'member', created_at: 'x', expires_at: 'y' }).createInvitation('c@example.com', 'member', 'password'));
  await assert.rejects(reply({ id: 'i', email: 'c@example.com', role: 'admin', created_at: 'x', expires_at: 'y' }).createInvitation('c@example.com', 'member', 'oidc'));
  await assert.rejects(reply({ id: 'i', email: 'c@example.com', role: 'member', created_at: 'x' }).createInvitation('c@example.com', 'member', 'oidc'));
  await assert.rejects(reply({ invitations: [{ id: 'i', email: 'c@example.com', role: 'member', created_at: 'x', expires_at: 'y' }] }).listInvitations());
});

test('with single sign-on an invitation has no link', async () => {
  const created = { id: 'i', email: 'c@example.com', role: 'owner', created_at: 'x', expires_at: 'y' };
  const sent = [];
  const reply = (data) => createUsersClient(async (path, init) => {
    sent.push([`${init?.method ?? 'GET'} ${path}`, init?.body ?? null]);
    return data;
  });
  assert.deepEqual(await reply(created).createInvitation('c@example.com', 'owner', 'oidc'), created);
  assert.deepEqual(sent, [['POST /api/v1/invitations', '{"email":"c@example.com","role":"owner"}']]);
  // A link in the response is not checked and not kept, whatever it is.
  for (const url of [`https://astovia.test/invite#inv_${'A'.repeat(43)}`, 'https://evil.test/invite#inv_x', 1, null]) {
    const invitation = await reply({ ...created, url }).createInvitation('c@example.com', 'owner', 'oidc');
    assert.deepEqual(invitation, created);
    assert.equal('url' in invitation, false);
  }
});
