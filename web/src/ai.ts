// Client of the AI settings API, with the labels and messages of the AI
// settings page. Pure functions, tested without a browser; the request
// function is injected (normally auth.ts's request).
import type { RequestFunction } from './actionItems';

// The models a user can choose, the default first. The server has the
// same list.
export const aiModels: readonly { value: string; label: string }[] = [
  { value: 'claude-haiku-5-5', label: 'Claude Haiku 5.5（既定・おすすめ）' },
  { value: 'claude-sonnet-5-5', label: 'Claude Sonnet 5.5' },
  { value: 'claude-opus-5-5', label: 'Claude Opus 5.5' },
];

export const DEFAULT_AI_MODEL = 'claude-haiku-5-5';

// ANTHROPIC_KEYS_URL opens the API key page of the Anthropic Console.
export const ANTHROPIC_KEYS_URL = 'https://platform.claude.com/settings/keys';

export interface AISettings {
  configured: true;
  provider: string;
  model: string;
  key_last4: string;
  status: string;
  last_error: string | null;
  updated_at: string;
}

export type AIState = { configured: false } | AISettings;

const modelNames: Readonly<Record<string, string>> = {
  'claude-haiku-5-5': 'Claude Haiku 5.5',
  'claude-sonnet-5-5': 'Claude Sonnet 5.5',
  'claude-opus-5-5': 'Claude Opus 5.5',
};

// modelName returns the display name of a model, or the model itself when
// it is unknown.
export function modelName(model: string): string {
  return Object.hasOwn(modelNames, model) ? modelNames[model] ?? model : model;
}

export function providerName(provider: string): string {
  return provider === 'anthropic' ? 'Anthropic' : provider;
}

const statusErrors: Readonly<Record<string, string>> = {
  key_invalid: 'API キーが無効になりました。キーを更新してください。',
  key_undecryptable: '保存した API キーを読み出せません。キーを登録し直してください。',
  billing: 'Anthropic の残高または支払い設定を確認してください。',
  model_unavailable: 'このキーでは選んだモデルを使えなくなりました。モデルかキーを変更してください。',
  request_rejected: 'AI が一部のリクエストを受け付けませんでした。モデルを変えるか、時間をおいて様子を見てください。',
};

// aiStatusMessage describes the state of the settings.
export function aiStatusMessage(status: string, lastError: string | null): string {
  if (lastError && Object.hasOwn(statusErrors, lastError)) return statusErrors[lastError] ?? lastError;
  if (status === 'error') return 'エラーで停止しています。キーを更新してください。';
  return '正常';
}

// aiHasProblem reports whether the state needs the user's attention.
export function aiHasProblem(state: AISettings): boolean {
  return state.status === 'error' || state.last_error !== null;
}

// aiErrorMessage describes why saving the settings failed, from the error
// code of the response.
export function aiErrorMessage(code: string | null): string {
  switch (code) {
    case 'ai_key_invalid':
      return 'API キーが無効です。';
    case 'ai_model_unavailable':
      return 'このキーではそのモデルを使えません。';
    case 'ai_billing':
      return 'Anthropic の残高または支払い設定を確認してください。';
    case 'ai_unavailable':
      return 'Anthropic に接続できませんでした。時間をおいて試してください。';
    case 'ai_conflict':
      return '別の画面で設定が変わりました。読み込み直してください。';
    case 'invalid_request':
      return 'API キーの形式が正しくありません。';
  }
  return 'AI の設定を保存できませんでした。接続を確認して、もう一度お試しください。';
}

// aiStatus is the state shown in the phone list of settings pages.
export function aiStatus(state: AIState): string {
  if (!state.configured) return '未設定';
  if (state.status === 'error') return 'AI のキーを確認してください';
  const name = modelName(state.model);
  return state.last_error ? `${name} を使用中（一部の判定に失敗）` : `${name} を使用中`;
}

// aiNeedsSetup reports whether mentions cannot be detected: the AI is not
// configured or stopped with an error.
export function aiNeedsSetup(state: AIState): boolean {
  return !state.configured || state.status === 'error';
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function readAI(data: unknown): AIState {
  if (isRecord(data) && data.configured === false) return { configured: false };
  if (
    isRecord(data) && data.configured === true &&
    typeof data.provider === 'string' && typeof data.model === 'string' && typeof data.key_last4 === 'string' &&
    typeof data.status === 'string' && (data.last_error === null || typeof data.last_error === 'string') &&
    typeof data.updated_at === 'string'
  ) return data as unknown as AISettings;
  throw new Error('Unexpected AI settings response');
}

export function createAIClient(request: RequestFunction) {
  return {
    async getAI(): Promise<AIState> {
      return readAI(await request<unknown>('/api/v1/ai'));
    },
    // apiKey is left out to keep the saved key and change only the model.
    async saveAI(model: string, apiKey?: string): Promise<AIState> {
      const body: { model: string; api_key?: string } = { model };
      if (apiKey !== undefined) body.api_key = apiKey;
      return readAI(await request<unknown>('/api/v1/ai', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) }));
    },
    async deleteAI(): Promise<void> {
      await request<unknown>('/api/v1/ai', { method: 'DELETE' });
    },
  };
}

export type AIClient = ReturnType<typeof createAIClient>;
