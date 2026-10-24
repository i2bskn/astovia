import { useCallback, useEffect, useId, useRef, useState } from 'react';
import { request } from './auth';
import { createActionItemsClient, isConflict } from './actionItems';
import type { ActionItem } from './actionItems';
import { autoReasonLabel, httpsURL } from './automation';
import { fullDateTime, happenedAt } from './format';
import { AutoIcon, ExternalIcon, UndoIcon } from './icons';
import Phrased from './Phrased';
import RefChip from './RefChip';
import { useNow } from './useNow';

const client = createActionItemsClient(request);

function AutoRow({ item, busy, now, index, onReopen }: { item: ActionItem; busy: boolean; now: Date; index: number; onReopen: () => void }) {
  const titleID = useId();
  const auto = item.auto;
  const href = httpsURL(auto?.ref_url);
  const source = item.refs[0];
  return <li className="task task-auto" style={{ '--i': Math.min(index, 12) } as React.CSSProperties}>
    <span className="task-mark" aria-hidden="true"><AutoIcon size={16} /></span>
    <div className="task-body">
      <p className="task-title" id={titleID}><Phrased text={item.content} /></p>
      {auto ? <p className="auto-reason"><strong><Phrased text={autoReasonLabel(auto.reason)} /></strong><Phrased text="ので、自動で完了にしました。" /></p> : null}
      <div className="task-meta">
        {source ? <RefChip value={source} /> : null}
      </div>
      {auto ? <p className="task-time" title={fullDateTime(auto.at)}>{happenedAt(auto.at, now, '処理')}</p> : null}
    </div>
    <div className="task-actions">
      <button className="button secondary small" type="button" disabled={busy} aria-describedby={titleID} onClick={onReopen}><UndoIcon size={15} />未完了に戻す</button>
      {href ? <a className="button ghost small" href={href} rel="noopener noreferrer" target="_blank" aria-describedby={titleID}>根拠を見る<ExternalIcon size={13} /><span className="sr-only">（新しいタブで開く）</span></a> : null}
    </div>
  </li>;
}

// AutoItemList shows the items an integration completed automatically, so
// that the user can check them and undo a wrong completion.
export default function AutoItemList() {
  const [items, setItems] = useState<ActionItem[]>([]);
  const [nextCursor, setNextCursor] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [conflict, setConflict] = useState(false);
  const [notice, setNotice] = useState<string | null>(null);
  const [busyIDs, setBusyIDs] = useState<ReadonlySet<string>>(new Set());
  const sequence = useRef(0);
  const now = useNow();
  const headingID = useId();

  const load = useCallback(async (cursor?: string) => {
    const expected = ++sequence.current;
    setLoading(true);
    setError(null);
    if (!cursor) setConflict(false);
    try {
      const page = await client.listActionItems({ status: 'done', auto: true, cursor });
      if (expected !== sequence.current) return;
      setItems((previous) => cursor ? [...previous, ...page.items] : page.items);
      setNextCursor(page.next_cursor);
    } catch {
      if (expected !== sequence.current) return;
      setError('自動で処理したものを読み込めませんでした。接続を確認して、もう一度お試しください。');
    } finally {
      if (expected === sequence.current) setLoading(false);
    }
  }, []);

  useEffect(() => { void load(); }, [load]);

  async function reopen(item: ActionItem): Promise<void> {
    if (busyIDs.has(item.id)) return;
    setBusyIDs((previous) => new Set(previous).add(item.id));
    setError(null);
    setNotice(null);
    try {
      await client.updateActionItem(item.id, { status: 'open', version: item.version });
      setItems((previous) => previous.filter((i) => i.id !== item.id));
      setNotice(`「${item.content}」を未完了に戻しました。タスクの一覧に表示されます。`);
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
    <h2 id={headingID} className="sr-only">自動で処理したものの一覧</h2>
    {notice ? <p className="notice success" role="status">{notice}</p> : null}
    {conflict ? <div className="notice error conflict" role="alert">
      <p>他の操作で更新されました。一覧を読み込み直してください。</p>
      <button className="button secondary small" type="button" onClick={() => { void load(); }}>一覧を読み込み直す</button>
    </div> : null}
    {error ? <p className="notice error" role="alert">{error}</p> : null}
    {items.length > 0 ? <ul className="task-list">
      {items.map((item, index) => <AutoRow key={item.id} item={item} index={index} now={now} busy={busyIDs.has(item.id)} onReopen={() => { void reopen(item); }} />)}
    </ul> : null}
    {!loading && !error && items.length === 0 ? <div className="empty">
      <span className="empty-mark" aria-hidden="true"><AutoIcon size={22} /></span>
      <p className="empty-title">自動で処理したものはありません。</p>
      <p className="empty-hint"><Phrased text="連携先でレビューを出したり Issue が閉じられたりすると、ここに並びます。" /></p>
    </div> : null}
    {loading ? <p className="list-loading" aria-live="polite">読み込んでいます…</p> : null}
    {nextCursor && !loading ? <button className="button secondary more-button" type="button" onClick={() => { void load(nextCursor); }}>さらに表示</button> : null}
  </section>;
}
