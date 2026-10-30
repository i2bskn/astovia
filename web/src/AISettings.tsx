import { useCallback, useEffect, useId, useState } from 'react';
import type { FormEvent } from 'react';
import { ANTHROPIC_KEYS_URL, DEFAULT_AI_MODEL, MANAGE_USAGE_URL, aiErrorMessage, aiHasProblem, aiModels, aiStatusMessage, createAIClient, endpointKey, modelName, normalizeEndpointURL, providerName } from './ai';
import type { AIProvider, AISettings as SavedAISettings, AIState, OpenAIModel } from './ai';
import { APIError, request } from './auth';
import Phrased from './Phrased';

const client = createAIClient(request);

// providerChoices are the providers offered when nothing is configured.
const providerChoices: readonly { value: AIProvider; label: string }[] = [
  { value: 'anthropic', label: 'Anthropic の API キー' },
  { value: 'openai', label: 'ChatGPT のプラン' },
  { value: 'openai_api', label: 'OpenAI の API キー' },
  { value: 'openai_compatible', label: 'OpenAI 互換' },
];

function errorCode(failure: unknown): string | null {
  return failure instanceof APIError ? failure.code : null;
}

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

type EndpointProvider = 'openai_api' | 'openai_compatible';

// EndpointForm registers or changes an OpenAI API key or an
// OpenAI-compatible endpoint. With saved settings, the fields start with the
// saved URL and model; an empty key field keeps the saved key, except for
// an endpoint whose URL changes (the key is entered again, or left empty
// for none).
function EndpointForm({ provider, saved, busy, error, onSubmit, onCancel }: {
  provider: EndpointProvider;
  saved?: SavedAISettings;
  busy: boolean;
  error: string | null;
  onSubmit: (model: string, key: string | undefined, baseUrl: string | undefined) => void;
  onCancel?: () => void;
}) {
  const compatible = provider === 'openai_compatible';
  const [url, setURL] = useState(saved?.base_url ?? '');
  const [key, setKey] = useState('');
  const [model, setModel] = useState(saved?.model ?? '');
  const [removeKey, setRemoveKey] = useState(false);
  const [validation, setValidation] = useState<string | null>(null);
  const urlID = useId();
  const keyID = useId();
  const removeID = useId();
  const modelID = useId();
  const errorID = useId();
  const message = validation ?? error;

  function submit(event: FormEvent<HTMLFormElement>): void {
    event.preventDefault();
    if (busy) return;
    const nextModel = model.trim();
    if (compatible && !url.trim()) {
      setValidation('URL を入力してください。');
      return;
    }
    if (!compatible && !saved && !key.trim()) {
      setValidation('API キーを入力してください。');
      return;
    }
    if (!nextModel) {
      setValidation('モデル名を入力してください。');
      return;
    }
    setValidation(null);
    const urlChanged = !saved || normalizeEndpointURL(url) !== normalizeEndpointURL(saved.base_url ?? '');
    onSubmit(nextModel, endpointKey(provider, key, urlChanged, removeKey), compatible ? url.trim() : undefined);
  }

  const describedBy = message ? errorID : undefined;
  return <form className="action-form" onSubmit={submit} aria-busy={busy}>
    {compatible ? <div className="field">
      <label htmlFor={urlID}>接続先の URL</label>
      <input id={urlID} name="base_url" type="url" required autoComplete="off" autoCapitalize="none" spellCheck={false} maxLength={2048} placeholder="https://example.com/v1" value={url} onChange={(e) => { setURL(e.target.value); setValidation(null); }} disabled={busy} aria-describedby={describedBy} />
    </div> : null}
    {compatible && saved ? <p className="field-hint">URL を変えるときは API キーも入れ直してください。</p> : null}
    <div className="field">
      <label htmlFor={keyID}>{compatible ? 'API キー（任意）' : 'OpenAI の API キー'}</label>
      <input id={keyID} name="api_key" type="password" autoComplete="off" autoCapitalize="none" spellCheck={false} maxLength={512} required={!compatible && !saved} value={key} onChange={(e) => { setKey(e.target.value); setValidation(null); }} disabled={busy} aria-describedby={describedBy} />
    </div>
    {compatible ? <p className="field-hint">{saved?.api_key_set ? '不要な接続先では空のまま。URL を変えないときは、空のままなら登録済みのキーを使います。' : '不要な接続先では空のまま。'}</p> : null}
    {!compatible && saved ? <p className="field-hint">空のままなら、登録済みのキーを使います。</p> : null}
    {compatible && saved?.api_key_set ? <div className="toggle-field">
      <input id={removeID} type="checkbox" checked={removeKey} disabled={busy} onChange={(e) => setRemoveKey(e.target.checked)} />
      <label htmlFor={removeID}>API キーを削除する</label>
    </div> : null}
    <div className="field">
      <label htmlFor={modelID}>モデル名</label>
      <input id={modelID} name="model" type="text" required autoComplete="off" autoCapitalize="none" spellCheck={false} maxLength={256} placeholder={compatible ? '例: meta-llama/Llama-3.1-8B-Instruct' : '例: gpt-5-mini'} value={model} onChange={(e) => { setModel(e.target.value); setValidation(null); }} disabled={busy} aria-describedby={describedBy} />
    </div>
    {message ? <p id={errorID} className="notice error" role="alert">{message}</p> : null}
    <div className="form-actions">
      {onCancel ? <button className="button secondary" type="button" disabled={busy} onClick={onCancel}>やめる</button> : null}
      <button className="button primary add-button" type="submit" disabled={busy}>{busy ? '確認中…' : '保存'}</button>
    </div>
  </form>;
}

// EndpointNotice tells what an OpenAI API key or an OpenAI-compatible
// endpoint needs.
function EndpointNotice({ provider }: { provider: EndpointProvider }) {
  return <p className="field-hint"><Phrased text={provider === 'openai_api'
    ? 'OpenAI の API キーとモデル名で判断します。費用は登録したキーの組織に請求されます。モデルは Chat Completions API と JSON スキーマの構造化出力に対応している必要があります。保存の前に、確認のための小さなリクエストを 1 回送ります（トークンを少し使います）。'
    : 'OpenRouter、Ollama、vLLM、LM Studio など、OpenAI の Chat Completions API と同じ形式の接続先で判断します。JSON スキーマの構造化出力に対応している必要があります。保存の前に、確認のための小さなリクエストを 1 回送ります。1 回の判断は 20 秒までで、応答の遅いモデルでは判断できません。'} /></p>;
}

// ChatGPTConnect connects the user's ChatGPT plan. OpenAI sends the browser
// back to 127.0.0.1, which Astovia cannot receive: the user pastes the
// address of that page instead.
function ChatGPTConnect({ busy, setBusy, onConnected, onConflict, onCancel }: {
  busy: boolean;
  setBusy: (busy: boolean) => void;
  onConnected: (state: SavedAISettings) => void;
  onConflict: () => Promise<void>;
  onCancel?: () => void;
}) {
  const [callbackURL, setCallbackURL] = useState('');
  const [error, setError] = useState<string | null>(null);
  // fallbackURL is the sign-in address when no tab could be opened.
  const [fallbackURL, setFallbackURL] = useState<string | null>(null);
  const [started, setStarted] = useState(false);
  const inputID = useId();
  const errorID = useId();

  async function start(): Promise<void> {
    if (busy) return;
    // The tab is opened before the request so that it is not blocked as a
    // popup; it gets the address once the server answers.
    const tab = window.open('about:blank', '_blank');
    if (tab) tab.opener = null;
    setBusy(true);
    setError(null);
    setFallbackURL(null);
    try {
      const { url } = await client.authorizeOpenAI();
      if (tab) {
        tab.location.href = url;
      } else {
        setFallbackURL(url);
      }
      setStarted(true);
    } catch (failure) {
      tab?.close();
      setError(aiErrorMessage(errorCode(failure), 'openai'));
    } finally {
      setBusy(false);
    }
  }

  async function complete(event: FormEvent<HTMLFormElement>): Promise<void> {
    event.preventDefault();
    if (busy) return;
    const value = callbackURL.trim();
    if (!value) {
      setError('ブラウザのアドレスバーの URL を貼り付けてください。');
      return;
    }
    setBusy(true);
    setError(null);
    setCallbackURL('');
    try {
      const next = await client.completeOpenAI(value);
      setFallbackURL(null);
      setStarted(false);
      onConnected(next);
    } catch (failure) {
      const code = errorCode(failure);
      setError(aiErrorMessage(code, 'openai'));
      if (code === 'ai_conflict') await onConflict();
    } finally {
      setBusy(false);
    }
  }

  return <div className="chatgpt-connect">
    <ol className="steps">
      <li>「ChatGPT で続ける」を押します。新しいタブで ChatGPT のサインイン画面が開きます。</li>
      <li>サインインして許可すると、「このサイトにアクセスできません」などと表示されます。それで正常です。</li>
      <li>そのタブのアドレスバーの URL（<span className="inline-code">http://127.0.0.1:1455/auth/callback?…</span>）をすべてコピーし、下の欄に貼って「接続」を押します。</li>
    </ol>
    <div className="form-actions">
      {onCancel ? <button className="button secondary" type="button" disabled={busy} onClick={onCancel}>やめる</button> : null}
      <button className="button primary" type="button" disabled={busy} onClick={() => { void start(); }}>ChatGPT で続ける</button>
    </div>
    {fallbackURL ? <p className="notice" role="status">新しいタブを開けませんでした。<a href={fallbackURL} rel="noopener noreferrer" target="_blank">ChatGPT のサインイン画面</a>を開いてください。</p> : null}
    <form className="action-form" onSubmit={(e) => { void complete(e); }} aria-busy={busy}>
      <div className="field">
        <label htmlFor={inputID}>アドレスバーの URL</label>
        <textarea id={inputID} className="callback-input" name="callback_url" rows={3} autoComplete="off" autoCapitalize="none" spellCheck={false} maxLength={8192} value={callbackURL} onChange={(e) => { setCallbackURL(e.target.value); setError(null); }} disabled={busy} placeholder="http://127.0.0.1:1455/auth/callback?code=…" aria-describedby={error ? errorID : undefined} />
      </div>
      <p className="field-hint"><Phrased text={started ? '「ChatGPT で続ける」を押してから 10 分以内に貼り付けてください。' : 'URL は「ChatGPT で続ける」を押してから 10 分以内に貼り付けてください。'} /></p>
      {error ? <p id={errorID} className="notice error" role="alert">{error}</p> : null}
      <div className="form-actions">
        <button className="button primary add-button" type="submit" disabled={busy}>{busy ? '確認中…' : '接続'}</button>
      </div>
    </form>
  </div>;
}

// PlanNotice tells that judgments use the ChatGPT plan, with the link to
// manage its usage.
function PlanNotice() {
  return <p className="notice">
    <Phrased text="ChatGPT のプランを使っています。判断のたびに、ChatGPT のプランの利用枠を使います。" />{' '}
    <a href={MANAGE_USAGE_URL} rel="noopener noreferrer" target="_blank">使用量を管理</a>
  </p>;
}

// OpenAIModelForm changes the model of the ChatGPT plan, chosen from the
// plan's list. The saved model stays selectable when the list lacks it.
function OpenAIModelForm({ saved, busy, onSave }: { saved: string; busy: boolean; onSave: (model: string) => void }) {
  const [models, setModels] = useState<OpenAIModel[] | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [model, setModel] = useState(saved);
  const modelID = useId();

  useEffect(() => {
    let active = true;
    setModels(null);
    setLoadError(null);
    client.openAIModels().then((list) => { if (active) setModels(list); }, (failure: unknown) => {
      if (active) setLoadError(`モデルの一覧を読み込めませんでした。${aiErrorMessage(errorCode(failure), 'openai')}`);
    });
    return () => { active = false; };
  }, [saved]);

  useEffect(() => { setModel(saved); }, [saved]);

  const options = models ?? [];
  const list = options.some((m) => m.id === saved) ? options : [{ id: saved, name: saved }, ...options];

  function submit(event: FormEvent<HTMLFormElement>): void {
    event.preventDefault();
    if (busy) return;
    onSave(model);
  }

  return <form className="action-form" onSubmit={submit} aria-busy={busy}>
    <div className="field">
      <label htmlFor={modelID}>モデル</label>
      <select id={modelID} value={model} disabled={busy || models === null} onChange={(e) => setModel(e.target.value)}>
        {list.map((m) => <option key={m.id} value={m.id}>{m.name === m.id ? m.id : `${m.name}（${m.id}）`}</option>)}
      </select>
    </div>
    {models === null && !loadError ? <p className="action-loading" aria-live="polite">モデルの一覧を読み込んでいます…</p> : null}
    {loadError ? <p className="notice error" role="alert">{loadError}</p> : null}
    <div className="form-actions">
      <button className="button primary" type="submit" disabled={busy || models === null || model === saved}>{busy ? '確認中…' : 'モデルを保存'}</button>
    </div>
  </form>;
}

// AISettings chooses the AI that judges the user's mentions: an Anthropic
// API key and model, or the user's ChatGPT plan. The key and the tokens are
// never shown again; only the key's last four characters and the ChatGPT
// account's address are.
export default function AISettings() {
  const [state, setState] = useState<AIState | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [formError, setFormError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [replacing, setReplacing] = useState(false);
  const [choice, setChoice] = useState<AIProvider>('anthropic');
  const [model, setModel] = useState(DEFAULT_AI_MODEL);
  const headingID = useId();
  const modelID = useId();

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const next = await client.getAI();
      setState(next);
      if (next.configured && next.provider === 'anthropic') setModel(next.model);
    } catch {
      setError('AI の設定を読み込めませんでした。接続を確認して、もう一度お試しください。');
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => { void load(); }, [load]);

  // save stores the settings; a conflict reloads them after the message.
  async function save(nextModel: string, key: string | undefined, provider: AIProvider, onError: (message: string) => void, baseUrl?: string): Promise<void> {
    setBusy(true);
    setFormError(null);
    setError(null);
    setNotice(null);
    try {
      const next = await client.saveAI(nextModel, key, provider === 'anthropic' ? undefined : provider, baseUrl);
      setState(next);
      if (next.configured && next.provider === 'anthropic') setModel(next.model);
      setReplacing(false);
    } catch (failure) {
      const code = errorCode(failure);
      onError(aiErrorMessage(code, provider));
      if (code === 'ai_conflict' || code === 'ai_provider_mismatch') await load();
    } finally {
      setBusy(false);
    }
  }

  async function remove(): Promise<void> {
    setBusy(true);
    setError(null);
    setNotice(null);
    try {
      const { revokeFailed } = await client.deleteAI();
      setState({ configured: false });
      setModel(DEFAULT_AI_MODEL);
      setReplacing(false);
      if (revokeFailed) setNotice('ChatGPT 側の接続を解除できませんでした。ChatGPT の設定から Astovia の接続を解除してください。');
    } catch {
      setError('AI の設定を削除できませんでした。接続を確認して、もう一度お試しください。');
    } finally {
      setBusy(false);
    }
  }

  function connected(next: SavedAISettings): void {
    setState(next);
    setReplacing(false);
    setError(null);
    setNotice(null);
  }

  function changeModel(event: FormEvent<HTMLFormElement>): void {
    event.preventDefault();
    if (busy) return;
    void save(model, undefined, 'anthropic', setError);
  }

  return <section className="action-panel settings-panel" id="ai" aria-labelledby={headingID}>
    <h2 id={headingID}>AI</h2>
    <p className="intro"><Phrased text="GitHub のメンションなどを読んで、あなたがやることがあるかを AI が判断します。判断のため、本文が選んだ提供元や接続先に送られます。" /></p>
    <p className="notice"><Phrased text="送られる内容: リポジトリ名（非公開のものを含む）、Issue と Pull Request のタイトルと本文、コメントの本文、投稿者の GitHub ユーザー名と日時。" /></p>
    {notice ? <p className="notice error" role="alert">{notice}</p> : null}
    {error ? <p className="notice error" role="alert">{error}</p> : null}
    {loading ? <p className="action-loading" aria-live="polite">読み込んでいます…</p> : null}
    {!loading && state && !state.configured ? <>
      <div className="segmented provider-choice" role="group" aria-label="AI の提供元">
        {providerChoices.map((c) => <button key={c.value} type="button" className="segment" aria-pressed={choice === c.value} disabled={busy} onClick={() => { setChoice(c.value); setFormError(null); }}>{c.label}</button>)}
      </div>
      {choice === 'openai_api' || choice === 'openai_compatible' ? <>
        <EndpointNotice provider={choice} />
        <EndpointForm key={choice} provider={choice} busy={busy} error={formError} onSubmit={(m, key, baseUrl) => { void save(m, key, choice, setFormError, baseUrl); }} />
      </> : choice === 'anthropic' ? <>
        <p className="field-hint"><Phrased text="費用は登録したキーの組織に請求されます。" /></p>
        <ol className="steps">
          <li><a href={ANTHROPIC_KEYS_URL} rel="noopener noreferrer" target="_blank">Anthropic Console の API キーのページ</a>を開き、API キーを作ります。</li>
          <li>作ったキーを下に貼り付け、モデルを選んで「保存」を押します。</li>
        </ol>
        <KeyForm submitLabel="保存" initialModel={model} busy={busy} error={formError} onSubmit={(key, m) => { void save(m, key, 'anthropic', setFormError); }} />
      </> : <>
        <p className="field-hint"><Phrased text="ChatGPT の Plus や Pro などのプランで判断します。判断のたびに、そのプランの利用枠を使います。" /></p>
        <ChatGPTConnect busy={busy} setBusy={setBusy} onConnected={connected} onConflict={load} />
      </>}
    </> : null}
    {!loading && state?.configured && state.provider === 'anthropic' ? <>
      <dl className="settings-facts">
        <div><dt>提供元</dt><dd>{providerName(state.provider)}</dd></div>
        <div><dt>モデル</dt><dd>{modelName(state.model)}</dd></div>
        <div><dt>API キー</dt><dd>{state.key_last4 ? `…${state.key_last4}` : '登録済み'}</dd></div>
        <div><dt>状態</dt><dd className={aiHasProblem(state) ? 'status-error' : undefined}>{aiStatusMessage(state.status, state.last_error, state.provider)}</dd></div>
      </dl>
      {replacing ? <KeyForm submitLabel="キーを更新" initialModel={state.model} busy={busy} error={formError} onSubmit={(key, m) => { void save(m, key, 'anthropic', setFormError); }} onCancel={() => { setReplacing(false); setFormError(null); }} /> : <>
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
    {!loading && state?.configured && state.provider === 'openai' ? <>
      <PlanNotice />
      <dl className="settings-facts">
        <div><dt>提供元</dt><dd>{providerName(state.provider)}</dd></div>
        <div><dt>アカウント</dt><dd>{state.account ?? '不明'}</dd></div>
        <div><dt>モデル</dt><dd>{modelName(state.model, state.provider)}</dd></div>
        <div><dt>状態</dt><dd className={aiHasProblem(state) ? 'status-error' : undefined}>
          {aiStatusMessage(state.status, state.last_error, state.provider)}
          {state.last_error === 'usage_limit' ? <span className="usage-action"><a className="button primary small" href={MANAGE_USAGE_URL} rel="noopener noreferrer" target="_blank">使用量を管理</a></span> : null}
        </dd></div>
      </dl>
      {replacing ? <ChatGPTConnect busy={busy} setBusy={setBusy} onConnected={connected} onConflict={load} onCancel={() => setReplacing(false)} /> : <>
        <OpenAIModelForm saved={state.model} busy={busy} onSave={(m) => { void save(m, undefined, 'openai', setError); }} />
        <div className="form-actions">
          <button className="button secondary" type="button" disabled={busy} onClick={() => setReplacing(true)}>つなぎ直す</button>
          <button className="button secondary danger" type="button" disabled={busy} onClick={() => { void remove(); }}>AI の設定を削除</button>
        </div>
      </>}
    </> : null}
    {!loading && state?.configured && (state.provider === 'openai_api' || state.provider === 'openai_compatible') ? <>
      <dl className="settings-facts">
        <div><dt>提供元</dt><dd>{providerName(state.provider)}</dd></div>
        {state.provider === 'openai_compatible' ? <div><dt>接続先 URL</dt><dd>{state.base_url ?? '読み出せません'}</dd></div> : null}
        <div><dt>モデル</dt><dd>{modelName(state.model, state.provider)}</dd></div>
        <div><dt>API キー</dt><dd>{state.api_key_set ? (state.key_last4 ? `…${state.key_last4}` : '登録済み') : 'なし'}</dd></div>
        <div><dt>状態</dt><dd className={aiHasProblem(state) ? 'status-error' : undefined}>{aiStatusMessage(state.status, state.last_error, state.provider)}</dd></div>
      </dl>
      {replacing ? <EndpointForm key={state.updated_at} provider={state.provider} saved={state} busy={busy} error={formError} onSubmit={(m, key, baseUrl) => { void save(m, key, state.provider, setFormError, baseUrl); }} onCancel={() => { setReplacing(false); setFormError(null); }} /> : <div className="form-actions">
        <button className="button secondary" type="button" disabled={busy} onClick={() => { setReplacing(true); setFormError(null); }}>設定を変更</button>
        <button className="button secondary danger" type="button" disabled={busy} onClick={() => { void remove(); }}>AI の設定を削除</button>
      </div>}
    </> : null}
  </section>;
}
