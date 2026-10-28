import { useCallback, useEffect, useId, useRef, useState, useSyncExternalStore } from 'react';
import type { FormEvent } from 'react';
import { Link } from 'react-router';
import { request } from './auth';
import { createActionItemsClient, isConflict } from './actionItems';
import type { ActionItem, ActionItemStatus } from './actionItems';
import { candidates } from './candidates';
import { dueLabel } from './format';
import { CheckIcon, ChevronRightIcon, PlusIcon, SparkleIcon } from './icons';
import Phrased from './Phrased';
import TaskRow, { toastMessage } from './TaskRow';
import { useNow } from './useNow';
import { countLabel } from './view';

const client = createActionItemsClient(request);

type Filter = ActionItemStatus | 'all';

const filters: { value: Filter; label: string; empty: string; hint: string }[] = [
  { value: 'open', label: '未完了', empty: '未完了のタスクはありません。', hint: '連携先で見つけたものや追加したものが、ここに並びます。片付かないまま日がたつと、Astovia が通知で知らせます。' },
  { value: 'done', label: '完了', empty: '完了したタスクはありません。', hint: '完了にしたものと、Astovia が自動で完了にしたものが並びます。' },
  { value: 'dismissed', label: '対象外', empty: '対象外のタスクはありません。', hint: '「対象外」にしたものがここに並びます。' },
  { value: 'all', label: 'すべて', empty: 'タスクはありません。', hint: '上の欄から最初のタスクを追加できます。' },
];

// previewLimit is the number of task candidates the notice above the open
// items shows when opened; the rest are on the candidates screen.
const previewLimit = 3;

function matches(filter: Filter, status: ActionItemStatus): boolean {
  return filter === 'all' || filter === status;
}

function Skeleton() {
  return <ul className="task-list skeleton" aria-hidden="true">
    {[0, 1, 2].map((i) => <li key={i} className="task"><span className="skeleton-dot" /><span className="skeleton-lines"><span /><span /></span></li>)}
  </ul>;
}

// Items due today or earlier come first, so that what is about to be
// forgotten is at the top; the rest keeps the server's order.
function urgent(item: ActionItem, now: Date): boolean {
  if (item.status !== 'open' || !item.due_at) return false;
  const tone = dueLabel(item.due_at, now)?.tone;
  return tone === 'past' || tone === 'today' || tone === 'soon';
}

export default function ActionItemList() {
  const [filter, setFilter] = useState<Filter>('open');
  const [items, setItems] = useState<ActionItem[]>([]);
  const [nextCursor, setNextCursor] = useState<string | null>(null);
  const candidateState = useSyncExternalStore(candidates.subscribe, candidates.getSnapshot);
  // The candidates notice starts closed, so that it does not push the
  // tasks down.
  const [previewOpen, setPreviewOpen] = useState(false);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [conflict, setConflict] = useState(false);
  const [busyIDs, setBusyIDs] = useState<ReadonlySet<string>>(new Set());
  const [content, setContent] = useState('');
  const [note, setNote] = useState('');
  const [noteOpen, setNoteOpen] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [formError, setFormError] = useState<string | null>(null);
  const [toast, setToast] = useState<{ item: ActionItem; previous: ActionItemStatus } | null>(null);
  const toastTimer = useRef<number | undefined>(undefined);
  const sequence = useRef(0);
  // Responses that arrive after a filter change are matched against the
  // filter being shown at that time.
  const shown = useRef<Filter>(filter);
  const now = useNow();
  const headingID = useId();
  const formErrorID = useId();
  const noteID = useId();
  const previewID = useId();

  // Only the response to the latest list request is applied.
  const load = useCallback(async (current: Filter, cursor?: string) => {
    const expected = ++sequence.current;
    setLoading(true);
    setError(null);
    if (!cursor) setConflict(false);
    try {
      const page = await client.listActionItems({ status: current === 'all' ? undefined : current, cursor });
      if (expected !== sequence.current) return;
      setItems((previous) => cursor ? [...previous, ...page.items] : page.items);
      setNextCursor(page.next_cursor);
    } catch {
      if (expected !== sequence.current) return;
      setError('タスクを読み込めませんでした。接続を確認して、もう一度お試しください。');
    } finally {
      if (expected === sequence.current) setLoading(false);
    }
  }, []);

  // reload reads the shown list and the candidates again after a change.
  const reload = useCallback(() => {
    void load(shown.current);
    void candidates.refresh();
  }, [load]);

  useEffect(() => {
    shown.current = filter;
    setItems([]);
    setNextCursor(null);
    void load(filter);
  }, [filter, load]);

  useEffect(() => () => window.clearTimeout(toastTimer.current), []);

  async function submit(event: FormEvent<HTMLFormElement>): Promise<void> {
    event.preventDefault();
    if (submitting) return;
    if (!content.trim()) {
      setFormError('タスクの内容を入力してください。');
      return;
    }
    setFormError(null);
    setSubmitting(true);
    try {
      const item = await client.createActionItem({ content, note: note.trim() ? note : null });
      setContent('');
      setNote('');
      setNoteOpen(false);
      if (matches(shown.current, item.status)) setItems((previous) => [item, ...previous.filter((i) => i.id !== item.id)]);
    } catch {
      setFormError('タスクを追加できませんでした。接続を確認して、もう一度お試しください。');
    } finally {
      setSubmitting(false);
    }
  }

  async function changeStatus(item: ActionItem, status: ActionItemStatus, undo = false): Promise<void> {
    if (busyIDs.has(item.id)) return;
    setBusyIDs((previous) => new Set(previous).add(item.id));
    setError(null);
    try {
      const updated = await client.updateActionItem(item.id, { status, version: item.version });
      setItems((previous) => {
        if (!matches(shown.current, updated.status)) return previous.filter((i) => i.id !== updated.id);
        // An undone item comes back to the list it left.
        return previous.some((i) => i.id === updated.id) ? previous.map((i) => i.id === updated.id ? updated : i) : [updated, ...previous];
      });
      reload();
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

  const current = filters.find((f) => f.value === filter) ?? { empty: '', hint: '' };
  // The candidates notice is shown with the open items only. The preview
  // takes the newest few; the server lists them newest first.
  const candidateCount = filter === 'open' ? candidateState.items.length : 0;
  const preview = candidateState.items.slice(0, previewLimit);
  // Past dates first, oldest first, then the nearest upcoming ones.
  const soon = filter === 'open' ? items.filter((i) => urgent(i, now)).sort((a, b) => Date.parse(a.due_at ?? '') - Date.parse(b.due_at ?? '')) : [];
  const rest = filter === 'open' ? items.filter((i) => !urgent(i, now)) : items;
  const row = (item: ActionItem, index: number) => <TaskRow key={item.id} item={item} index={index} now={now} busy={busyIDs.has(item.id)} onChange={(i, s) => { void changeStatus(i, s); }} />;

  return <section className="tasks" aria-labelledby={headingID}>
    <h2 id={headingID} className="sr-only">タスクの一覧</h2>
    <form className="composer" onSubmit={(event) => { void submit(event); }} aria-busy={submitting}>
      <div className="composer-row">
        <label htmlFor="action-content" className="sr-only">新しいタスク</label>
        <span className="composer-icon" aria-hidden="true"><PlusIcon /></span>
        <input id="action-content" name="content" required maxLength={500} placeholder="タスクを追加" autoComplete="off" value={content} onChange={(e) => { setContent(e.target.value); setFormError(null); }} disabled={submitting} aria-describedby={formError ? formErrorID : undefined} />
        <button className="button primary" type="submit" disabled={submitting}>{submitting ? '追加中…' : '追加'}</button>
      </div>
      {noteOpen ? <div className="composer-note">
        <label htmlFor={noteID}>補足</label>
        <textarea id={noteID} name="note" maxLength={4000} rows={3} value={note} onChange={(e) => setNote(e.target.value)} disabled={submitting} />
      </div> : null}
      <div className="composer-foot">
        <button className="link-button" type="button" aria-expanded={noteOpen} onClick={() => setNoteOpen((open) => !open)}>{noteOpen ? '補足を閉じる' : note.trim() ? '補足を編集（入力済み）' : '補足を書く'}</button>
      </div>
      {formError ? <p id={formErrorID} className="notice error" role="alert">{formError}</p> : null}
    </form>

    <div className="segmented" role="group" aria-label="表示する状態">
      {filters.map((f) => <button key={f.value} type="button" className="segment" aria-pressed={filter === f.value} onClick={() => setFilter(f.value)}>{f.label}</button>)}
    </div>

    {conflict ? <div className="notice error conflict" role="alert">
      <p>他の操作で更新されました。一覧を読み込み直してください。</p>
      <button className="button secondary small" type="button" onClick={() => { void load(filter); }}>一覧を読み込み直す</button>
    </div> : null}
    {error ? <p className="notice error" role="alert">{error}</p> : null}

    {candidateCount > 0 ? <div className="candidate-notice">
      <button className="candidate-toggle" type="button" aria-expanded={previewOpen} aria-controls={previewID} onClick={() => setPreviewOpen((open) => !open)}>
        <span className="candidate-mark" aria-hidden="true"><SparkleIcon size={16} /></span>
        <span className="candidate-text">タスク候補が <strong>{countLabel(candidateCount, candidateState.more)}件</strong> あります</span>
        <ChevronRightIcon size={16} />
      </button>
      {previewOpen ? <div id={previewID} className="candidate-preview">
        <p className="group-hint">AI が自分宛てか判断しきれなかったものです。タスクにするか、不要かを選んでください。</p>
        <ul className="task-list">{preview.map(row)}</ul>
        <Link className="candidate-all" to="/candidates">すべて見る（{countLabel(candidateCount, candidateState.more)}件）</Link>
      </div> : null}
    </div> : null}
    {soon.length > 0 ? <div className="task-group">
      <h3 className="group-label urgent">期限切れ・まもなく期限<span className="count">{soon.length}</span></h3>
      <ul className="task-list">{soon.map(row)}</ul>
    </div> : null}
    {rest.length > 0 ? <div className="task-group">
      {soon.length > 0 ? <h3 className="group-label">期限なし・先の予定<span className="count">{rest.length}</span></h3> : null}
      <ul className="task-list">{rest.map((item, i) => row(item, i + soon.length))}</ul>
    </div> : null}

    {!loading && !error && items.length === 0 ? <div className="empty">
      <span className="empty-mark" aria-hidden="true"><CheckIcon size={22} /></span>
      <p className="empty-title"><Phrased text={current.empty} /></p>
      <p className="empty-hint"><Phrased text={current.hint} /></p>
    </div> : null}
    {loading && items.length === 0 ? <><p className="sr-only" aria-live="polite">読み込んでいます…</p><Skeleton /></> : null}
    {loading && items.length > 0 ? <p className="list-loading" aria-live="polite">読み込んでいます…</p> : null}
    {nextCursor && !loading ? <button className="button secondary more-button" type="button" onClick={() => { void load(filter, nextCursor); }}>さらに表示</button> : null}

    <div className="toast-region" role="status" aria-live="polite">
      {toast ? <div className="toast">
        <span className="toast-text"><span className="toast-subject">「{toast.item.content}」</span>を{toastMessage(toast.previous, toast.item.status)}</span>
        <button className="toast-action" type="button" disabled={busyIDs.has(toast.item.id)} onClick={() => { void changeStatus(toast.item, toast.previous, true); }}>元に戻す</button>
      </div> : null}
    </div>
  </section>;
}
