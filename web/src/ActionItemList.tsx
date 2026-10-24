import { useCallback, useEffect, useId, useRef, useState } from 'react';
import type { FormEvent } from 'react';
import { request } from './auth';
import { createActionItemsClient, isConflict } from './actionItems';
import type { ActionItem, ActionItemRef, ActionItemStatus } from './actionItems';
import { autoReasonLabel, httpsURL } from './automation';

const client = createActionItemsClient(request);

type Filter = ActionItemStatus | 'all';

const filters: { value: Filter; label: string; empty: string }[] = [
  { value: 'open', label: '未完了', empty: '未完了の対応事項はありません。' },
  { value: 'done', label: '完了', empty: '完了した対応事項はありません。' },
  { value: 'dismissed', label: '対象外', empty: '対象外の対応事項はありません。' },
  { value: 'all', label: 'すべて', empty: '対応事項はありません。' },
];

function matches(filter: Filter, status: ActionItemStatus): boolean {
  return filter === 'all' || filter === status;
}

function formatDate(value: string): string {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString('ja-JP');
}

// Only http(s) URLs become links, even though the server already checks them.
function safeURL(value: string | undefined): string | null {
  if (!value) return null;
  try {
    const url = new URL(value);
    return url.protocol === 'http:' || url.protocol === 'https:' ? url.href : null;
  } catch {
    return null;
  }
}

function RefLabel({ value }: { value: ActionItemRef }) {
  const label = [value.service, value.account, value.conversation_id, value.message_id].filter(Boolean).join(' / ');
  const href = safeURL(value.url);
  return href ? <a href={href} rel="noopener noreferrer" target="_blank">{label}</a> : <span>{label}</span>;
}

function ItemRow({ item, busy, onChange }: { item: ActionItem; busy: boolean; onChange: (item: ActionItem, status: ActionItemStatus) => void }) {
  return <li className="action-item">
    <div className="action-body">
      <p className="action-content">{item.source === 'github' ? <span className="source-badge">GitHub</span> : null}{item.content}</p>
      {item.note ? <p className="action-note">{item.note}</p> : null}
      <dl className="action-meta">
        {item.due_at ? <div><dt>関連日時</dt><dd>{formatDate(item.due_at)}</dd></div> : null}
        <div><dt>作成</dt><dd>{formatDate(item.created_at)}</dd></div>
        {item.auto ? <div><dt>自動で完了</dt><dd>{autoReasonLabel(item.auto.reason)}（{formatDate(item.auto.at)}）</dd></div> : null}
      </dl>
      {item.auto && httpsURL(item.auto.ref_url) ? <p className="auto-evidence"><a href={httpsURL(item.auto.ref_url) ?? undefined} rel="noopener noreferrer" target="_blank">根拠を開く</a></p> : null}
      {item.refs.length > 0 ? <ul className="action-refs" aria-label="参照">
        {item.refs.map((ref, index) => <li key={index}><RefLabel value={ref} /></li>)}
      </ul> : null}
    </div>
    <div className="action-buttons">
      {item.status === 'open' ? <>
        <button className="button secondary small" type="button" disabled={busy} onClick={() => onChange(item, 'done')}>完了</button>
        <button className="button secondary small" type="button" disabled={busy} onClick={() => onChange(item, 'dismissed')}>対象外</button>
      </> : <button className="button secondary small" type="button" disabled={busy} onClick={() => onChange(item, 'open')}>未完了に戻す</button>}
    </div>
  </li>;
}

export default function ActionItemList() {
  const [filter, setFilter] = useState<Filter>('open');
  const [items, setItems] = useState<ActionItem[]>([]);
  const [nextCursor, setNextCursor] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [conflict, setConflict] = useState(false);
  const [busyIDs, setBusyIDs] = useState<ReadonlySet<string>>(new Set());
  const [content, setContent] = useState('');
  const [note, setNote] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const [formError, setFormError] = useState<string | null>(null);
  const sequence = useRef(0);
  // Responses that arrive after a filter change are matched against the
  // filter being shown at that time.
  const shown = useRef<Filter>(filter);
  const headingID = useId();
  const formErrorID = useId();

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
      setError('対応事項を読み込めませんでした。接続を確認して、もう一度お試しください。');
    } finally {
      if (expected === sequence.current) setLoading(false);
    }
  }, []);

  useEffect(() => {
    shown.current = filter;
    setItems([]);
    setNextCursor(null);
    void load(filter);
  }, [filter, load]);

  async function submit(event: FormEvent<HTMLFormElement>): Promise<void> {
    event.preventDefault();
    if (submitting) return;
    if (!content.trim()) {
      setFormError('対応内容を入力してください。');
      return;
    }
    setFormError(null);
    setSubmitting(true);
    try {
      const item = await client.createActionItem({ content, note: note.trim() ? note : null });
      setContent('');
      setNote('');
      if (matches(shown.current, item.status)) setItems((previous) => [item, ...previous.filter((i) => i.id !== item.id)]);
    } catch {
      setFormError('対応事項を追加できませんでした。接続を確認して、もう一度お試しください。');
    } finally {
      setSubmitting(false);
    }
  }

  async function changeStatus(item: ActionItem, status: ActionItemStatus): Promise<void> {
    if (busyIDs.has(item.id)) return;
    setBusyIDs((previous) => new Set(previous).add(item.id));
    setError(null);
    try {
      const updated = await client.updateActionItem(item.id, { status, version: item.version });
      setItems((previous) => matches(shown.current, updated.status)
        ? previous.map((i) => i.id === updated.id ? updated : i)
        : previous.filter((i) => i.id !== updated.id));
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

  const empty = filters.find((f) => f.value === filter)?.empty ?? '';

  return <section className="action-panel" aria-labelledby={headingID}>
    <h2 id={headingID}>対応事項</h2>
    <form className="action-form" onSubmit={(event) => { void submit(event); }} aria-busy={submitting}>
      <div className="field">
        <label htmlFor="action-content">対応内容</label>
        <input id="action-content" name="content" required maxLength={500} value={content} onChange={(e) => { setContent(e.target.value); setFormError(null); }} disabled={submitting} aria-describedby={formError ? formErrorID : undefined} />
      </div>
      <div className="field">
        <label htmlFor="action-note">補足（任意）</label>
        <textarea id="action-note" name="note" maxLength={4000} rows={3} value={note} onChange={(e) => setNote(e.target.value)} disabled={submitting} />
      </div>
      {formError ? <p id={formErrorID} className="notice error" role="alert">{formError}</p> : null}
      <button className="button primary add-button" type="submit" disabled={submitting}>{submitting ? '追加中…' : '追加'}</button>
    </form>
    <div className="filter-tabs" aria-label="表示する状態">
      {filters.map((f) => <button key={f.value} type="button" className="filter-tab" aria-pressed={filter === f.value} onClick={() => setFilter(f.value)}>{f.label}</button>)}
    </div>
    {conflict ? <div className="notice error conflict" role="alert">
      <p>他の操作で更新されました。一覧を読み込み直してください。</p>
      <button className="button secondary small" type="button" onClick={() => { void load(filter); }}>一覧を読み込み直す</button>
    </div> : null}
    {error ? <p className="notice error" role="alert">{error}</p> : null}
    {items.length > 0 ? <ul className="action-list">
      {items.map((item) => <ItemRow key={item.id} item={item} busy={busyIDs.has(item.id)} onChange={(i, s) => { void changeStatus(i, s); }} />)}
    </ul> : null}
    {!loading && !error && items.length === 0 ? <p className="action-empty">{empty}</p> : null}
    {loading ? <p className="action-loading" aria-live="polite">読み込んでいます…</p> : null}
    {nextCursor && !loading ? <button className="button secondary more-button" type="button" onClick={() => { void load(filter, nextCursor); }}>さらに表示</button> : null}
  </section>;
}
