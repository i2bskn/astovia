import { useEffect, useRef, useState } from 'react';

// CopyField shows a command or URL on its own line with a copy button.
export default function CopyField({ label, value }: { label: string; value: string }) {
  const [state, setState] = useState<'idle' | 'copied' | 'failed'>('idle');
  const timer = useRef<number | undefined>(undefined);

  useEffect(() => () => window.clearTimeout(timer.current), []);

  async function copy(): Promise<void> {
    try {
      await navigator.clipboard.writeText(value);
      setState('copied');
    } catch {
      setState('failed');
    }
    window.clearTimeout(timer.current);
    timer.current = window.setTimeout(() => setState('idle'), 2000);
  }

  return <div className="copy-field">
    <p className="copy-label">{label}</p>
    <div className="copy-row">
      <code className="copy-value">{value}</code>
      <button className="button secondary small" type="button" aria-label={`${label}をコピー`} onClick={() => { void copy(); }}>{state === 'copied' ? 'コピーしました' : state === 'failed' ? '選択してコピー' : 'コピー'}</button>
    </div>
  </div>;
}
