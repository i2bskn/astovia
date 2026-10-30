import assert from 'node:assert/strict';
import { test } from 'node:test';
import { aiErrorMessage, aiHasProblem, aiModels, aiNeedsSetup, aiStatus, aiStatusMessage, createAIClient, DEFAULT_AI_MODEL, endpointKey, MANAGE_USAGE_URL, modelName, normalizeEndpointURL, providerName } from '../.test-build/ai.js';

const settings = { configured: true, provider: 'anthropic', model: 'claude-haiku-5-5', key_last4: 'WXYZ', api_key_set: true, account: null, base_url: null, status: 'active', last_error: null, updated_at: '2026-10-25T00:00:00Z' };
const plan = { ...settings, provider: 'openai', model: 'gpt-5.5', key_last4: '', api_key_set: false, account: 'alice@example.com' };
const openAIKey = { ...settings, provider: 'openai_api', model: 'gpt-5-mini', key_last4: 'ABCD' };
const endpoint = { ...settings, provider: 'openai_compatible', model: 'meta-llama/Llama-3.1-8B-Instruct', key_last4: '', api_key_set: false, base_url: 'http://ollama:11434/v1' };

test('the models match the server, Haiku first', () => {
  assert.deepEqual(aiModels.map((m) => m.value), ['claude-haiku-5-5', 'claude-sonnet-5-5', 'claude-opus-5-5']);
  assert.equal(DEFAULT_AI_MODEL, 'claude-haiku-5-5');
  assert.equal(modelName('claude-opus-5-5'), 'Claude Opus 5.5');
  assert.equal(modelName('toString'), 'toString');
  assert.equal(providerName('anthropic'), 'Anthropic');
});

test('AI messages', () => {
  assert.equal(aiErrorMessage('ai_key_invalid'), 'API キーが無効です。');
  assert.equal(aiErrorMessage('ai_model_unavailable'), 'このキーではそのモデルを使えません。');
  assert.equal(aiErrorMessage('ai_unavailable'), 'Anthropic に接続できませんでした。時間をおいて試してください。');
  assert.equal(aiErrorMessage('ai_billing'), 'Anthropic の残高または支払い設定を確認してください。');
  assert.equal(aiErrorMessage('ai_conflict'), '別の画面で設定が変わりました。読み込み直してください。');
  assert.match(aiErrorMessage(null), /保存できませんでした/);
  assert.equal(aiStatusMessage('active', null), '正常');
  assert.match(aiStatusMessage('error', 'key_invalid'), /無効/);
  assert.match(aiStatusMessage('error', 'key_undecryptable'), /登録し直して/);
  assert.match(aiStatusMessage('error', 'billing'), /残高/);
  assert.match(aiStatusMessage('error', 'model_unavailable'), /モデル/);
  assert.match(aiStatusMessage('active', 'request_rejected'), /一部のリクエスト/);
  assert.match(aiStatusMessage('error', null), /停止/);
  assert.equal(aiHasProblem(settings), false);
  assert.equal(aiHasProblem({ ...settings, last_error: 'request_rejected' }), true);
});

test('the phone list and the GitHub notice', () => {
  assert.equal(aiStatus({ configured: false }), '未設定');
  assert.equal(aiStatus(settings), 'Claude Haiku 5.5 を使用中');
  assert.equal(aiStatus({ ...settings, last_error: 'request_rejected' }), 'Claude Haiku 5.5 を使用中（一部の判定に失敗）');
  assert.equal(aiStatus({ ...settings, status: 'error', last_error: 'key_invalid' }), 'AI のキーを確認してください');
  assert.equal(aiNeedsSetup({ configured: false }), true);
  assert.equal(aiNeedsSetup({ ...settings, status: 'error' }), true);
  assert.equal(aiNeedsSetup({ ...settings, last_error: 'request_rejected' }), false);
});

function fake(response) {
  const calls = [];
  const client = createAIClient(async (path, init) => {
    calls.push({ path, init });
    return response;
  });
  return { client, calls };
}

test('AI settings client', async () => {
  let f = fake({ configured: false });
  assert.deepEqual(await f.client.getAI(), { configured: false });
  assert.equal(f.calls[0].path, '/api/v1/ai');

  f = fake(settings);
  assert.deepEqual(await f.client.saveAI('claude-haiku-5-5', 'sk-ant-key'), settings);
  assert.equal(f.calls[0].init.method, 'PUT');
  assert.deepEqual(JSON.parse(f.calls[0].init.body), { model: 'claude-haiku-5-5', api_key: 'sk-ant-key' });
  await f.client.saveAI('claude-opus-5-5');
  assert.deepEqual(JSON.parse(f.calls[1].init.body), { model: 'claude-opus-5-5' });
  f = fake(undefined);
  await f.client.deleteAI();
  assert.equal(f.calls[0].path, '/api/v1/ai');
  assert.equal(f.calls[0].init.method, 'DELETE');

  for (const bad of [null, {}, { configured: true }, { ...settings, last_error: 1 }, { ...settings, account: 1 }, { ...settings, account: undefined }, { ...settings, provider: 'other' },
    { ...settings, api_key_set: undefined }, { ...settings, api_key_set: 'true' }, { ...settings, base_url: undefined }, { ...settings, base_url: 1 }]) {
    await assert.rejects(fake(bad).client.getAI());
  }
});

test('ChatGPT plan messages', () => {
  assert.equal(providerName('openai'), 'ChatGPT のプラン（OpenAI）');
  assert.equal(modelName('gpt-5.5', 'openai'), 'gpt-5.5');
  assert.equal(modelName('claude-opus-5-5', 'openai'), 'claude-opus-5-5');
  assert.equal(MANAGE_USAGE_URL, 'https://chatgpt.com/settings/usage');
  assert.equal(aiStatusMessage('error', 'key_invalid', 'openai'), 'ChatGPT との接続が切れました。つなぎ直してください。');
  assert.equal(aiStatusMessage('error', 'billing', 'openai'), 'この ChatGPT のプランでは使えません。');
  assert.equal(aiStatusMessage('active', 'usage_limit', 'openai'), 'ChatGPT のプランの利用上限に達しています。');
  assert.match(aiStatusMessage('error', 'key_undecryptable', 'openai'), /つなぎ直して/);
  assert.match(aiStatusMessage('error', 'model_unavailable', 'openai'), /プラン/);
  assert.match(aiStatusMessage('error', null, 'openai'), /ChatGPT/);
  assert.equal(aiStatusMessage('active', 'usage_limit'), '正常');
  assert.match(aiStatusMessage('error', 'key_invalid', 'anthropic'), /API キー/);

  assert.equal(aiErrorMessage('ai_key_invalid', 'openai'), 'ChatGPT との接続が切れました。つなぎ直してください。');
  assert.equal(aiErrorMessage('ai_billing', 'openai'), 'この ChatGPT のプランでは使えません。');
  assert.match(aiErrorMessage('ai_unavailable', 'openai'), /OpenAI/);
  assert.match(aiErrorMessage('ai_model_unavailable', 'openai'), /プラン/);
  assert.equal(aiErrorMessage('invalid_request', 'openai'), '入力を確認してください。');
  assert.equal(aiErrorMessage('invalid_request'), 'API キーの形式が正しくありません。');
  assert.match(aiErrorMessage('ai_provider_mismatch'), /削除してから/);
  assert.match(aiErrorMessage('ai_usage_limit', 'openai'), /利用上限/);
  assert.match(aiErrorMessage('ai_unsupported', 'openai'), /応答/);
  assert.equal(aiErrorMessage('openai_callback_invalid', 'openai'), '貼り付けた URL を読み取れませんでした。127.0.0.1 から始まる URL をそのまま貼ってください。');
  assert.equal(aiErrorMessage('openai_state_invalid', 'openai'), '接続の有効期限が切れました。もう一度『ChatGPT で続ける』から始めてください。');
  assert.match(aiErrorMessage('openai_denied', 'openai'), /許可されませんでした/);
  assert.equal(aiErrorMessage('openai_scope_missing', 'openai'), 'ChatGPT のプランを使う許可がありませんでした。');
  assert.match(aiErrorMessage('openai_unavailable', 'openai'), /ChatGPT で続ける/);

  assert.equal(aiStatus(plan), 'gpt-5.5 を使用中');
  assert.equal(aiStatus({ ...plan, last_error: 'usage_limit' }), 'gpt-5.5 を使用中（一部の判定に失敗）');
  assert.equal(aiStatus({ ...plan, status: 'error', last_error: 'key_invalid' }), 'ChatGPT との接続を確認してください');
  assert.equal(aiHasProblem({ ...plan, last_error: 'usage_limit' }), true);
});

test('ChatGPT plan client', async () => {
  let f = fake({ url: 'https://auth.openai.com/api/accounts/authorize?x=1', redirect_uri: 'http://127.0.0.1:1455/auth/callback' });
  assert.deepEqual(await f.client.authorizeOpenAI(), { url: 'https://auth.openai.com/api/accounts/authorize?x=1', redirectUri: 'http://127.0.0.1:1455/auth/callback' });
  assert.equal(f.calls[0].path, '/api/v1/ai/openai/authorize');
  assert.equal(f.calls[0].init.method, 'POST');
  assert.equal(f.calls[0].init.headers['Content-Type'], 'application/json');
  assert.deepEqual(JSON.parse(f.calls[0].init.body), {});
  for (const bad of [null, {}, { url: 'javascript:alert(1)', redirect_uri: 'x' }, { url: 'https://x', redirect_uri: 1 }]) {
    await assert.rejects(fake(bad).client.authorizeOpenAI());
  }

  f = fake(plan);
  assert.deepEqual(await f.client.completeOpenAI('http://127.0.0.1:1455/auth/callback?code=c&state=s'), plan);
  assert.equal(f.calls[0].path, '/api/v1/ai/openai/complete');
  assert.equal(f.calls[0].init.method, 'POST');
  assert.deepEqual(JSON.parse(f.calls[0].init.body), { callback_url: 'http://127.0.0.1:1455/auth/callback?code=c&state=s' });
  await assert.rejects(fake({ configured: false }).client.completeOpenAI('x'));

  f = fake({ models: [{ id: 'gpt-a', name: 'GPT A' }] });
  assert.deepEqual(await f.client.openAIModels(), [{ id: 'gpt-a', name: 'GPT A' }]);
  assert.equal(f.calls[0].path, '/api/v1/ai/openai/models');
  for (const bad of [null, {}, { models: [{ id: 1, name: 'x' }] }, { models: [null] }]) {
    await assert.rejects(fake(bad).client.openAIModels());
  }

  f = fake(plan);
  await f.client.saveAI('gpt-b', undefined, 'openai');
  assert.deepEqual(JSON.parse(f.calls[0].init.body), { model: 'gpt-b', provider: 'openai' });

  assert.deepEqual(await fake(undefined).client.deleteAI(), { revokeFailed: false });
  assert.deepEqual(await fake({ revoke_failed: true }).client.deleteAI(), { revokeFailed: true });
  await assert.rejects(fake({}).client.deleteAI());
});

test('OpenAI API key and OpenAI-compatible endpoint messages', () => {
  assert.equal(providerName('openai_api'), 'OpenAI の API キー');
  assert.equal(providerName('openai_compatible'), 'OpenAI 互換の接続先');
  assert.equal(modelName('gpt-5-mini', 'openai_api'), 'gpt-5-mini');
  assert.equal(modelName('claude-opus-5-5', 'openai_compatible'), 'claude-opus-5-5');

  // Each provider has its own messages; Anthropic and ChatGPT keep theirs.
  assert.equal(aiErrorMessage('ai_key_invalid', 'anthropic'), 'API キーが無効です。');
  assert.equal(aiErrorMessage('ai_key_invalid', 'openai'), 'ChatGPT との接続が切れました。つなぎ直してください。');
  assert.equal(aiErrorMessage('ai_key_invalid', 'openai_api'), 'API キーが無効です。');
  assert.equal(aiErrorMessage('ai_key_invalid', 'openai_compatible'), 'API キーが無効です。');
  assert.equal(aiErrorMessage('ai_unavailable', 'anthropic'), 'Anthropic に接続できませんでした。時間をおいて試してください。');
  assert.equal(aiErrorMessage('ai_unavailable', 'openai'), 'OpenAI に接続できませんでした。時間をおいて試してください。');
  assert.equal(aiErrorMessage('ai_unavailable', 'openai_api'), 'OpenAI に接続できませんでした。時間をおいて試してください。');
  assert.equal(aiErrorMessage('ai_unavailable', 'openai_compatible'), '接続先に接続できませんでした。URL を確認するか、時間をおいて試してください。');
  assert.equal(aiErrorMessage('ai_billing', 'openai_api'), '提供元の残高または支払い設定を確認してください。');
  assert.equal(aiErrorMessage('ai_billing', 'openai_compatible'), '提供元の残高または支払い設定を確認してください。');
  assert.equal(aiErrorMessage('ai_model_unavailable', 'openai_api'), 'このキーではそのモデルを使えません。');
  assert.equal(aiErrorMessage('ai_model_unavailable', 'openai_compatible'), '接続先の URL かモデル名が見つかりません。URL（/v1 などのパスを含むか）とモデル名を確認してください。');
  assert.equal(aiErrorMessage('ai_unsupported', 'openai_compatible'), 'この接続先は、Astovia が使う形式（JSON スキーマの構造化出力）に対応していません。');
  assert.equal(aiErrorMessage('ai_unsupported', 'openai'), 'この ChatGPT のプランでは、Astovia が使う形式の応答を受け取れませんでした。');
  assert.equal(aiErrorMessage('ai_endpoint_blocked', 'openai_compatible'), 'この URL は使えません。公開されている HTTPS の URL を指定してください（社内やローカルの接続先は、サーバーの設定で許可が必要です）。');
  assert.equal(aiErrorMessage('invalid_request', 'openai_api'), '入力を確認してください。');
  assert.equal(aiErrorMessage('invalid_request', 'openai_compatible'), '入力を確認してください。');
  assert.equal(aiErrorMessage('rate_limited', 'openai_compatible'), '試す回数が多すぎます。1分ほど待ってから試してください。');
  assert.match(aiErrorMessage('ai_provider_mismatch', 'openai_api'), /削除してから/);
  assert.match(aiErrorMessage(null, 'openai_compatible'), /保存できませんでした/);

  assert.equal(aiStatusMessage('error', null, 'anthropic'), 'エラーで停止しています。キーを更新してください。');
  assert.equal(aiStatusMessage('error', null, 'openai'), 'エラーで停止しています。ChatGPT とつなぎ直してください。');
  assert.equal(aiStatusMessage('error', null, 'openai_api'), 'エラーで停止しています。設定を更新してください。');
  assert.equal(aiStatusMessage('error', null, 'openai_compatible'), 'エラーで停止しています。設定を更新してください。');
  assert.equal(aiStatusMessage('error', 'key_invalid', 'openai_api'), 'API キーが無効になりました。設定を更新してください。');
  assert.equal(aiStatusMessage('error', 'key_invalid', 'openai_compatible'), 'API キーか接続先の URL が使えなくなりました。設定を更新してください。');
  assert.equal(aiStatusMessage('error', 'billing', 'openai_compatible'), '提供元の残高または支払い設定を確認してください。');
  assert.match(aiStatusMessage('error', 'key_undecryptable', 'openai_compatible'), /登録し直して/);
  assert.match(aiStatusMessage('error', 'model_unavailable', 'openai_compatible'), /モデル名/);
  assert.match(aiStatusMessage('active', 'request_rejected', 'openai_compatible'), /一部のリクエスト/);
  assert.equal(aiStatusMessage('active', null, 'openai_api'), '正常');
  for (const provider of ['openai_api', 'openai_compatible']) {
    assert.doesNotMatch(['key_invalid', 'key_undecryptable', 'billing', 'model_unavailable', 'request_rejected'].map((e) => aiStatusMessage('error', e, provider)).join(), /Anthropic/);
  }

  assert.equal(aiStatus(openAIKey), 'gpt-5-mini を使用中');
  assert.equal(aiStatus(endpoint), 'meta-llama/Llama-3.1-8B-Instruct を使用中');
  assert.equal(aiStatus({ ...settings, status: 'error' }), 'AI のキーを確認してください');
  assert.equal(aiStatus({ ...plan, status: 'error' }), 'ChatGPT との接続を確認してください');
  assert.equal(aiStatus({ ...openAIKey, status: 'error' }), 'AI の設定を確認してください');
  assert.equal(aiStatus({ ...endpoint, status: 'error' }), 'AI の設定を確認してください');
});

test('endpoint URL and key of a save', () => {
  assert.equal(normalizeEndpointURL(' https://example.com/v1/ '), 'https://example.com/v1');
  assert.equal(normalizeEndpointURL('https://example.com///'), 'https://example.com');
  assert.equal(normalizeEndpointURL('https://example.com/v1'), 'https://example.com/v1');
  assert.equal(normalizeEndpointURL(''), '');

  // The OpenAI API keeps the saved key when the field is empty.
  assert.equal(endpointKey('openai_api', ' sk-key ', false, false), 'sk-key');
  assert.equal(endpointKey('openai_api', '  ', false, false), undefined);
  assert.equal(endpointKey('openai_api', '', true, true), undefined);
  // An endpoint whose URL is new or changed sends the field as it is.
  assert.equal(endpointKey('openai_compatible', '', true, false), '');
  assert.equal(endpointKey('openai_compatible', ' k ', true, false), 'k');
  // With the same URL, an empty field keeps the key unless it is removed.
  assert.equal(endpointKey('openai_compatible', '', false, false), undefined);
  assert.equal(endpointKey('openai_compatible', '', false, true), '');
  assert.equal(endpointKey('openai_compatible', 'k', false, true), 'k');
});

test('OpenAI API key and OpenAI-compatible endpoint client', async () => {
  let f = fake(openAIKey);
  assert.deepEqual(await f.client.getAI(), openAIKey);
  await f.client.saveAI('gpt-5-mini', 'sk-key', 'openai_api');
  assert.deepEqual(JSON.parse(f.calls[1].init.body), { model: 'gpt-5-mini', api_key: 'sk-key', provider: 'openai_api' });
  await f.client.saveAI('gpt-5', undefined, 'openai_api');
  assert.deepEqual(JSON.parse(f.calls[2].init.body), { model: 'gpt-5', provider: 'openai_api' });

  f = fake(endpoint);
  assert.deepEqual(await f.client.saveAI('llama3', '', 'openai_compatible', 'http://ollama:11434/v1'), endpoint);
  assert.deepEqual(JSON.parse(f.calls[0].init.body), { model: 'llama3', api_key: '', provider: 'openai_compatible', base_url: 'http://ollama:11434/v1' });
  await f.client.saveAI('llama3', undefined, 'openai_compatible', 'http://ollama:11434/v1');
  assert.deepEqual(JSON.parse(f.calls[1].init.body), { model: 'llama3', provider: 'openai_compatible', base_url: 'http://ollama:11434/v1' });
  await f.client.saveAI('llama3', 'k', 'openai_compatible');
  assert.deepEqual(JSON.parse(f.calls[2].init.body), { model: 'llama3', api_key: 'k', provider: 'openai_compatible' });
});
