// phrases splits Japanese text into chunks that read well when a line ends
// between them, close to bunsetsu: a word plus the hiragana (particles,
// endings) that follow it. The UI joins them with <wbr> and forbids other
// breaks, so lines end the same way in every browser; CSS auto-phrase is
// Chromium only and not applied reliably.
const hiragana = /^[ぁ-ゟー]/u;
const endsHiragana = /[ぁ-ゟー]$/u;
const opening = /^[「『（(［[【〈《"']/u;
const clause = /[、。，．！？!?]$/u;
const closing = /[」』）)\]］】〉》"'\s]$/u;
// A short Latin word and the word after it stay together ("AI クライアント",
// "Web Push"); shortWordSpace matches such a phrase so far.
export const shortWordSpace = /^[A-Za-z0-9]{1,10} $/u;
const joinable = /^[A-Za-z0-9\u30a0-\u30ff]/u;
const trailing = /^[、。，．,.!?！？」』）)\]］】〉》"'\s]/u;

const segmenter = typeof Intl !== 'undefined' && 'Segmenter' in Intl
  ? new Intl.Segmenter('ja', { granularity: 'word' })
  : null;

export function phrases(text: string): string[] {
  if (!segmenter) return [text];
  const result: string[] = [];
  let current = '';
  for (const { segment } of segmenter.segment(text)) {
    // A phrase ends after a comma or a full stop, and where a word that is
    // not hiragana follows hiragana, a space or a closing bracket
    // ("間違っていたら|未完了に"). Hiragana stays with what comes before it.
    const boundary = current !== '' && !trailing.test(segment) && !(shortWordSpace.test(current) && joinable.test(segment)) && (clause.test(current) ||
      (!hiragana.test(segment) && (endsHiragana.test(current) || closing.test(current) || opening.test(segment))));
    if (boundary) {
      result.push(current);
      current = '';
    }
    current += segment;
  }
  if (current) result.push(current);
  return result;
}
