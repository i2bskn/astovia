import type { ActionItemRef } from './actionItems';
import { refDetail, refLabel } from './format';
import { ExternalIcon } from './icons';

// Only http(s) URLs become links, even though the server already checks them.
function safeURL(value: string | undefined): string | null {
  if (!value) return null;
  try {
    const url = new URL(value);
    return url.protocol === 'http:' || url.protocol === 'https:' ? url.href : null;
  } catch {
    return null;
  }
}

// RefChip names where an item came from. Sources are outlined so that they
// read differently from the filled status marks; linked ones carry an arrow.
export default function RefChip({ value }: { value: ActionItemRef }) {
  const href = safeURL(value.url);
  return href
    ? <a className="chip chip-ref chip-link" href={href} rel="noopener noreferrer" target="_blank" title={refDetail(value)}>{refLabel(value)}<ExternalIcon size={12} /><span className="sr-only">（新しいタブで開く）</span></a>
    : <span className="chip chip-ref" title={refDetail(value)}>{refLabel(value)}</span>;
}
