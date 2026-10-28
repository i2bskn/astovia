import { useId } from 'react';
import { calendarURL } from './actionItems';
import type { ActionItem, ActionItemStatus } from './actionItems';
import { autoOutcome, autoReasonLabel, httpsURL } from './automation';
import { dueLabel, eventRange, fullDateTime, happenedAt } from './format';
import { CalendarIcon, CheckIcon, ClockIcon, DismissIcon, ExternalIcon, SparkleIcon, UndoIcon } from './icons';
import Phrased from './Phrased';
import RefChip from './RefChip';

const statusMessages: Record<ActionItemStatus, string> = {
  done: '完了にしました',
  dismissed: '対象外にしました',
  open: '未完了に戻しました',
  candidate: 'タスク候補に戻しました',
};

// toastMessage describes a status change; a candidate made open becomes a
// task rather than coming back.
export function toastMessage(previous: ActionItemStatus, status: ActionItemStatus): string {
  return previous === 'candidate' && status === 'open' ? 'タスクにしました' : statusMessages[status];
}

// TaskRow is one task in a list, with the actions its status allows: a
// candidate is made a task or dropped, an event can be added to the calendar.
export default function TaskRow({ item, busy, now, index, onChange }: { item: ActionItem; busy: boolean; now: Date; index: number; onChange: (item: ActionItem, status: ActionItemStatus) => void }) {
  const titleID = useId();
  const open = item.status === 'open';
  const candidate = item.status === 'candidate';
  const event = item.kind === 'event' ? item.event : null;
  const due = open && item.due_at && !event ? dueLabel(item.due_at, now) : null;
  const calendar = open || candidate ? calendarURL(item) : null;
  const evidence = httpsURL(item.auto?.ref_url);
  const stacked = candidate || calendar !== null;
  return <li className={`task task-${item.status}${stacked ? ' task-stack' : ''}`} style={{ '--i': Math.min(index, 12) } as React.CSSProperties}>
    {candidate
      ? <span className="task-mark task-mark-candidate" aria-hidden="true"><SparkleIcon size={16} /></span>
      : <button className="task-check" type="button" disabled={busy} aria-describedby={titleID} aria-label={open ? '完了にする' : '未完了に戻す'} title={open ? '完了にする' : '未完了に戻す'} onClick={() => onChange(item, open ? 'done' : 'open')}>
        <span className="task-check-ring">{item.status === 'dismissed' ? <DismissIcon size={14} /> : <CheckIcon size={13} />}</span>
      </button>}
    <div className="task-body">
      <p className="task-title" id={titleID}><Phrased text={item.content} /></p>
      {item.note ? <p className="task-note"><Phrased text={item.note} /></p> : null}
      <div className="task-meta">
        {event ? <span className="chip chip-event" title={`予定: ${event.title}（${fullDateTime(event.start_at)}）`}><CalendarIcon size={12} />{eventRange(event.start_at, event.end_at, now)}</span> : null}
        {due ? <span className={`chip due due-${due.tone}`} title={`関連日時: ${fullDateTime(item.due_at ?? '')}`}><ClockIcon size={12} />期限 {due.text}{due.tone === 'past' ? '（超過）' : ''}</span> : null}
        {candidate ? <span className="chip chip-candidate">タスク候補</span> : null}
        {item.source ? <span className="chip chip-auto">自動検出</span> : null}
        {item.refs.map((ref, i) => <RefChip key={i} value={ref} />)}
        {item.auto ? <span className="task-auto-note">{autoReasonLabel(item.auto.reason)}ので自動で{autoOutcome(item.status)}{evidence ? <> · <a href={evidence} rel="noopener noreferrer" target="_blank">根拠<span className="sr-only">を新しいタブで開く</span></a></> : null}</span> : null}
      </div>
      <p className="task-time" title={fullDateTime(item.created_at)}>{happenedAt(item.created_at, now, item.source ? '検出' : '追加')}</p>
    </div>
    <div className="task-actions">
      {calendar ? <a className="button secondary small" href={calendar} rel="noopener noreferrer" target="_blank" aria-describedby={titleID}><CalendarIcon size={15} />Google カレンダーに追加<ExternalIcon size={13} /><span className="sr-only">（新しいタブで開く）</span></a> : null}
      {/* Always offered: the event may have been added elsewhere, by an invitation or another app. */}
      {calendar ? <button className="button secondary small" type="button" disabled={busy} aria-describedby={titleID} onClick={() => onChange(item, 'done')}><CheckIcon size={15} />登録済み</button> : null}
      {candidate
        ? <>
          <button className="button secondary small" type="button" disabled={busy} aria-describedby={titleID} onClick={() => onChange(item, 'open')}><CheckIcon size={15} />タスクにする</button>
          <button className="button ghost small" type="button" disabled={busy} aria-describedby={titleID} onClick={() => onChange(item, 'dismissed')}><DismissIcon size={15} />不要</button>
        </>
        : open
          ? <button className="button ghost small" type="button" disabled={busy} aria-describedby={titleID} onClick={() => onChange(item, 'dismissed')}><DismissIcon size={15} />対象外にする</button>
          : <button className="button ghost small" type="button" disabled={busy} aria-describedby={titleID} onClick={() => onChange(item, 'open')}><UndoIcon size={15} />未完了に戻す</button>}
    </div>
  </li>;
}
