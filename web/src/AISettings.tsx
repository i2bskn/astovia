import { useCallback, useEffect, useId, useState } from 'react';
import type { FormEvent } from 'react';
import { ANTHROPIC_KEYS_URL, DEFAULT_AI_MODEL, aiErrorMessage, aiHasProblem, aiModels, aiStatusMessage, createAIClient, modelName, providerName } from './ai';
import type { AIState } from './ai';
import { APIError, request } from './auth';
import Phrased from './Phrased';

const client = createAIClient(request);

function ModelSelect({ id, value, disabled, onChange }: { id: string; value: string; disabled: boolean; onChange: (model: string) => void }) {
  return <select id={id} value={value} disabled={disabled} onChange={(e) => onChange(e.target.value)}>
    {aiModels.map((m) => <option key={m.value} value={m.value}>{m.label}</option>)}
  </select>;
}

function KeyForm({ submitLabel, initialModel, busy, error, onSubmit, onCancel }: {
  submitLabel: string;
  initialModel: string;
  busy: boolean;
  error: string | null;
  onSubmit: (key: string, model: string) => void;
  onCancel?: () => void;
}) {
  const [key, setKey] = useState('');
  const [model, setModel] = useState(initialModel);
  const [validation, setValidation] = useState<string | null>(null);
  const keyID = useId();
  const modelID = useId();
  const errorID = useId();
  const message = validation ?? error;

  function submit(event: FormEvent<HTMLFormElement>): void {
    event.preventDefault();
    if (busy) return;
    const value = key.trim();
    if (!value) {
      setValidation('API キーを入力してください。');
      return;
    }
    setValidation(null);
    onSubmit(value, model);
  }

  return <form className="action-form" onSubmit={submit} aria-busy={busy}>
    <div className="field">
      <label htmlFor={keyID}>Anthropic の API キー</label>
      <input id={keyID} name="api_key" type="password" autoComplete="off" autoCapitalize="none" spellCheck={false} maxLength={512} value={key} onChange={(e) => { setKey(e.target.value); setValidation(null); }} disabled={busy} aria-describedby={message ? errorID : undefined} />
    </div>
    <div className="field">
      <label htmlFor={modelID}>モデル</label>
      <ModelSelect id={modelID} value={model} disabled={busy} onChange={setModel} />
    </div>
    {message ? <p id={errorID} className="notice error" role="alert">{message}</p> : null}
    <div className="form-actions">
      {onCancel ? <button className="button secondary" type="button" disabled={busy} onClick={onCancel}>やめる</button> : null}
      <button className="button primary add-button" type="submit" disabled={busy}>{busy ? '確認中…' : submitLabel}</button>
    </div>
  </form>;
}

// AISettings registers the Anthropic API key and model that judge the
// user's mentions. The key is sent once and never shown again; only its
// last four characters are.
export default function AISettings() {
  const [state, setState] = useState<AIState | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [formError, setFormError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [replacing, setReplacing] = useState(false);
  const [model, setModel] = useState(DEFAULT_AI_MODEL);
  const headingID = useId();
  const modelID = useId();

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const next = await client.getAI();
      setState(next);
      if (next.configured) setModel(next.model);
    } catch {
      setError('AI の設定を読み込めませんでした。接続を確認して、もう一度お試しください。');
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => { void load(); }, [load]);

  // save stores the settings; a conflict reloads them after the message.
  async function save(nextModel: string, key: string | undefined, onError: (message: string) => void): Promise<void> {
    setBusy(true);
    setFormError(null);
    setError(null);
    try {
      const next = await client.saveAI(nextModel, key);
      setState(next);
      if (next.configured) setModel(next.model);
      setReplacing(false);
    } catch (failure) {
      const code = failure instanceof APIError ? failure.code : null;
      onError(aiErrorMessage(code));
      if (code === 'ai_conflict') await load();
    } finally {
      setBusy(false);
    }
  }

  async function remove(): Promise<void> {
    setBusy(true);
    setError(null);
    try {
      await client.deleteAI();
      setState({ configured: false });
      setModel(DEFAULT_AI_MODEL);
      setReplacing(false);
    } catch {
      setError('AI の設定を削除できませんでした。接続を確認して、もう一度お試しください。');
    } finally {
      setBusy(false);
    }
  }

  function changeModel(event: FormEvent<HTMLFormElement>): void {
    event.preventDefault();
    if (busy) return;
    void save(model, undefined, setError);
  }

  return <section className="action-panel settings-panel" id="ai" aria-labelledby={headingID}>
    <h2 id={headingID}>AI</h2>
    <p className="intro"><Phrased text="GitHub のメンションなどを読んで、あなたがやることがあるかを AI が判断します。判断のため、コメントの本文が Anthropic に送られます。費用は登録したキーの組織に請求されます。" /></p>
    <p className="notice"><Phrased text="送られる内容: リポジトリ名（非公開のものを含む）、Issue と Pull Request のタイトルと本文、コメントの本文、投稿者の GitHub ユーザー名と日時。" /></p>
    {error ? <p className="notice error" role="alert">{error}</p> : null}
    {loading ? <p className="action-loading" aria-live="polite">読み込んでいます…</p> : null}
    {!loading && state && !state.configured ? <>
      <ol className="steps">
        <li><a href={ANTHROPIC_KEYS_URL} rel="noopener noreferrer" target="_blank">Anthropic Console の API キーのページ</a>を開き、API キーを作ります。</li>
        <li>作ったキーを下に貼り付け、モデルを選んで「保存」を押します。</li>
      </ol>
      <KeyForm submitLabel="保存" initialModel={model} busy={busy} error={formError} onSubmit={(key, m) => { void save(m, key, setFormError); }} />
    </> : null}
    {!loading && state?.configured ? <>
      <dl className="settings-facts">
        <div><dt>提供元</dt><dd>{providerName(state.provider)}</dd></div>
        <div><dt>モデル</dt><dd>{modelName(state.model)}</dd></div>
        <div><dt>API キー</dt><dd>{state.key_last4 ? `…${state.key_last4}` : '登録済み'}</dd></div>
        <div><dt>状態</dt><dd className={aiHasProblem(state) ? 'status-error' : undefined}>{aiStatusMessage(state.status, state.last_error)}</dd></div>
      </dl>
      {replacing ? <KeyForm submitLabel="キーを更新" initialModel={state.model} busy={busy} error={formError} onSubmit={(key, m) => { void save(m, key, setFormError); }} onCancel={() => { setReplacing(false); setFormError(null); }} /> : <>
        <form className="action-form" onSubmit={changeModel} aria-busy={busy}>
          <div className="field">
            <label htmlFor={modelID}>モデル</label>
            <ModelSelect id={modelID} value={model} disabled={busy} onChange={setModel} />
          </div>
          <div className="form-actions">
            <button className="button primary" type="submit" disabled={busy || model === state.model}>{busy ? '確認中…' : 'モデルを保存'}</button>
          </div>
        </form>
        <div className="form-actions">
          <button className="button secondary" type="button" disabled={busy} onClick={() => setReplacing(true)}>キーを更新</button>
          <button className="button secondary danger" type="button" disabled={busy} onClick={() => { void remove(); }}>AI の設定を削除</button>
        </div>
      </>}
    </> : null}
  </section>;
}
