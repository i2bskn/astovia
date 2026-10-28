import { useCallback, useEffect, useId, useRef, useState } from 'react';
import type { FormEvent } from 'react';
import { Link } from 'react-router';
import { request } from './auth';
import { calendarURL, createActionItemsClient, isConflict, isInvalid, isNotFound } from './actionItems';
import type { ActionItem, ActionItemStatus } from './actionItems';
import { autoOutcome, autoReasonLabel, httpsURL } from './automation';
import { candidates } from './candidates';
import { dueLabel, eventRange, fullDateTime } from './format';
import { CalendarIcon, CheckIcon, DismissIcon, ExternalIcon, UndoIcon } from './icons';
import { backLink, canAddChild, canJoinParent, detailTitle, parentChoices } from './nesting';
import Phrased from './Phrased';
import RefChip from './RefChip';
import TaskRow, { toastMessage } from './TaskRow';
import { useNow } from './useNow';

const client = createActionItemsClient(request);

// statuses name each status, with the chip style it is shown in.
const statuses: Record<ActionItemStatus, { label: string; chip: string }> = {
  open: { label: '未完了', chip: 'chip-event' },
  candidate: { label: 'タスク候補', chip: 'chip-candidate' },
  done: { label: '完了', chip: 'chip-progress' },
  dismissed: { label: '対象外', chip: 'chip-candidate' },
};

const loadError = 'タスクを読み込めませんでした。接続を確認して、もう一度お試しください。';
const childrenError = '子タスクを読み込めませんでした。接続を確認して、もう一度お試しください。';
const statusError = '状態を変更できませんでした。接続を確認して、もう一度お試しください。';

// parentError is the message of a failed change of parent.
function parentError(failure: unknown): string {
  return isInvalid(failure)
    ? '親タスクを変更できませんでした。親を持つタスクや子を持つタスクは選べません。'
    : '親タスクを変更できませんでした。接続を確認して、もう一度お試しください。';
}

// TaskDetail shows one item with everything known about it, its subtasks
// and the actions on its status and its parent. The route creates it again
// for each id, so that nothing is carried over from another item.
export default function TaskDetail({ id }: { id: string }) {
  const [item, setItem] = useState<ActionItem | null>(null);
  const [state, setState] = useState<'loading' | 'ready' | 'notfound' | 'error'>('loading');
  const [children, setChildren] = useState<ActionItem[]>([]);
  const [childrenFailed, setChildrenFailed] = useState(false);
  const [busyIDs, setBusyIDs] = useState<ReadonlySet<string>>(new Set());
  const [error, setError] = useState<string | null>(null);
  const [conflict, setConflict] = useState(false);
  const [toast, setToast] = useState<{ item: ActionItem; previous: ActionItemStatus } | null>(null);
  const [childContent, setChildContent] = useState('');
  const [childError, setChildError] = useState<string | null>(null);
  const [adding, setAdding] = useState(false);
  const [leaveError, setLeaveError] = useState<string | null>(null);
  const [joinOpen, setJoinOpen] = useState(false);
  const [choices, setChoices] = useState<ActionItem[] | null>(null);
  const [choicesFailed, setChoicesFailed] = useState(false);
  const [parentID, setParentID] = useState('');
  const [relationError, setRelationError] = useState<string | null>(null);
  const toastTimer = useRef<number | undefined>(undefined);
  const sequence = useRef(0);
  const choiceSequence = useRef(0);
  const mounted = useRef(true);
  const now = useNow();
  const headingID = useId();
  const titleID = useId();
  const childrenHeadingID = useId();
  const childInputID = useId();
  const childErrorID = useId();
  const parentSelectID = useId();

  // load reads the item, then its subtasks when it can have some. Only the
  // response to the latest request is applied.
  const load = useCallback(async () => {
    const expected = ++sequence.current;
    let loaded: ActionItem;
    try {
      loaded = await client.getActionItem(id);
    } catch (failure) {
      if (expected !== sequence.current) return;
      setState(isNotFound(failure) ? 'notfound' : 'error');
      return;
    }
    if (expected !== sequence.current) return;
    setItem(loaded);
    setState('ready');
    // The workspace sets a generic title on every route change; the item's
    // content replaces it while this screen is shown.
    if (mounted.current) document.title = detailTitle(loaded.content);
    if (loaded.parent_id !== null) {
      setChildren([]);
      setChildrenFailed(false);
      return;
    }
    try {
      const page = await client.listChildren(loaded.id);
      if (expected !== sequence.current) return;
      setChildren(page.items);
      setChildrenFailed(false);
    } catch {
      if (expected === sequence.current) setChildrenFailed(true);
    }
  }, [id]);

  useEffect(() => {
    mounted.current = true;
    void load();
    return () => {
      mounted.current = false;
      window.clearTimeout(toastTimer.current);
    };
  }, [load]);

  function markBusy(target: string, busy: boolean): void {
    setBusyIDs((previous) => {
      const next = new Set(previous);
      if (busy) next.add(target);
      else next.delete(target);
      return next;
    });
  }

  // reload reads the item and its subtasks again after a change, and the
  // candidates of the menu badge.
  async function reload(): Promise<void> {
    setConflict(false);
    await load();
    void candidates.refresh();
  }

  // changeStatus changes the status of the item or of a subtask. The server
  // completes or reopens the parent, so both are read again.
  async function changeStatus(target: ActionItem, status: ActionItemStatus, undo = false): Promise<void> {
    if (busyIDs.has(target.id)) return;
    markBusy(target.id, true);
    setError(null);
    try {
      const updated = await client.updateActionItem(target.id, { status, version: target.version });
      await reload();
      window.clearTimeout(toastTimer.current);
      if (undo) {
        setToast(null);
      } else {
        setToast({ item: updated, previous: target.status });
        toastTimer.current = window.setTimeout(() => setToast(null), 6000);
      }
    } catch (failure) {
      if (isConflict(failure)) setConflict(true);
      else setError(statusError);
    } finally {
      markBusy(target.id, false);
    }
  }

  // setParent puts an item under a parent, or takes it out with null, and
  // reports the failure with report.
  async function setParent(target: ActionItem, parent: string | null, report: (message: string | null) => void): Promise<boolean> {
    if (busyIDs.has(target.id)) return false;
    markBusy(target.id, true);
    report(null);
    try {
      await client.updateActionItem(target.id, { parent_id: parent, version: target.version });
      await reload();
      return true;
    } catch (failure) {
      if (isConflict(failure)) setConflict(true);
      else report(parentError(failure));
      return false;
    } finally {
      markBusy(target.id, false);
    }
  }

  async function addChild(event: FormEvent<HTMLFormElement>): Promise<void> {
    event.preventDefault();
    if (!item || adding) return;
    if (!childContent.trim()) {
      setChildError('タスクの内容を入力してください。');
      return;
    }
    setChildError(null);
    setAdding(true);
    try {
      await client.createActionItem({ content: childContent, parent_id: item.id });
      setChildContent('');
      await reload();
    } catch (failure) {
      setChildError(isInvalid(failure)
        ? '子タスクを追加できませんでした。このタスクには子タスクを追加できません。'
        : '子タスクを追加できませんでした。接続を確認して、もう一度お試しください。');
    } finally {
      setAdding(false);
    }
  }

  // loadChoices reads the parents to choose from: the first page of the open
  // items without a parent.
  async function loadChoices(): Promise<void> {
    if (!item) return;
    const expected = ++choiceSequence.current;
    setChoices(null);
    setChoicesFailed(false);
    try {
      const page = await client.listActionItems({ status: 'open', view: 'top' });
      if (expected === choiceSequence.current) setChoices(parentChoices(item, page.items));
    } catch {
      if (expected === choiceSequence.current) setChoicesFailed(true);
    }
  }

  function toggleJoin(): void {
    setRelationError(null);
    if (joinOpen) {
      setJoinOpen(false);
      return;
    }
    setJoinOpen(true);
    void loadChoices();
  }

  async function join(parent: string): Promise<void> {
    if (!item) return;
    if (await setParent(item, parent, setRelationError)) setJoinOpen(false);
  }

  const back = item ? backLink(item) : { to: '/', label: '← タスク' };
  const backElement = <Link className="detail-back" to={back.to}><Phrased text={back.label} /></Link>;

  if (state === 'notfound') {
    return <section className="tasks detail" aria-labelledby={headingID}>
      {backElement}
      <h1 id={headingID} className="sr-only">タスクの詳細</h1>
      <div className="empty">
        <p className="empty-title" role="alert">タスクが見つかりませんでした。</p>
        <p className="empty-hint"><Link to="/">タスクの一覧に戻る</Link></p>
      </div>
    </section>;
  }
  if (state === 'error' || !item) {
    return <section className="tasks detail" aria-labelledby={headingID}>
      {backElement}
      <h1 id={headingID} className="sr-only">タスクの詳細</h1>
      {state === 'error' ? <div className="notice error conflict" role="alert">
        <p>{loadError}</p>
        <button className="button secondary small" type="button" onClick={() => { setState('loading'); void load(); }}>読み込み直す</button>
      </div> : <p className="list-loading" aria-live="polite">読み込んでいます…</p>}
    </section>;
  }

  const open = item.status === 'open';
  const candidate = item.status === 'candidate';
  const busy = busyIDs.has(item.id);
  const calendar = open || candidate ? calendarURL(item) : null;
  const due = item.due_at ? dueLabel(item.due_at, now) : null;
  const evidence = httpsURL(item.auto?.ref_url);
  const progress = item.children && item.children.total > 0 ? item.children : null;
  const chosen = choices?.some((p) => p.id === parentID) ? parentID : choices?.[0]?.id ?? '';
  const event = item.kind === 'event' ? item.event : null;

  return <section className="tasks detail" aria-labelledby={titleID}>
    {backElement}
    <header className="page-header detail-header">
      <h1 className="detail-title" id={titleID}><Phrased text={item.content} /></h1>
      <div className="task-meta">
        <span className={`chip ${statuses[item.status].chip}`}><span className="sr-only">状態: </span>{statuses[item.status].label}</span>
        {item.source ? <span className="chip chip-auto">自動検出</span> : null}
      </div>
    </header>

    {conflict ? <div className="notice error conflict" role="alert">
      <p>他の操作で更新されました。読み込み直してください。</p>
      <button className="button secondary small" type="button" onClick={() => { void reload(); }}>読み込み直す</button>
    </div> : null}
    {error ? <p className="notice error" role="alert">{error}</p> : null}

    <div className="detail-actions">
      {calendar ? <a className="button secondary small" href={calendar} rel="noopener noreferrer" target="_blank" aria-describedby={titleID}><CalendarIcon size={15} />Google カレンダーに追加<ExternalIcon size={13} /><span className="sr-only">（新しいタブで開く）</span></a> : null}
      {/* Always offered: the event may have been added elsewhere, by an invitation or another app. */}
      {calendar ? <button className="button secondary small" type="button" disabled={busy} aria-describedby={titleID} onClick={() => { void changeStatus(item, 'done'); }}><CheckIcon size={15} />登録済み</button> : null}
      {candidate
        ? <>
          <button className="button secondary small" type="button" disabled={busy} aria-describedby={titleID} onClick={() => { void changeStatus(item, 'open'); }}><CheckIcon size={15} />タスクにする</button>
          <button className="button ghost small" type="button" disabled={busy} aria-describedby={titleID} onClick={() => { void changeStatus(item, 'dismissed'); }}><DismissIcon size={15} />不要</button>
        </>
        : open
          ? <>
            <button className="button secondary small" type="button" disabled={busy} aria-describedby={titleID} onClick={() => { void changeStatus(item, 'done'); }}><CheckIcon size={15} />完了にする</button>
            <button className="button ghost small" type="button" disabled={busy} aria-describedby={titleID} onClick={() => { void changeStatus(item, 'dismissed'); }}><DismissIcon size={15} />対象外にする</button>
          </>
          : <button className="button secondary small" type="button" disabled={busy} aria-describedby={titleID} onClick={() => { void changeStatus(item, 'open'); }}><UndoIcon size={15} />未完了に戻す</button>}
    </div>

    <dl className="settings-facts detail-facts">
      {item.note ? <div><dt>補足</dt><dd className="detail-note"><Phrased text={item.note} /></dd></div> : null}
      {item.due_at ? <div><dt>期限</dt><dd title={fullDateTime(item.due_at)}>{due ? due.text : item.due_at}{due?.tone === 'past' && (open || candidate) ? '（超過）' : ''}</dd></div> : null}
      {event ? <div><dt>予定</dt><dd><Phrased text={event.title} /><br />{eventRange(event.start_at, event.end_at, now)}</dd></div> : null}
      <div><dt>{item.source ? '検出' : '作成'}</dt><dd>{fullDateTime(item.created_at)}</dd></div>
      {item.refs.length > 0 ? <div><dt>元の投稿</dt><dd className="detail-refs">{item.refs.map((ref, i) => <RefChip key={i} value={ref} />)}</dd></div> : null}
      {item.auto ? <div><dt>自動で処理</dt><dd>{autoReasonLabel(item.auto.reason)}ので自動で{autoOutcome(item.status)}（{fullDateTime(item.auto.at)}）{evidence ? <> · <a href={evidence} rel="noopener noreferrer" target="_blank">根拠<span className="sr-only">を新しいタブで開く</span></a></> : null}</dd></div> : null}
    </dl>

    {item.parent_id === null ? <section className="detail-section" aria-labelledby={childrenHeadingID}>
      <h3 className="group-label" id={childrenHeadingID}>子タスク{progress ? <span className="count"><span className="sr-only">完了 </span>{progress.done}/{progress.total}</span> : null}</h3>
      {childrenFailed ? <div className="notice error conflict" role="alert">
        <p>{childrenError}</p>
        <button className="button secondary small" type="button" onClick={() => { void load(); }}>読み込み直す</button>
      </div> : null}
      {leaveError ? <p className="notice error" role="alert">{leaveError}</p> : null}
      {children.length > 0 ? <ul className="task-list">
        {children.map((child, index) => <TaskRow key={child.id} item={child} index={index} now={now} busy={busyIDs.has(child.id)} onChange={(i, s) => { void changeStatus(i, s); }} onLeave={(i) => { void setParent(i, null, setLeaveError); }} />)}
      </ul> : !childrenFailed ? <p className="group-hint">子タスクはありません。</p> : null}
      {canAddChild(item) ? <form className="task-inline detail-form" onSubmit={(e) => { void addChild(e); }} aria-busy={adding}>
        <label htmlFor={childInputID} className="sr-only">子タスクの内容</label>
        <input id={childInputID} required maxLength={500} placeholder="子タスクを追加" autoComplete="off" value={childContent} onChange={(e) => { setChildContent(e.target.value); setChildError(null); }} disabled={adding} aria-describedby={childError ? childErrorID : undefined} />
        <span className="task-inline-buttons">
          <button className="button primary small" type="submit" disabled={adding}>{adding ? '追加中…' : '追加'}</button>
        </span>
        {childError ? <p id={childErrorID} className="notice error" role="alert">{childError}</p> : null}
      </form> : null}
    </section> : null}

    {item.parent_id !== null || canJoinParent(item) ? <section className="detail-section" aria-label="親タスク">
      <h3 className="group-label">親タスク</h3>
      {relationError ? <p className="notice error" role="alert">{relationError}</p> : null}
      {item.parent_id !== null ? <div className="detail-actions">
        {item.parent ? <Link className="chip chip-parent chip-link" to={back.to} title={`親タスク: ${item.parent.content}`}>親: {item.parent.content}</Link> : null}
        <button className="button ghost small" type="button" disabled={busy} onClick={() => { void setParent(item, null, setRelationError); }}>親から外す</button>
      </div> : <>
        <div className="detail-actions">
          <button className="button secondary small" type="button" disabled={busy} aria-expanded={joinOpen} onClick={toggleJoin}>親タスクに入れる</button>
        </div>
        {joinOpen ? <div className="task-inline detail-form">
          {choicesFailed ? <div className="notice error conflict" role="alert">
            <p>親タスクの候補を読み込めませんでした。接続を確認して、もう一度お試しください。</p>
            <button className="button secondary small" type="button" onClick={() => { void loadChoices(); }}>読み込み直す</button>
          </div> : choices === null ? <p className="group-hint" aria-live="polite">読み込んでいます…</p>
            : choices.length === 0 ? <p className="group-hint">入れられる親タスクがありません。未完了で、親を持たないタスクを選べます。</p>
              : <>
                <label htmlFor={parentSelectID} className="sr-only">入れる先の親タスク</label>
                <select id={parentSelectID} value={chosen} onChange={(e) => setParentID(e.target.value)} disabled={busy}>
                  {choices.map((p) => <option key={p.id} value={p.id}>{p.content}</option>)}
                </select>
                <span className="task-inline-buttons">
                  <button className="button primary small" type="button" disabled={busy || chosen === ''} onClick={() => { void join(chosen); }}>入れる</button>
                  <button className="button ghost small" type="button" onClick={() => setJoinOpen(false)}>やめる</button>
                </span>
              </>}
        </div> : null}
      </>}
    </section> : null}

    <div className="toast-region" role="status" aria-live="polite">
      {toast ? <div className="toast">
        <span className="toast-text"><span className="toast-subject">「{toast.item.content}」</span>を{toastMessage(toast.previous, toast.item.status)}</span>
        <button className="toast-action" type="button" disabled={busyIDs.has(toast.item.id)} onClick={() => { void changeStatus(toast.item, toast.previous, true); }}>元に戻す</button>
      </div> : null}
    </div>
  </section>;
}
