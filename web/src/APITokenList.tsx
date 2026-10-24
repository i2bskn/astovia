import { useCallback, useEffect, useId, useRef, useState } from 'react';
import type { FormEvent } from 'react';
import { request } from './auth';
import { dateTime } from './format';
import { isConflict } from './actionItems';
import { createAPITokensClient } from './apiTokens';
import type { APIToken, CreatedAPIToken } from './apiTokens';
import Phrased from './Phrased';

const client = createAPITokensClient(request);

function formatDate(value: string): string {
  return dateTime(value, new Date());
}

function connectCommand(token: string): string {
  return `claude mcp add --transport http astovia ${location.origin}/mcp --header "Authorization: Bearer ${token}"`;
}

// CreatedPanel shows the secret once. It lives only in this component's state
// and is gone after a reload or navigation.
function CreatedPanel({ created, onClose }: { created: CreatedAPIToken; onClose: () => void }) {
  const [copied, setCopied] = useState(false);
  const [copyError, setCopyError] = useState(false);
  const timer = useRef<number | undefined>(undefined);

  useEffect(() => () => window.clearTimeout(timer.current), []);

  async function copy(): Promise<void> {
    try {
      await navigator.clipboard.writeText(created.token);
      setCopyError(false);
      setCopied(true);
      window.clearTimeout(timer.current);
      timer.current = window.setTimeout(() => setCopied(false), 2000);
    } catch {
      setCopied(false);
      setCopyError(true);
    }
  }

  return <div className="token-created" role="status">
    <p className="token-created-lead">このトークンは今だけ表示します。安全な場所に保管してください。</p>
    <code className="token-secret">{created.token}</code>
    <div className="token-created-actions">
      <button className="button secondary small" type="button" onClick={() => { void copy(); }}>{copied ? 'コピーしました' : 'コピー'}</button>
      <button className="button secondary small" type="button" onClick={onClose}>閉じる</button>
    </div>
    {copyError ? <p className="notice error" role="alert">コピーできませんでした。手動で選択してください。</p> : null}
    <p className="token-created-label">接続例（Claude Code）</p>
    <pre className="token-command">{connectCommand(created.token)}</pre>
  </div>;
}

function TokenRow({ token, confirming, busy, onAsk, onCancel, onDelete }: {
  token: APIToken;
  confirming: boolean;
  busy: boolean;
  onAsk: () => void;
  onCancel: () => void;
  onDelete: () => void;
}) {
  return <li className="action-item">
    <div className="action-body">
      <p className="action-content">{token.name}</p>
      <dl className="action-meta">
        <div><dt>作成</dt><dd>{formatDate(token.created_at)}</dd></div>
        <div><dt>最終使用</dt><dd>{token.last_used_at ? formatDate(token.last_used_at) : '未使用'}</dd></div>
      </dl>
    </div>
    <div className="action-buttons">
      {confirming ? <>
        <button className="button secondary small danger" type="button" disabled={busy} onClick={onDelete}>本当に削除</button>
        <button className="button secondary small" type="button" disabled={busy} onClick={onCancel}>やめる</button>
      </> : <button className="button secondary small" type="button" disabled={busy} onClick={onAsk}>削除</button>}
    </div>
  </li>;
}

export default function APITokenList() {
  const [tokens, setTokens] = useState<APIToken[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [name, setName] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const [formError, setFormError] = useState<string | null>(null);
  const [created, setCreated] = useState<CreatedAPIToken | null>(null);
  const [confirmingID, setConfirmingID] = useState<string | null>(null);
  const [deletingID, setDeletingID] = useState<string | null>(null);
  const headingID = useId();
  const nameID = useId();
  const formErrorID = useId();

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      setTokens(await client.listAPITokens());
    } catch {
      setError('API トークンを読み込めませんでした。接続を確認して、もう一度お試しください。');
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => { void load(); }, [load]);

  async function submit(event: FormEvent<HTMLFormElement>): Promise<void> {
    event.preventDefault();
    if (submitting) return;
    if (!name.trim()) {
      setFormError('名前を入力してください。');
      return;
    }
    setFormError(null);
    setSubmitting(true);
    try {
      const token = await client.createAPIToken(name);
      setName('');
      setCreated(token);
      const listed: APIToken = { id: token.id, name: token.name, created_at: token.created_at, last_used_at: token.last_used_at };
      setTokens((previous) => [listed, ...previous.filter((t) => t.id !== listed.id)]);
    } catch (failure) {
      setFormError(isConflict(failure)
        ? 'API トークンは50件までです。不要なものを削除してください。'
        : 'API トークンを発行できませんでした。接続を確認して、もう一度お試しください。');
    } finally {
      setSubmitting(false);
    }
  }

  async function remove(id: string): Promise<void> {
    if (deletingID) return;
    setDeletingID(id);
    setError(null);
    try {
      await client.deleteAPIToken(id);
      setTokens((previous) => previous.filter((t) => t.id !== id));
      // A deleted token must not stay on screen as if it were still usable.
      setCreated((current) => (current?.id === id ? null : current));
      setConfirmingID(null);
    } catch {
      setError('API トークンを削除できませんでした。接続を確認して、もう一度お試しください。');
    } finally {
      setDeletingID(null);
    }
  }

  return <section className="action-panel settings-panel" id="tokens" aria-labelledby={headingID}>
    <h2 id={headingID}>API トークン</h2>
    <p className="intro"><Phrased text="OAuth に対応していない AI クライアントや REST API から使うときに発行します。発行すると、トークンと Authorization ヘッダーに付けて接続する例を一度だけ表示します。" /></p>
    <form className="action-form" onSubmit={(event) => { void submit(event); }} aria-busy={submitting}>
      <div className="field">
        <label htmlFor={nameID}>名前</label>
        <input id={nameID} name="name" maxLength={100} placeholder="例: Claude Code" value={name} onChange={(e) => { setName(e.target.value); setFormError(null); }} disabled={submitting} aria-describedby={formError ? formErrorID : undefined} />
      </div>
      {formError ? <p id={formErrorID} className="notice error" role="alert">{formError}</p> : null}
      <button className="button primary add-button" type="submit" disabled={submitting}>{submitting ? '発行中…' : '発行'}</button>
    </form>
    {created ? <CreatedPanel created={created} onClose={() => setCreated(null)} /> : null}
    {error ? <p className="notice error" role="alert">{error}</p> : null}
    {tokens.length > 0 ? <h3 className="settings-subheading">発行済みのトークン</h3> : null}
    {tokens.length > 0 ? <ul className="action-list token-list">
      {tokens.map((token) => <TokenRow key={token.id} token={token}
        confirming={confirmingID === token.id}
        busy={deletingID === token.id}
        onAsk={() => setConfirmingID(token.id)}
        onCancel={() => setConfirmingID(null)}
        onDelete={() => { void remove(token.id); }} />)}
    </ul> : null}
    {!loading && !error && tokens.length === 0 ? <p className="action-empty">API トークンはまだありません。</p> : null}
    {loading ? <p className="action-loading" aria-live="polite">読み込んでいます…</p> : null}
  </section>;
}
