import { useCallback, useEffect, useId, useRef, useState } from 'react';
import { request } from './auth';
import { createActionItemsClient, isConflict } from './actionItems';
import type { ActionItem } from './actionItems';
import { autoReasonLabel, httpsURL } from './automation';

const client = createActionItemsClient(request);

function formatDate(value: string): string {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString('ja-JP');
}

function AutoRow({ item, busy, onReopen }: { item: ActionItem; busy: boolean; onReopen: () => void }) {
  const auto = item.auto;
  const href = httpsURL(auto?.ref_url);
  return <li className="action-item">
    <div className="action-body">
      <p className="action-content">{item.content}</p>
      {auto ? <dl className="action-meta">
        <div><dt>理由</dt><dd>{autoReasonLabel(auto.reason)}</dd></div>
        <div><dt>処理</dt><dd>{formatDate(auto.at)}</dd></div>
      </dl> : null}
      {href ? <p className="auto-evidence"><a href={href} rel="noopener noreferrer" target="_blank">根拠を開く</a></p> : null}
    </div>
    <div className="action-buttons">
      <button className="button secondary small" type="button" disabled={busy} onClick={onReopen}>未完了に戻す</button>
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
  const [busyIDs, setBusyIDs] = useState<ReadonlySet<string>>(new Set());
  const sequence = useRef(0);
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
    try {
      await client.updateActionItem(item.id, { status: 'open', version: item.version });
      setItems((previous) => previous.filter((i) => i.id !== item.id));
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

  return <section className="action-panel" aria-labelledby={headingID}>
    <h2 id={headingID}>自動で処理したもの</h2>
    <p className="intro">連携先の状態から完了にした対応事項です。間違っていたら「未完了に戻す」を押してください。戻したものは、以後自動では処理しません。</p>
    {conflict ? <div className="notice error conflict" role="alert">
      <p>他の操作で更新されました。一覧を読み込み直してください。</p>
      <button className="button secondary small" type="button" onClick={() => { void load(); }}>一覧を読み込み直す</button>
    </div> : null}
    {error ? <p className="notice error" role="alert">{error}</p> : null}
    {items.length > 0 ? <ul className="action-list auto-list">
      {items.map((item) => <AutoRow key={item.id} item={item} busy={busyIDs.has(item.id)} onReopen={() => { void reopen(item); }} />)}
    </ul> : null}
    {!loading && !error && items.length === 0 ? <p className="action-empty">自動で処理したものはありません。</p> : null}
    {loading ? <p className="action-loading" aria-live="polite">読み込んでいます…</p> : null}
    {nextCursor && !loading ? <button className="button secondary more-button" type="button" onClick={() => { void load(nextCursor); }}>さらに表示</button> : null}
  </section>;
}
