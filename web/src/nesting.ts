// Subtasks in the lists and on the detail screen: pure functions, tested
// without a browser. Like actionItems.ts, this module imports nothing at
// runtime. Items nest at most two levels.
import type { ActionItem } from './actionItems';

// taskPath is the path of an item's detail screen.
export function taskPath(id: string): string {
  return `/tasks/${encodeURIComponent(id)}`;
}

// urgentTone reports whether a tone of format.ts's dueLabel is urgent: past
// dates, today and tomorrow.
export function urgentTone(tone: string | undefined): boolean {
  return tone === 'past' || tone === 'today' || tone === 'soon';
}

// urgentTime returns the earliest urgent date of an open item, as a time:
// its own due date, or the next due date of its open and candidate
// subtasks. isUrgent tells whether a date is urgent (the list passes
// urgentTone of format.ts's dueLabel). It is null when neither date is urgent.
export function urgentTime(item: ActionItem, isUrgent: (value: string) => boolean): number | null {
  if (item.status !== 'open') return null;
  const times = [item.due_at, item.children?.next_due_at ?? null]
    .filter((value): value is string => value !== null && isUrgent(value))
    .map((value) => Date.parse(value))
    .filter((time) => !Number.isNaN(time));
  return times.length > 0 ? Math.min(...times) : null;
}

// splitUrgent separates the items with an urgent date (soon) from the rest,
// each item once. soon is ordered by the earliest urgent date, past dates
// first; rest keeps the order of the list.
export function splitUrgent(items: readonly ActionItem[], isUrgent: (value: string) => boolean): { soon: ActionItem[]; rest: ActionItem[] } {
  const seen = new Set<string>();
  const soon: { item: ActionItem; time: number }[] = [];
  const rest: ActionItem[] = [];
  for (const item of items) {
    if (seen.has(item.id)) continue;
    seen.add(item.id);
    const time = urgentTime(item, isUrgent);
    if (time === null) rest.push(item);
    else soon.push({ item, time });
  }
  soon.sort((a, b) => a.time - b.time);
  return { soon: soon.map((s) => s.item), rest };
}

// canAddChild reports whether subtasks can be added to an item: an open
// item without a parent.
export function canAddChild(item: ActionItem): boolean {
  return item.parent_id === null && item.status === 'open';
}

// canJoinParent reports whether an item can be put under a parent: an open
// item with neither a parent nor subtasks.
export function canJoinParent(item: ActionItem): boolean {
  return item.parent_id === null && item.children === null && item.status === 'open';
}

// parentChoices are the items an item can be put under: other open items
// without a parent, each once. None when the item cannot have a parent.
export function parentChoices(item: ActionItem, items: readonly ActionItem[]): ActionItem[] {
  if (!canJoinParent(item)) return [];
  const seen = new Set<string>();
  return items.filter((p) => {
    if (p.id === item.id || p.status !== 'open' || p.parent_id !== null || seen.has(p.id)) return false;
    seen.add(p.id);
    return true;
  });
}

// backLink is where the detail screen goes back to: the parent's detail for
// a subtask whose parent is known, else the task list.
export function backLink(item: ActionItem): { to: string; label: string } {
  if (item.parent_id !== null && item.parent) return { to: taskPath(item.parent.id), label: `← 親: ${item.parent.content}` };
  return { to: '/', label: '← タスク' };
}

// titleLimit is the number of characters of the content in the tab title.
const titleLimit = 40;

// detailTitle is the tab title of an item's detail screen.
export function detailTitle(content: string): string {
  const head = Array.from(content.replace(/\s+/g, ' ').trim()).slice(0, titleLimit).join('');
  return `${head} – Astovia`;
}
