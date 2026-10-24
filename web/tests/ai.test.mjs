import assert from 'node:assert/strict';
import { test } from 'node:test';
import { aiErrorMessage, aiHasProblem, aiModels, aiNeedsSetup, aiStatus, aiStatusMessage, createAIClient, DEFAULT_AI_MODEL, modelName, providerName } from '../.test-build/ai.js';

const settings = { configured: true, provider: 'anthropic', model: 'claude-haiku-5-5', key_last4: 'WXYZ', status: 'active', last_error: null, updated_at: '2026-10-25T00:00:00Z' };

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
  await f.client.deleteAI();
  assert.equal(f.calls[2].init.method, 'DELETE');

  for (const bad of [null, {}, { configured: true }, { ...settings, last_error: 1 }]) {
    await assert.rejects(fake(bad).client.getAI());
  }
});
