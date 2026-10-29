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

// MANAGE_USAGE_URL opens the usage page of ChatGPT, where the user manages
// what the ChatGPT plan is used for.
export const MANAGE_USAGE_URL = 'https://chatgpt.com/settings/usage';

// AIProvider is an Anthropic API key, or the user's ChatGPT plan (OpenAI).
export type AIProvider = 'anthropic' | 'openai';

// AISettings are the saved settings. key_last4 identifies an Anthropic key
// ("" for ChatGPT); account is the ChatGPT account's address, or null.
export interface AISettings {
  configured: true;
  provider: AIProvider;
  model: string;
  key_last4: string;
  account: string | null;
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
// it is unknown. A ChatGPT model is shown as its slug.
export function modelName(model: string, provider: AIProvider = 'anthropic'): string {
  if (provider === 'openai') return model;
  return Object.hasOwn(modelNames, model) ? modelNames[model] ?? model : model;
}

export function providerName(provider: string): string {
  if (provider === 'anthropic') return 'Anthropic';
  if (provider === 'openai') return 'ChatGPT のプラン（OpenAI）';
  return provider;
}

const statusErrors: Readonly<Record<AIProvider, Readonly<Record<string, string>>>> = {
  anthropic: {
    key_invalid: 'API キーが無効になりました。キーを更新してください。',
    key_undecryptable: '保存した API キーを読み出せません。キーを登録し直してください。',
    billing: 'Anthropic の残高または支払い設定を確認してください。',
    model_unavailable: 'このキーでは選んだモデルを使えなくなりました。モデルかキーを変更してください。',
    request_rejected: 'AI が一部のリクエストを受け付けませんでした。モデルを変えるか、時間をおいて様子を見てください。',
  },
  openai: {
    key_invalid: 'ChatGPT との接続が切れました。つなぎ直してください。',
    key_undecryptable: '保存した ChatGPT との接続を読み出せません。つなぎ直してください。',
    billing: 'この ChatGPT のプランでは使えません。',
    model_unavailable: 'このプランでは選んだモデルを使えなくなりました。モデルを変更してください。',
    request_rejected: 'AI が一部のリクエストを受け付けませんでした。モデルを変えるか、時間をおいて様子を見てください。',
    usage_limit: 'ChatGPT のプランの利用上限に達しています。',
  },
};

// aiStatusMessage describes the state of the settings of provider.
export function aiStatusMessage(status: string, lastError: string | null, provider: AIProvider = 'anthropic'): string {
  const messages = statusErrors[provider];
  if (lastError && Object.hasOwn(messages, lastError)) return messages[lastError] ?? lastError;
  if (status === 'error') return provider === 'openai' ? 'エラーで停止しています。ChatGPT とつなぎ直してください。' : 'エラーで停止しています。キーを更新してください。';
  return '正常';
}

// aiHasProblem reports whether the state needs the user's attention.
export function aiHasProblem(state: AISettings): boolean {
  return state.status === 'error' || state.last_error !== null;
}

// aiErrorMessage describes why saving the settings (or connecting ChatGPT)
// failed, from the error code of the response and the provider in use.
export function aiErrorMessage(code: string | null, provider: AIProvider = 'anthropic'): string {
  const openai = provider === 'openai';
  switch (code) {
    case 'ai_key_invalid':
      return openai ? 'ChatGPT との接続が切れました。つなぎ直してください。' : 'API キーが無効です。';
    case 'ai_model_unavailable':
      return openai ? 'このプランではそのモデルを使えません。' : 'このキーではそのモデルを使えません。';
    case 'ai_billing':
      return openai ? 'この ChatGPT のプランでは使えません。' : 'Anthropic の残高または支払い設定を確認してください。';
    case 'ai_unavailable':
      return openai ? 'OpenAI に接続できませんでした。時間をおいて試してください。' : 'Anthropic に接続できませんでした。時間をおいて試してください。';
    case 'ai_conflict':
      return '別の画面で設定が変わりました。読み込み直してください。';
    case 'ai_provider_mismatch':
      return '別の提供元の設定があります。今の AI の設定を削除してから設定してください。';
    case 'ai_usage_limit':
      return 'ChatGPT のプランの利用上限に達しています。時間をおいて試してください。';
    case 'ai_unsupported':
      return 'この ChatGPT のプランでは、Astovia が使う形式の応答を受け取れませんでした。';
    case 'openai_callback_invalid':
      return '貼り付けた URL を読み取れませんでした。127.0.0.1 から始まる URL をそのまま貼ってください。';
    case 'openai_state_invalid':
      return '接続の有効期限が切れました。もう一度『ChatGPT で続ける』から始めてください。';
    case 'openai_denied':
      return 'ChatGPT で接続が許可されませんでした。';
    case 'openai_scope_missing':
      return 'ChatGPT のプランを使う許可がありませんでした。';
    case 'openai_unavailable':
      return 'ChatGPT との接続を完了できませんでした。時間をおいて、もう一度『ChatGPT で続ける』から始めてください。';
    case 'invalid_request':
      return openai ? '入力を確認してください。' : 'API キーの形式が正しくありません。';
  }
  return 'AI の設定を保存できませんでした。接続を確認して、もう一度お試しください。';
}

// aiStatus is the state shown in the phone list of settings pages. The
// provider is the one of state.
export function aiStatus(state: AIState): string {
  if (!state.configured) return '未設定';
  if (state.status === 'error') return state.provider === 'openai' ? 'ChatGPT との接続を確認してください' : 'AI のキーを確認してください';
  const name = modelName(state.model, state.provider);
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
    (data.provider === 'anthropic' || data.provider === 'openai') && typeof data.model === 'string' && typeof data.key_last4 === 'string' &&
    (data.account === null || typeof data.account === 'string') &&
    typeof data.status === 'string' && (data.last_error === null || typeof data.last_error === 'string') &&
    typeof data.updated_at === 'string'
  ) return data as unknown as AISettings;
  throw new Error('Unexpected AI settings response');
}

// readConnected reads the settings saved by connecting ChatGPT.
function readConnected(data: unknown): AISettings {
  const state = readAI(data);
  if (!state.configured) throw new Error('Unexpected AI settings response');
  return state;
}

// OpenAIAuthorization is where the ChatGPT sign-in starts, and the address
// the browser is sent back to (which the user pastes).
export interface OpenAIAuthorization {
  url: string;
  redirectUri: string;
}

function readAuthorization(data: unknown): OpenAIAuthorization {
  if (isRecord(data) && typeof data.url === 'string' && data.url.startsWith('https://') && typeof data.redirect_uri === 'string') {
    return { url: data.url, redirectUri: data.redirect_uri };
  }
  throw new Error('Unexpected authorization response');
}

// OpenAIModel is a model of the user's ChatGPT plan: id is sent to the
// server, name is shown.
export interface OpenAIModel {
  id: string;
  name: string;
}

function readModels(data: unknown): OpenAIModel[] {
  if (isRecord(data) && Array.isArray(data.models) && data.models.every((m) => isRecord(m) && typeof m.id === 'string' && typeof m.name === 'string')) {
    return (data.models as OpenAIModel[]).map((m) => ({ id: m.id, name: m.name }));
  }
  throw new Error('Unexpected models response');
}

// readDeleted reads the answer of DELETE: nothing (204), or whether the
// ChatGPT connection could not be revoked.
function readDeleted(data: unknown): { revokeFailed: boolean } {
  if (data === undefined) return { revokeFailed: false };
  if (isRecord(data) && typeof data.revoke_failed === 'boolean') return { revokeFailed: data.revoke_failed };
  throw new Error('Unexpected delete response');
}

const json = { 'Content-Type': 'application/json' };

export function createAIClient(request: RequestFunction) {
  return {
    async getAI(): Promise<AIState> {
      return readAI(await request<unknown>('/api/v1/ai'));
    },
    // apiKey is left out to keep the saved key (or the ChatGPT connection)
    // and change only the model; provider is left out for Anthropic.
    async saveAI(model: string, apiKey?: string, provider?: AIProvider): Promise<AIState> {
      const body: { model: string; api_key?: string; provider?: AIProvider } = { model };
      if (apiKey !== undefined) body.api_key = apiKey;
      if (provider !== undefined) body.provider = provider;
      return readAI(await request<unknown>('/api/v1/ai', { method: 'PUT', headers: json, body: JSON.stringify(body) }));
    },
    async deleteAI(): Promise<{ revokeFailed: boolean }> {
      return readDeleted(await request<unknown>('/api/v1/ai', { method: 'DELETE' }));
    },
    // authorizeOpenAI starts connecting ChatGPT.
    async authorizeOpenAI(): Promise<OpenAIAuthorization> {
      return readAuthorization(await request<unknown>('/api/v1/ai/openai/authorize', { method: 'POST', headers: json, body: '{}' }));
    },
    // completeOpenAI finishes it with the address the user pasted.
    async completeOpenAI(callbackUrl: string): Promise<AISettings> {
      return readConnected(await request<unknown>('/api/v1/ai/openai/complete', { method: 'POST', headers: json, body: JSON.stringify({ callback_url: callbackUrl }) }));
    },
    async openAIModels(): Promise<OpenAIModel[]> {
      return readModels(await request<unknown>('/api/v1/ai/openai/models'));
    },
  };
}

export type AIClient = ReturnType<typeof createAIClient>;
