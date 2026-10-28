// Display helpers for dates and references. Pure functions, tested without a
// browser; the current time is always passed in.
import type { ActionItemRef } from './actionItems';

const weekdays = ['日', '月', '火', '水', '木', '金', '土'];
const day = 24 * 60 * 60 * 1000;

function parse(value: string): Date | null {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? null : date;
}

function startOfDay(date: Date): number {
  return new Date(date.getFullYear(), date.getMonth(), date.getDate()).getTime();
}

function clock(date: Date): string {
  return `${date.getHours()}:${String(date.getMinutes()).padStart(2, '0')}`;
}

// shortDate is "10/24(金)", with the year only when it differs from now.
export function shortDate(date: Date, now: Date): string {
  const monthDay = `${date.getMonth() + 1}/${date.getDate()}(${weekdays[date.getDay()]})`;
  return date.getFullYear() === now.getFullYear() ? monthDay : `${date.getFullYear()}/${monthDay}`;
}

// dateTime is "10/24(金) 9:05", for lists where the exact time matters.
export function dateTime(value: string, now: Date): string {
  const date = parse(value);
  return date ? `${shortDate(date, now)} ${clock(date)}` : value;
}

// eventRange is the time of a meeting: "10/29(木) 12:30〜13:30", with the
// end date only when it differs from the start date and no end when there is
// none.
export function eventRange(start: string, end: string | null, now: Date): string {
  const from = parse(start);
  if (!from) return start;
  const to = end ? parse(end) : null;
  const head = `${shortDate(from, now)} ${clock(from)}`;
  if (!to) return head;
  return startOfDay(to) === startOfDay(from) ? `${head}〜${clock(to)}` : `${head}〜${shortDate(to, now)} ${clock(to)}`;
}

// fullDateTime is the locale date and time, for tooltips.
export function fullDateTime(value: string): string {
  const date = parse(value);
  return date ? date.toLocaleString('ja-JP') : value;
}

export type DueTone = 'past' | 'today' | 'soon' | 'later';

// dueLabel describes an item's date relative to now: past dates, today and
// tomorrow get their own tone so that they stand out in the list.
export function dueLabel(value: string, now: Date): { text: string; tone: DueTone } | null {
  const date = parse(value);
  if (!date) return null;
  const days = Math.round((startOfDay(date) - startOfDay(now)) / day);
  const time = clock(date);
  const named = days === 0 ? '今日' : days === 1 ? '明日' : days === -1 ? '昨日' : shortDate(date, now);
  const text = `${named} ${time}`;
  if (date.getTime() < now.getTime()) return { text, tone: 'past' };
  if (days === 0) return { text, tone: 'today' };
  if (days === 1) return { text, tone: 'soon' };
  return { text, tone: 'later' };
}

// happenedAt is relativeTime followed by what happened: "3時間前に検出".
export function happenedAt(value: string, now: Date, verb: string): string {
  const when = relativeTime(value, now);
  return when === 'たった今' ? `たった今${verb}` : `${when}に${verb}`;
}

// relativeTime is "3時間前" for the last week and a short date before that.
export function relativeTime(value: string, now: Date): string {
  const date = parse(value);
  if (!date) return value;
  const elapsed = now.getTime() - date.getTime();
  if (elapsed < 0) return `${shortDate(date, now)} ${clock(date)}`;
  const minutes = Math.floor(elapsed / 60000);
  if (minutes < 1) return 'たった今';
  if (minutes < 60) return `${minutes}分前`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}時間前`;
  const days = Math.floor(hours / 24);
  if (days < 7) return `${days}日前`;
  return shortDate(date, now);
}

const serviceNames: Readonly<Record<string, string>> = {
  github: 'GitHub',
  slack: 'Slack',
  chatwork: 'Chatwork',
  gmail: 'Gmail',
  email: 'メール',
  line: 'LINE',
};

// serviceLabel returns the display name of a source service, or the code
// itself when it is unknown.
export function serviceLabel(service: string): string {
  const key = service.toLowerCase();
  return Object.hasOwn(serviceNames, key) ? serviceNames[key] ?? service : service;
}

// refLabel is the short label of a reference ("Slack · acme"); the ids stay
// in refDetail for the tooltip.
export function refLabel(ref: ActionItemRef): string {
  return [serviceLabel(ref.service), ref.account].filter(Boolean).join(' · ');
}

export function refDetail(ref: ActionItemRef): string {
  return [ref.service, ref.account, ref.conversation_id, ref.message_id].filter(Boolean).join(' / ');
}
