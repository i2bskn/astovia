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

// AIProvider is an Anthropic API key, the user's ChatGPT plan (OpenAI), an
// OpenAI API key, or an OpenAI-compatible endpoint.
export type AIProvider = 'anthropic' | 'openai' | 'openai_api' | 'openai_compatible';

const providers: readonly AIProvider[] = ['anthropic', 'openai', 'openai_api', 'openai_compatible'];

// AISettings are the saved settings. key_last4 identifies an API key ("" for
// ChatGPT or a short key); api_key_set tells whether a key is saved;
// account is the ChatGPT account's address, or null; base_url is the URL of
// an OpenAI-compatible endpoint, or null.
export interface AISettings {
  configured: true;
  provider: AIProvider;
  model: string;
  key_last4: string;
  api_key_set: boolean;
  account: string | null;
  base_url: string | null;
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
// it is unknown. The models of the other providers are shown as they are.
export function modelName(model: string, provider: AIProvider = 'anthropic'): string {
  if (provider !== 'anthropic') return model;
  return Object.hasOwn(modelNames, model) ? modelNames[model] ?? model : model;
}

export function providerName(provider: string): string {
  switch (provider) {
    case 'anthropic':
      return 'Anthropic';
    case 'openai':
      return 'ChatGPT のプラン（OpenAI）';
    case 'openai_api':
      return 'OpenAI の API キー';
    case 'openai_compatible':
      return 'OpenAI 互換の接続先';
  }
  return provider;
}

// normalizeEndpointURL is the URL of an endpoint as the server keeps it:
// without surrounding spaces and trailing slashes.
export function normalizeEndpointURL(value: string): string {
  return value.trim().replace(/\/+$/, '');
}

// endpointKey is the api_key to send when saving an OpenAI API key or an
// OpenAI-compatible endpoint, or undefined to keep the saved key. key is
// the field's value. An OpenAI API key is kept when the field is empty. An
// endpoint whose URL is new or changed gets the field as it is (empty means
// no key), so that the saved key is never sent to another URL; with the
// same URL, an empty field keeps the saved key unless removeKey is set.
export function endpointKey(provider: AIProvider, key: string, urlChanged: boolean, removeKey: boolean): string | undefined {
  const value = key.trim();
  if (provider !== 'openai_compatible') return value || undefined;
  if (urlChanged || value) return value;
  return removeKey ? '' : undefined;
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
  openai_api: {
    key_invalid: 'API キーが無効になりました。設定を更新してください。',
    key_undecryptable: '保存した API キーを読み出せません。設定を登録し直してください。',
    billing: '提供元の残高または支払い設定を確認してください。',
    model_unavailable: 'このキーでは選んだモデルを使えなくなりました。モデルかキーを変更してください。',
    request_rejected: 'AI が一部のリクエストを受け付けませんでした。モデルを変えるか、時間をおいて様子を見てください。',
  },
  openai_compatible: {
    key_invalid: 'API キーか接続先の URL が使えなくなりました。設定を更新してください。',
    key_undecryptable: '保存した接続先の設定を読み出せません。設定を登録し直してください。',
    billing: '提供元の残高または支払い設定を確認してください。',
    model_unavailable: '接続先の URL かモデル名が見つからなくなりました。URL とモデル名を確認してください。',
    request_rejected: '接続先が一部のリクエストを受け付けませんでした。モデルを変えるか、時間をおいて様子を見てください。',
  },
};

// stoppedMessages describe settings stopped by an error without a known
// last error.
const stoppedMessages: Readonly<Record<AIProvider, string>> = {
  anthropic: 'エラーで停止しています。キーを更新してください。',
  openai: 'エラーで停止しています。ChatGPT とつなぎ直してください。',
  openai_api: 'エラーで停止しています。設定を更新してください。',
  openai_compatible: 'エラーで停止しています。設定を更新してください。',
};

// aiStatusMessage describes the state of the settings of provider.
export function aiStatusMessage(status: string, lastError: string | null, provider: AIProvider = 'anthropic'): string {
  const messages = statusErrors[provider];
  if (lastError && Object.hasOwn(messages, lastError)) return messages[lastError] ?? lastError;
  if (status === 'error') return stoppedMessages[provider];
  return '正常';
}

// aiHasProblem reports whether the state needs the user's attention.
export function aiHasProblem(state: AISettings): boolean {
  return state.status === 'error' || state.last_error !== null;
}

// providerErrors are the messages of the errors that depend on the
// provider.
const providerErrors: Readonly<Record<AIProvider, Readonly<Record<string, string>>>> = {
  anthropic: {
    ai_key_invalid: 'API キーが無効です。',
    ai_model_unavailable: 'このキーではそのモデルを使えません。',
    ai_billing: 'Anthropic の残高または支払い設定を確認してください。',
    ai_unavailable: 'Anthropic に接続できませんでした。時間をおいて試してください。',
    invalid_request: 'API キーの形式が正しくありません。',
  },
  openai: {
    ai_key_invalid: 'ChatGPT との接続が切れました。つなぎ直してください。',
    ai_model_unavailable: 'このプランではそのモデルを使えません。',
    ai_billing: 'この ChatGPT のプランでは使えません。',
    ai_unavailable: 'OpenAI に接続できませんでした。時間をおいて試してください。',
    invalid_request: '入力を確認してください。',
  },
  openai_api: {
    ai_key_invalid: 'API キーが無効です。',
    ai_model_unavailable: 'このキーではそのモデルを使えません。',
    ai_billing: '提供元の残高または支払い設定を確認してください。',
    ai_unavailable: 'OpenAI に接続できませんでした。時間をおいて試してください。',
    ai_unsupported: 'この接続先は、Astovia が使う形式（JSON スキーマの構造化出力）に対応していません。',
    ai_endpoint_blocked: 'この URL は使えません。公開されている HTTPS の URL を指定してください（社内やローカルの接続先は、サーバーの設定で許可が必要です）。',
    invalid_request: '入力を確認してください。',
  },
  openai_compatible: {
    ai_key_invalid: 'API キーが無効です。',
    ai_model_unavailable: '接続先の URL かモデル名が見つかりません。URL（/v1 などのパスを含むか）とモデル名を確認してください。',
    ai_billing: '提供元の残高または支払い設定を確認してください。',
    ai_unavailable: '接続先に接続できませんでした。URL を確認するか、時間をおいて試してください。',
    ai_unsupported: 'この接続先は、Astovia が使う形式（JSON スキーマの構造化出力）に対応していません。',
    ai_endpoint_blocked: 'この URL は使えません。公開されている HTTPS の URL を指定してください（社内やローカルの接続先は、サーバーの設定で許可が必要です）。',
    invalid_request: '入力を確認してください。',
  },
};

// aiErrorMessage describes why saving the settings (or connecting ChatGPT)
// failed, from the error code of the response and the provider in use.
export function aiErrorMessage(code: string | null, provider: AIProvider = 'anthropic'): string {
  const messages = providerErrors[provider];
  if (code !== null && Object.hasOwn(messages, code)) return messages[code] ?? code;
  switch (code) {
    case 'rate_limited':
      return '試す回数が多すぎます。1分ほど待ってから試してください。';
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
  }
  return 'AI の設定を保存できませんでした。接続を確認して、もう一度お試しください。';
}

// stoppedStatus is the phone list state of settings stopped by an error.
const stoppedStatus: Readonly<Record<AIProvider, string>> = {
  anthropic: 'AI のキーを確認してください',
  openai: 'ChatGPT との接続を確認してください',
  openai_api: 'AI の設定を確認してください',
  openai_compatible: 'AI の設定を確認してください',
};

// aiStatus is the state shown in the phone list of settings pages. The
// provider is the one of state.
export function aiStatus(state: AIState): string {
  if (!state.configured) return '未設定';
  if (state.status === 'error') return stoppedStatus[state.provider];
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
    providers.includes(data.provider as AIProvider) && typeof data.model === 'string' && typeof data.key_last4 === 'string' &&
    typeof data.api_key_set === 'boolean' && (data.account === null || typeof data.account === 'string') &&
    (data.base_url === null || typeof data.base_url === 'string') &&
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
    // and change only the model; "" removes the key of an OpenAI-compatible
    // endpoint. provider is left out for Anthropic, and baseUrl is only for
    // an OpenAI-compatible endpoint (left out to keep the saved URL).
    async saveAI(model: string, apiKey?: string, provider?: AIProvider, baseUrl?: string): Promise<AIState> {
      const body: { model: string; api_key?: string; provider?: AIProvider; base_url?: string } = { model };
      if (apiKey !== undefined) body.api_key = apiKey;
      if (provider !== undefined) body.provider = provider;
      if (baseUrl !== undefined) body.base_url = baseUrl;
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
