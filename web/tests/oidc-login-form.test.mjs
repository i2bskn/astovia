import assert from 'node:assert/strict';
import { test } from 'node:test';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import OIDCLoginForm from '../.test-build/OIDCLoginForm.js';
import { oidcLoginPath, oidcReturnTo } from '../.test-build/oidcLogin.js';

function render(returnTo) {
  return renderToStaticMarkup(createElement(OIDCLoginForm, { returnTo, label: 'シングルサインオンでログイン' }));
}

test('the sign-in form posts to the OIDC login path', () => {
  const html = render('/');
  assert.equal(oidcLoginPath, '/auth/oidc/login');
  const form = html.match(/^<form [^>]*>/)[0];
  assert.match(form, / method="post"/);
  assert.match(form, / action="\/auth\/oidc\/login"/);
  assert.match(html, /<button[^>]* type="submit">シングルサインオンでログイン<\/button>/);
  // The return_to field is its only field.
  assert.equal(html.match(/<input /g).length, 1);
});

test('the sign-in form carries the screen to return to in a hidden field', () => {
  const returnTo = oidcReturnTo('/oauth/authorize', '?client_id=https%3A%2F%2Fclaude.ai%2Fm&state=a%2Fb&x="<>');
  const html = render(returnTo);
  const input = html.match(/<input [^>]*\/>/)[0];
  assert.match(input, /type="hidden"/);
  assert.match(input, /name="return_to"/);
  assert.match(input, /value="\/oauth\/authorize\?client_id=https%3A%2F%2Fclaude\.ai%2Fm&amp;state=a%2Fb&amp;x=&quot;&lt;&gt;"/);
  assert.match(render('/settings').match(/<input [^>]*\/>/)[0], / value="\/settings"/);
});
