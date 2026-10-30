import { useEffect, useId, useState } from 'react';
import type { FormEvent } from 'react';
import { createAIClient } from './ai';
import { MAX_INSTRUCTION_LENGTH, createAIInstructionsClient, decisionLabel, detectionDifferences, emptyInstructions, importanceLabel, instructionGroups, instructionLength, instructionScopes, instructionTooLong, isReplyScope, sampleStatusLabel, tryErrorMessage } from './aiInstructions';
import type { AIInstructions, InstructionScope, TryDetection, TryResult, TrySample, TryScope } from './aiInstructions';
import { APIError, request } from './auth';
import { dateTime, eventRange } from './format';
import Phrased from './Phrased';

const client = createAIInstructionsClient(request);
const aiClient = createAIClient(request);

// errorText is the message of a failed request of a try.
function errorText(failure: unknown): string {
  return tryErrorMessage(failure instanceof APIError ? failure.code : null);
}

function DetectionColumn({ title, value, differs }: { title: string; value: TryDetection; differs: Set<string> }) {
  const now = new Date();
  const mark = (field: string) => differs.has(field) ? 'try-value changed' : 'try-value';
  return <div className="try-column">
    <h4>{title}</h4>
    <dl>
      <dt>判定</dt><dd className={mark('decision')}>{decisionLabel(value.decision)}</dd>
      {value.decision !== 'none' ? <>
        <dt>タスク</dt><dd className={mark('task')}>{value.task}</dd>
        <dt>重要度</dt><dd className={mark('importance')}>{importanceLabel(value.importance)}</dd>
      </> : null}
      {value.event ? <><dt>予定</dt><dd className={mark('event')}>{eventRange(value.event.start, value.event.end, now)} {value.event.title}</dd></> : differs.has('event') ? <><dt>予定</dt><dd className={mark('event')}>なし</dd></> : null}
      {value.decision === 'none' ? <><dt>内容</dt><dd className={mark('summary')}>{value.summary}</dd></> : null}
    </dl>
  </div>;
}

function TryResultView({ result }: { result: TryResult }) {
  if (result.reply) {
    const changed = result.current.done !== result.draft.done ? 'try-value changed' : 'try-value';
    return <div className="try-result">
      {([['保存済みの指示', result.current.done], ['この指示', result.draft.done]] as const).map(([title, done]) => <div className="try-column" key={title}>
        <h4>{title}</h4>
        <dl><dt>判定</dt><dd className={changed}>{done ? '済んだ' : '済んでいない'}</dd></dl>
      </div>)}
    </div>;
  }
  const differs = detectionDifferences(result.current, result.draft);
  return <div className="try-result">
    <DetectionColumn title="保存済みの指示" value={result.current} differs={differs} />
    <DetectionColumn title="この指示" value={result.draft} differs={differs} />
  </div>;
}

// TryPanel lists the items of the scope that can be judged again and
// judges the chosen one with the saved instructions and the draft ones.
function TryPanel({ scope, common, instruction }: { scope: TryScope; common: string; instruction: string }) {
  const [samples, setSamples] = useState<TrySample[] | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [running, setRunning] = useState<string | null>(null);
  const [result, setResult] = useState<{ id: string; value: TryResult } | null>(null);
  const [tryError, setTryError] = useState<string | null>(null);
  const reply = isReplyScope(scope);

  useEffect(() => {
    let active = true;
    client.samples(scope).then((list) => {
      if (active) setSamples(list);
    }, (failure: unknown) => {
      if (active) setLoadError(errorText(failure));
    });
    return () => { active = false; };
  }, [scope]);

  async function run(id: string): Promise<void> {
    if (running) return;
    setRunning(id);
    setResult(null);
    setTryError(null);
    try {
      setResult({ id, value: await client.tryInstruction(scope, id, common, instruction) });
    } catch (failure) {
      setTryError(errorText(failure));
    } finally {
      setRunning(null);
    }
  }

  const now = new Date();
  if (loadError) return <div className="try-panel"><p className="notice error" role="alert">{loadError}</p></div>;
  if (!samples) return <div className="try-panel"><p className="action-loading" aria-live="polite">読み込んでいます…</p></div>;
  if (samples.length === 0) return <div className="try-panel"><p className="notice">試せる判定がまだありません。</p></div>;
  return <div className="try-panel">
    <p className="field-hint">選んだものを、保存済みの指示とこの欄の指示でもう一度判定して並べます。タスクは作られず、変わりません。元の判定のときとは会話の内容（その後の投稿や関連するタスク）が変わっているため、同じ結果になるとは限りません。{reply ? '追跡中のものは、自分の返信がまだないと試せません。' : ''}</p>
    <ul className="try-samples">
      {samples.map((sample) => <li key={sample.id}>
        <button type="button" className="try-sample" onClick={() => { void run(sample.id); }} disabled={running !== null} aria-pressed={result?.id === sample.id}>
          <span className="try-sample-content">{sample.content}</span>
          <span className="try-sample-meta">{[sample.conversation_name, sampleStatusLabel(sample.status), sample.detected_at ? dateTime(sample.detected_at, now) : null].filter(Boolean).join(' ・ ')}</span>
        </button>
      </li>)}
    </ul>
    {running ? <p className="action-loading" aria-live="polite">判定しています…</p> : null}
    {tryError ? <p className="notice error" role="alert">{tryError}</p> : null}
    {result ? <TryResultView result={result.value} /> : null}
  </div>;
}

function InstructionField({ scope, label, value, common, disabled, aiReady, aiNoteID, onChange }: {
  scope: InstructionScope;
  label: string;
  value: string;
  common: string;
  disabled: boolean;
  aiReady: boolean;
  aiNoteID: string;
  onChange: (scope: InstructionScope, value: string) => void;
}) {
  const id = useId();
  const countID = useId();
  const errorID = useId();
  const [trying, setTrying] = useState(false);
  const over = instructionTooLong(value);
  const tryScope = scope === 'common' ? null : scope;
  const tryDisabled = disabled || !aiReady || over || instructionTooLong(common);
  return <div className="field">
    <label htmlFor={id}>{label}</label>
    <textarea id={id} name={scope} rows={3} value={value} onChange={(e) => onChange(scope, e.target.value)} disabled={disabled} aria-invalid={over} aria-describedby={over ? `${countID} ${errorID}` : countID} />
    <span id={countID} className={over ? 'instruction-count over' : 'instruction-count'}>{instructionLength(value)} / {MAX_INSTRUCTION_LENGTH}</span>
    {over ? <p id={errorID} className="notice error" role="alert">{MAX_INSTRUCTION_LENGTH}文字以内にしてください。</p> : null}
    {tryScope ? <>
      <div className="try-actions">
        <button type="button" className="button secondary small" onClick={() => setTrying((v) => !v)} disabled={tryDisabled && !trying} aria-expanded={trying} aria-describedby={aiReady ? undefined : aiNoteID}>{trying ? '閉じる' : '試す'}</button>
      </div>
      {trying && !tryDisabled ? <TryPanel scope={tryScope} common={common} instruction={value} /> : null}
    </> : null}
  </div>;
}

// AIInstructionsSettings edits the user's additional instructions to the
// AI: one for every judgment and one for each process of each integration.
// They can be saved without AI settings.
export default function AIInstructionsSettings() {
  const [values, setValues] = useState<AIInstructions>(emptyInstructions);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [message, setMessage] = useState<{ error: boolean; text: string } | null>(null);
  const [aiReady, setAIReady] = useState(false);
  const headingID = useId();
  const aiNoteID = useId();

  useEffect(() => {
    let active = true;
    aiClient.getAI().then((state) => {
      if (active) setAIReady(state.configured && state.status === 'active');
    }, () => {
      if (active) setAIReady(false);
    });
    return () => { active = false; };
  }, []);

  useEffect(() => {
    let active = true;
    client.getInstructions().then((next) => {
      if (active) setValues(next);
    }, () => {
      if (active) setLoadError('追加の指示を読み込めませんでした。接続を確認して、もう一度お試しください。');
    }).finally(() => {
      if (active) setLoading(false);
    });
    return () => { active = false; };
  }, []);

  function change(scope: InstructionScope, value: string): void {
    setValues((current) => ({ ...current, [scope]: value }));
    setMessage(null);
  }

  async function save(event: FormEvent<HTMLFormElement>): Promise<void> {
    event.preventDefault();
    if (saving || loading || loadError) return;
    setSaving(true);
    setMessage(null);
    try {
      setValues(await client.saveInstructions(values));
      setMessage({ error: false, text: '保存しました。' });
    } catch (failure) {
      setMessage({ error: true, text: failure instanceof APIError && failure.status === 400
        ? `保存できませんでした。各欄を${MAX_INSTRUCTION_LENGTH}文字以内にしてください。`
        : '保存できませんでした。接続を確認して、もう一度お試しください。' });
    } finally {
      setSaving(false);
    }
  }

  const tooLong = instructionScopes.some((scope) => instructionTooLong(values[scope]));
  const disabled = saving || loading || loadError !== null;

  return <section className="action-panel settings-panel" id="ai-instructions" aria-labelledby={headingID}>
    <h2 id={headingID}>追加の指示</h2>
    <p className="intro"><Phrased text="AIの判断の基準を調整できます。出力の形式や『元の会話にないことを作らない』などの決まりは変わりません。" /></p>
    {loadError ? <p className="notice error" role="alert">{loadError}</p> : null}
    {loading ? <p className="action-loading" aria-live="polite">読み込んでいます…</p> : null}
    <p className="field-hint">各連携の欄の「試す」で、最近の判定を保存済みの指示とこの欄の指示でもう一度判定して比べられます。{aiReady ? null : <span id={aiNoteID}>AIの設定が有効なときに試せます。</span>}</p>
    <form className="action-form settings-form" onSubmit={(event) => { void save(event); }} aria-busy={saving}>
      {instructionGroups.map((group) => <div key={group.heading ?? 'common'}>
        {group.heading ? <h3 className="settings-subheading">{group.heading}</h3> : null}
        {group.items.map((item) => <InstructionField key={item.scope} scope={item.scope} label={item.label} value={values[item.scope]} common={values.common} disabled={disabled} aiReady={aiReady} aiNoteID={aiNoteID} onChange={change} />)}
      </div>)}
      {message ? <p className={message.error ? 'notice error' : 'notice success'} role={message.error ? 'alert' : 'status'}>{message.text}</p> : null}
      <div className="form-actions"><button className="button primary" type="submit" disabled={disabled || tooLong}>{saving ? '保存中…' : '保存'}</button></div>
    </form>
  </section>;
}
