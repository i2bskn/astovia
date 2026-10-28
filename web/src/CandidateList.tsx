import { useCallback, useEffect, useId, useRef, useState } from 'react';
import { request } from './auth';
import { createActionItemsClient, isConflict } from './actionItems';
import type { ActionItem, ActionItemStatus } from './actionItems';
import { candidates } from './candidates';
import { SparkleIcon } from './icons';
import Phrased from './Phrased';
import TaskRow, { toastMessage } from './TaskRow';
import { useNow } from './useNow';

const client = createActionItemsClient(request);

// CandidateList shows all task candidates, so that the user can make each
// one a task or drop it.
export default function CandidateList() {
  const [items, setItems] = useState<ActionItem[]>([]);
  const [nextCursor, setNextCursor] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [conflict, setConflict] = useState(false);
  const [busyIDs, setBusyIDs] = useState<ReadonlySet<string>>(new Set());
  const [toast, setToast] = useState<{ item: ActionItem; previous: ActionItemStatus } | null>(null);
  const toastTimer = useRef<number | undefined>(undefined);
  const sequence = useRef(0);
  const now = useNow();
  const headingID = useId();

  // Only the response to the latest list request is applied.
  const load = useCallback(async (cursor?: string) => {
    const expected = ++sequence.current;
    setLoading(true);
    setError(null);
    if (!cursor) setConflict(false);
    try {
      const page = await client.listActionItems({ status: 'candidate', view: 'top', cursor });
      if (expected !== sequence.current) return;
      setItems((previous) => cursor ? [...previous, ...page.items] : page.items);
      setNextCursor(page.next_cursor);
    } catch {
      if (expected !== sequence.current) return;
      setError('タスク候補を読み込めませんでした。接続を確認して、もう一度お試しください。');
    } finally {
      if (expected === sequence.current) setLoading(false);
    }
  }, []);

  useEffect(() => { void load(); }, [load]);
  useEffect(() => () => window.clearTimeout(toastTimer.current), []);

  async function changeStatus(item: ActionItem, status: ActionItemStatus, undo = false): Promise<void> {
    if (busyIDs.has(item.id)) return;
    setBusyIDs((previous) => new Set(previous).add(item.id));
    setError(null);
    try {
      const updated = await client.updateActionItem(item.id, { status, version: item.version });
      // A candidate leaves the list; an undone one comes back to the top.
      setItems((previous) => updated.status !== 'candidate'
        ? previous.filter((i) => i.id !== updated.id)
        : previous.some((i) => i.id === updated.id) ? previous.map((i) => i.id === updated.id ? updated : i) : [updated, ...previous]);
      // The list is read again: a candidate made a task can bring its
      // candidate subtasks into the list.
      void load();
      void candidates.refresh();
      window.clearTimeout(toastTimer.current);
      if (undo) {
        setToast(null);
      } else {
        setToast({ item: updated, previous: item.status });
        toastTimer.current = window.setTimeout(() => setToast(null), 6000);
      }
    } catch (failure) {
      if (isConflict(failure)) setConflict(true);
      else setError('状態を変更できませんでした。接続を確認して、もう一度お試しください。');
    } finally {
      setBusyIDs((previous) => {
        const next = new Set(previous);
        next.delete(item.id);
        return next;
      });
    }
  }

  return <section className="tasks" aria-labelledby={headingID}>
    <h2 id={headingID} className="sr-only">タスク候補の一覧</h2>
    {conflict ? <div className="notice error conflict" role="alert">
      <p>他の操作で更新されました。一覧を読み込み直してください。</p>
      <button className="button secondary small" type="button" onClick={() => { void load(); }}>一覧を読み込み直す</button>
    </div> : null}
    {error ? <p className="notice error" role="alert">{error}</p> : null}
    {items.length > 0 ? <ul className="task-list">
      {items.map((item, index) => <TaskRow key={item.id} item={item} index={index} now={now} busy={busyIDs.has(item.id)} onChange={(i, s) => { void changeStatus(i, s); }} />)}
    </ul> : null}
    {!loading && !error && items.length === 0 ? <div className="empty">
      <span className="empty-mark" aria-hidden="true"><SparkleIcon size={22} /></span>
      <p className="empty-title">タスク候補はありません。</p>
      <p className="empty-hint"><Phrased text="AI が自分宛てか判断しきれなかったメンションや発言が、ここに並びます。" /></p>
    </div> : null}
    {loading ? <p className="list-loading" aria-live="polite">読み込んでいます…</p> : null}
    {nextCursor && !loading ? <button className="button secondary more-button" type="button" onClick={() => { void load(nextCursor); }}>さらに表示</button> : null}

    <div className="toast-region" role="status" aria-live="polite">
      {toast ? <div className="toast">
        <span className="toast-text"><span className="toast-subject">「{toast.item.content}」</span>を{toastMessage(toast.previous, toast.item.status)}</span>
        <button className="toast-action" type="button" disabled={busyIDs.has(toast.item.id)} onClick={() => { void changeStatus(toast.item, toast.previous, true); }}>元に戻す</button>
      </div> : null}
    </div>
  </section>;
}
