import { Fragment, useMemo } from 'react';
import { phrases } from './phrases';

// The space after a short leading Latin word becomes non-breaking, so that
// the phrase does not wrap there either.
const leadingWord = /^([A-Za-z0-9]{1,10}) (?=[A-Za-z0-9\u30a0-\u30ff])/u;

// Phrased lets Japanese text wrap only between phrases (see phrases.ts).
export default function Phrased({ text }: { text: string }) {
  const parts = useMemo(() => phrases(text).map((part) => part.replace(leadingWord, '$1\u00a0')), [text]);
  return <span className="phrased">{parts.map((part, i) => <Fragment key={i}>{i > 0 ? <wbr /> : null}{part}</Fragment>)}</span>;
}
