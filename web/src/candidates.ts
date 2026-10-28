import { request } from './auth';
import { createActionItemsClient } from './actionItems';
import type { ActionItem } from './actionItems';

const client = createActionItemsClient(request);

// CandidateState is the first page of the task candidates, newest first:
// more is set when there are further pages.
export interface CandidateState {
  items: ActionItem[];
  more: boolean;
}

const empty: CandidateState = { items: [], more: false };
let state: CandidateState = empty;
let sequence = 0;
const listeners = new Set<() => void>();

function publish(next: CandidateState): void {
  state = next;
  listeners.forEach((listener) => listener());
}

// candidates keeps the task candidates shown in the menu badge and above
// the open tasks, for useSyncExternalStore. Screens that change an item call
// refresh.
export const candidates = {
  subscribe(listener: () => void): () => void {
    listeners.add(listener);
    return () => { listeners.delete(listener); };
  },
  getSnapshot(): CandidateState {
    return state;
  },
  // refresh reads the first page again; only the latest response is kept,
  // and a failure leaves the last one.
  async refresh(): Promise<void> {
    const expected = ++sequence;
    try {
      const page = await client.listActionItems({ status: 'candidate' });
      if (expected === sequence) publish({ items: page.items, more: page.next_cursor !== null });
    } catch {
      // The next refresh tries again.
    }
  },
  // reset forgets the candidates on sign-out; a response on its way is
  // ignored.
  reset(): void {
    sequence++;
    publish(empty);
  },
};
