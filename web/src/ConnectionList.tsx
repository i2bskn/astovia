import { useCallback, useEffect, useId, useState } from 'react';
import { request } from './auth';
import { createOAuthClient } from './oauth';
import type { Connection } from './oauth';

const client = createOAuthClient(request);

function formatDate(value: string): string {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString('ja-JP');
}

function ConnectionRow({ connection, confirming, busy, onAsk, onCancel, onDelete }: {
  connection: Connection;
  confirming: boolean;
  busy: boolean;
  onAsk: () => void;
  onCancel: () => void;
  onDelete: () => void;
}) {
  return <li className="action-item">
    <div className="action-body">
      <p className="action-content">{connection.client_name}</p>
      {connection.client_host ? <p className="action-note">{connection.client_host}</p> : null}
      <dl className="action-meta">
        <div><dt>接続</dt><dd>{formatDate(connection.created_at)}</dd></div>
        <div><dt>最終使用</dt><dd>{connection.last_used_at ? formatDate(connection.last_used_at) : '未使用'}</dd></div>
        <div><dt>期限</dt><dd>{formatDate(connection.expires_at)}</dd></div>
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

export default function ConnectionList() {
  const [connections, setConnections] = useState<Connection[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [confirmingID, setConfirmingID] = useState<string | null>(null);
  const [deletingID, setDeletingID] = useState<string | null>(null);
  const headingID = useId();
  const endpoint = `${location.origin}/mcp`;

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      setConnections(await client.listConnections());
    } catch {
      setError('接続中の AI クライアントを読み込めませんでした。接続を確認して、もう一度お試しください。');
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => { void load(); }, [load]);

  async function remove(id: string): Promise<void> {
    if (deletingID) return;
    setDeletingID(id);
    setError(null);
    try {
      await client.deleteConnection(id);
      setConnections((previous) => previous.filter((c) => c.id !== id));
      setConfirmingID(null);
    } catch {
      setError('接続を削除できませんでした。接続を確認して、もう一度お試しください。');
    } finally {
      setDeletingID(null);
    }
  }

  return <section className="action-panel token-panel" aria-labelledby={headingID}>
    <h2 id={headingID}>接続中の AI クライアント</h2>
    <p className="intro">Claude Code: <code className="inline-code">claude mcp add --transport http astovia {endpoint}</code>。Claude のカスタムコネクタや ChatGPT には <code className="inline-code">{endpoint}</code> を入力します。</p>
    {error ? <p className="notice error" role="alert">{error}</p> : null}
    {connections.length > 0 ? <ul className="action-list token-list">
      {connections.map((connection) => <ConnectionRow key={connection.id} connection={connection}
        confirming={confirmingID === connection.id}
        busy={deletingID === connection.id}
        onAsk={() => setConfirmingID(connection.id)}
        onCancel={() => setConfirmingID(null)}
        onDelete={() => { void remove(connection.id); }} />)}
    </ul> : null}
    {!loading && !error && connections.length === 0 ? <p className="action-empty">接続中の AI クライアントはありません。</p> : null}
    {loading ? <p className="action-loading" aria-live="polite">読み込んでいます…</p> : null}
  </section>;
}
