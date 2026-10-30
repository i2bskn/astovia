// Client of the AI instructions API, with the scopes and labels of the
// "additional instructions" section of the AI settings page. Pure functions,
// tested without a browser; the request function is injected (normally
// auth.ts's request).
import type { RequestFunction } from './actionItems';

// The scopes of the instructions, in display order. The server has the
// same list.
export const instructionScopes = [
  'common',
  'github.detect',
  'slack.detect',
  'chatwork.detect',
  'gmail.detect',
  'slack.reply',
  'chatwork.reply',
  'gmail.reply',
] as const;

export type InstructionScope = typeof instructionScopes[number];

// AIInstructions has every scope; a scope without an instruction is "".
export type AIInstructions = Record<InstructionScope, string>;

// MAX_INSTRUCTION_LENGTH is the maximum length of one instruction, in code
// points after trimming, like the server's rune count.
export const MAX_INSTRUCTION_LENGTH = 2000;

// instructionGroups are the headings of the section and the scopes under
// them. The common instruction has no group heading.
export const instructionGroups: readonly { heading: string | null; items: readonly { scope: InstructionScope; label: string }[] }[] = [
  { heading: null, items: [{ scope: 'common', label: 'すべての判断' }] },
  { heading: 'タスクの検出', items: [
    { scope: 'github.detect', label: 'GitHub' },
    { scope: 'slack.detect', label: 'Slack' },
    { scope: 'chatwork.detect', label: 'Chatwork' },
    { scope: 'gmail.detect', label: 'メール' },
  ] },
  { heading: '返信で済んだかの判断', items: [
    { scope: 'slack.reply', label: 'Slack' },
    { scope: 'chatwork.reply', label: 'Chatwork' },
    { scope: 'gmail.reply', label: 'メール' },
  ] },
];

// emptyInstructions returns instructions with every scope empty.
export function emptyInstructions(): AIInstructions {
  return Object.fromEntries(instructionScopes.map((scope) => [scope, ''])) as AIInstructions;
}

// instructionLength counts the code points of value without the
// surrounding white space.
export function instructionLength(value: string): number {
  return [...value.trim()].length;
}

// instructionTooLong reports whether value is over MAX_INSTRUCTION_LENGTH.
export function instructionTooLong(value: string): boolean {
  return instructionLength(value) > MAX_INSTRUCTION_LENGTH;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

// readInstructions checks a response of the instructions API: every scope
// is a string.
export function readInstructions(data: unknown): AIInstructions {
  if (isRecord(data) && isRecord(data.instructions)) {
    const raw = data.instructions;
    if (instructionScopes.every((scope) => Object.hasOwn(raw, scope) && typeof raw[scope] === 'string')) {
      return Object.fromEntries(instructionScopes.map((scope) => [scope, raw[scope] as string])) as AIInstructions;
    }
  }
  throw new Error('Unexpected AI instructions response');
}

// TryScope is a scope that can be tried: every scope but the common one.
export type TryScope = Exclude<InstructionScope, 'common'>;

// isReplyScope reports whether the scope is a reply judgment.
export function isReplyScope(scope: TryScope): boolean {
  return scope.endsWith('.reply');
}

// TrySample is an item of a scope's integration that can be judged again.
export interface TrySample {
  id: string;
  content: string;
  status: string;
  auto_reason: string | null;
  conversation_name: string | null;
  detected_at: string | null;
}

// TryDetection is the result of a detection: event start and end are the
// local times the AI wrote (YYYY-MM-DDTHH:MM).
export interface TryDetection {
  decision: string;
  task: string;
  importance: string;
  summary: string;
  event: { title: string; start: string; end: string | null } | null;
}

export interface TryReply {
  done: boolean;
}

// TryResult is a judgment with the saved instructions (current) and with
// the draft ones.
export type TryResult = { reply: false; current: TryDetection; draft: TryDetection } | { reply: true; current: TryReply; draft: TryReply };

function isNullableString(value: unknown): value is string | null {
  return value === null || typeof value === 'string';
}

// readSamples checks a response of the samples API.
export function readSamples(data: unknown): TrySample[] {
  if (isRecord(data) && Array.isArray(data.samples) && data.samples.every((v: unknown) => isRecord(v) &&
    typeof v.id === 'string' && typeof v.content === 'string' && typeof v.status === 'string' &&
    isNullableString(v.auto_reason) && isNullableString(v.conversation_name) && isNullableString(v.detected_at))) {
    return (data.samples as Record<string, unknown>[]).map((v) => ({ id: v.id as string, content: v.content as string, status: v.status as string,
      auto_reason: v.auto_reason as string | null, conversation_name: v.conversation_name as string | null, detected_at: v.detected_at as string | null }));
  }
  throw new Error('Unexpected samples response');
}

function readDetection(value: unknown): TryDetection | null {
  if (!isRecord(value) || typeof value.decision !== 'string' || typeof value.task !== 'string' || typeof value.importance !== 'string' || typeof value.summary !== 'string') return null;
  let event: TryDetection['event'] = null;
  if (value.event !== null) {
    const e = value.event;
    if (!isRecord(e) || typeof e.title !== 'string' || typeof e.start !== 'string' || !isNullableString(e.end)) return null;
    event = { title: e.title, start: e.start, end: e.end };
  }
  return { decision: value.decision, task: value.task, importance: value.importance, summary: value.summary, event };
}

function readReply(value: unknown): TryReply | null {
  return isRecord(value) && typeof value.done === 'boolean' ? { done: value.done } : null;
}

// readTryResult checks a response of the try API of a detection (reply
// false) or a reply judgment.
export function readTryResult(data: unknown, reply: boolean): TryResult {
  if (isRecord(data)) {
    if (reply) {
      const current = readReply(data.current);
      const draft = readReply(data.draft);
      if (current && draft) return { reply: true, current, draft };
    } else {
      const current = readDetection(data.current);
      const draft = readDetection(data.draft);
      if (current && draft) return { reply: false, current, draft };
    }
  }
  throw new Error('Unexpected try response');
}

// decisionLabel, importanceLabel and sampleStatusLabel name the values for
// the results and the samples.
export function decisionLabel(decision: string): string {
  return decision === 'task' ? 'タスク' : decision === 'candidate' ? 'タスク候補' : decision === 'none' ? '対応不要' : decision;
}

export function importanceLabel(importance: string): string {
  return importance === 'high' ? '高' : importance === 'low' ? '低' : importance === 'normal' ? '中' : importance;
}

export function sampleStatusLabel(status: string): string {
  return status === 'open' ? '未完了' : status === 'candidate' ? 'タスク候補' : status === 'done' ? '完了' : status === 'dismissed' ? '対象外' : status;
}

// The fields of a detection compared between the two results.
export type DetectionField = 'decision' | 'task' | 'importance' | 'event' | 'summary';

// detectionDifferences returns the fields that differ between two
// detections. The task and the importance are compared only when one of them
// is a task or a candidate, and the summary only when one of them needs
// nothing.
export function detectionDifferences(a: TryDetection, b: TryDetection): Set<DetectionField> {
  const out = new Set<DetectionField>();
  if (a.decision !== b.decision) out.add('decision');
  const acting = a.decision !== 'none' || b.decision !== 'none';
  if (acting && a.task !== b.task) out.add('task');
  if (acting && a.importance !== b.importance) out.add('importance');
  if (JSON.stringify(a.event) !== JSON.stringify(b.event)) out.add('event');
  if ((a.decision === 'none' || b.decision === 'none') && a.summary !== b.summary) out.add('summary');
  return out;
}

// tryErrorMessages are the messages of the failures of a try or of the
// samples, by error code.
const tryErrorMessages: Record<string, string> = {
  invalid_request: `指示を${MAX_INSTRUCTION_LENGTH}文字以内にしてください。`,
  not_found: '選んだ判定は試せません。一覧を読み込み直してください。',
  not_connected: '連携が接続されていないか、使えなくなっています。連携の設定を確認してください。',
  source_unavailable: '元の会話を読み直せませんでした（削除された、または古くて取得できない可能性があります）。',
  no_reply: '自分の返信がまだないため、試せません。',
  upstream_error: '連携先から読み直せませんでした。時間をおいてお試しください。',
  rate_limited: '試す回数が多すぎます。1分ほどおいてお試しください。',
  timeout: '時間内に判定できませんでした。',
  ai_not_ready: 'AIの設定が有効ではないか、変わりました。設定を確認して、もう一度お試しください。',
  ai_cooling_down: 'AIの利用上限に達したため、しばらく判定を止めています。時間をおいてお試しください。',
  ai_key_invalid: 'AIのキーまたは接続を確認してください。',
  ai_model_unavailable: '選んだモデルを使えません。',
  ai_billing: 'AIの利用料金の設定を確認してください。',
  ai_usage_limit: 'AIの利用上限に達しました。時間をおいてお試しください。',
  ai_error: 'AIで判定できませんでした。',
};

// tryErrorMessage returns the message of an error code of a try.
export function tryErrorMessage(code: string | null): string {
  return (code !== null && tryErrorMessages[code]) || '判定できませんでした。接続を確認して、もう一度お試しください。';
}

const json = { 'Content-Type': 'application/json' };

export function createAIInstructionsClient(request: RequestFunction) {
  return {
    async getInstructions(): Promise<AIInstructions> {
      return readInstructions(await request<unknown>('/api/v1/ai/instructions'));
    },
    // saveInstructions replaces every instruction; an empty one removes it.
    async saveInstructions(instructions: AIInstructions): Promise<AIInstructions> {
      const body = Object.fromEntries(instructionScopes.map((scope) => [scope, instructions[scope]]));
      return readInstructions(await request<unknown>('/api/v1/ai/instructions', { method: 'PUT', headers: json, body: JSON.stringify({ instructions: body }) }));
    },
    // samples lists the items of the scope that can be judged again.
    async samples(scope: TryScope): Promise<TrySample[]> {
      return readSamples(await request<unknown>(`/api/v1/ai/instructions/samples?scope=${encodeURIComponent(scope)}`));
    },
    // tryInstruction judges the item again with the saved instructions and
    // with common and instruction, the draft ones of the page.
    async tryInstruction(scope: TryScope, itemID: string, common: string, instruction: string): Promise<TryResult> {
      const body = JSON.stringify({ scope, item_id: itemID, common, instruction });
      return readTryResult(await request<unknown>('/api/v1/ai/instructions/try', { method: 'POST', headers: json, body }), isReplyScope(scope));
    },
  };
}

export type AIInstructionsClient = ReturnType<typeof createAIInstructionsClient>;
