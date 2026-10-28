// The notice of a recent password reset, shown at the top of every screen
// for 14 days after the user completed a reset link that an owner issued.
// Closing it hides it in this browser only; the server keeps reporting it.
import type { PasswordResetNotice } from './auth';

// resetNoticeKey is the localStorage key that records, per user, the at of
// the notice closed in this browser.
export function resetNoticeKey(userID: string): string {
  return `astovia.reset-notice.${userID}`;
}

// showResetNotice reports whether notice is to be shown, given the at that
// was closed in this browser (null when none).
export function showResetNotice(notice: PasswordResetNotice | null, closedAt: string | null): boolean {
  return notice !== null && notice.at !== closedAt;
}

// resetNoticeText is the notice for notice, whose at is shown as when (the
// caller formats it, so that this module has no runtime imports).
export function resetNoticeText(notice: PasswordResetNotice, when: string): string {
  return `${when} に、オーナー（${notice.by_email}）が発行したリンクでパスワードが再設定されました。心当たりがなければオーナーに確認してください。`;
}
