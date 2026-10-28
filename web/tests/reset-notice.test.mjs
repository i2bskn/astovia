import assert from 'node:assert/strict';
import { test } from 'node:test';
import { resetNoticeKey, resetNoticeText, showResetNotice } from '../.test-build/resetNotice.js';

const notice = { at: '2026-10-23T01:02:03Z', by_email: 'owner@example.com' };

test('the notice is shown until it is closed in this browser', () => {
  assert.equal(resetNoticeKey('user-1'), 'astovia.reset-notice.user-1');
  assert.equal(showResetNotice(null, null), false);
  assert.equal(showResetNotice(notice, null), true);
  assert.equal(showResetNotice(notice, notice.at), false);
  // A newer reset is shown again even after an older one was closed.
  assert.equal(showResetNotice({ ...notice, at: '2026-10-25T00:00:00Z' }, notice.at), true);
});

test('the notice names the time and the owner', () => {
  const text = resetNoticeText(notice, '10/23(金) 10:02');
  assert.ok(text.includes('owner@example.com'), text);
  assert.match(text, /オーナー（owner@example\.com）が発行したリンクでパスワードが再設定されました。心当たりがなければオーナーに確認してください。$/);
  assert.ok(text.startsWith('10/23(金) 10:02 に、'), text);
});
