import assert from 'node:assert/strict';
import { test } from 'node:test';
import { MAX_INSTRUCTION_LENGTH, createAIInstructionsClient, decisionLabel, detectionDifferences, emptyInstructions, importanceLabel, instructionGroups, instructionLength, instructionScopes, instructionTooLong, isReplyScope, readInstructions, readSamples, readTryResult, tryErrorMessage } from '../.test-build/aiInstructions.js';

const scopes = ['common', 'github.detect', 'slack.detect', 'chatwork.detect', 'gmail.detect', 'slack.reply', 'chatwork.reply', 'gmail.reply'];

test('the scopes match the server and every one is shown once', () => {
  assert.deepEqual([...instructionScopes], scopes);
  assert.deepEqual(instructionGroups.flatMap((g) => g.items.map((i) => i.scope)), scopes);
  assert.deepEqual(instructionGroups.map((g) => g.heading), [null, 'タスクの検出', '返信で済んだかの判断']);
  assert.deepEqual(Object.keys(emptyInstructions()), scopes);
});

test('the length is counted in code points without the surrounding white space', () => {
  assert.equal(MAX_INSTRUCTION_LENGTH, 2000);
  assert.equal(instructionLength('  abc \n'), 3);
  assert.equal(instructionLength('𠮷野家'), 3);
  assert.equal(instructionTooLong('𠮷'.repeat(2000)), false);
  assert.equal(instructionTooLong(`  ${'あ'.repeat(2000)}  `), false);
  assert.equal(instructionTooLong('あ'.repeat(2001)), true);
});

test('readInstructions accepts only every scope as a string', () => {
  const full = { ...emptyInstructions(), common: 'a' };
  assert.deepEqual(readInstructions({ instructions: { ...full, extra: 'x' } }), full);
  for (const bad of [null, {}, { instructions: null }, { instructions: [] }, { instructions: { ...full, 'gmail.reply': null } }, { instructions: { ...full, 'slack.detect': 1 } }]) {
    assert.throws(() => readInstructions(bad));
  }
  const missing = { ...full };
  delete missing['chatwork.reply'];
  assert.throws(() => readInstructions({ instructions: missing }));
});

test('the client sends every scope', async () => {
  const calls = [];
  const saved = { ...emptyInstructions(), common: 'a' };
  const client = createAIInstructionsClient(async (path, init) => {
    calls.push({ path, init });
    return { instructions: saved };
  });
  assert.deepEqual(await client.getInstructions(), saved);
  assert.equal(calls[0].path, '/api/v1/ai/instructions');
  assert.deepEqual(await client.saveInstructions(saved), saved);
  assert.equal(calls[1].init.method, 'PUT');
  assert.deepEqual(JSON.parse(calls[1].init.body), { instructions: saved });
});

const detection = { decision: 'task', task: '資料を見る', importance: 'normal', summary: 's', event: null };

test('readSamples accepts the samples with nullable fields', () => {
  const sample = { id: 'a', content: 'c', status: 'open', auto_reason: null, conversation_name: '#general', detected_at: '2026-10-24T01:02:03Z' };
  assert.deepEqual(readSamples({ samples: [sample, { ...sample, id: 'b', conversation_name: null, detected_at: null, auto_reason: 'slack.reply_resolved' }] }).map((s) => s.id), ['a', 'b']);
  assert.deepEqual(readSamples({ samples: [] }), []);
  for (const bad of [null, {}, { samples: null }, { samples: [{ ...sample, id: 1 }] }, { samples: [{ ...sample, detected_at: 1 }] }, { samples: [{ id: 'a' }] }]) {
    assert.throws(() => readSamples(bad));
  }
});

test('readTryResult checks both results of a detection or a reply', () => {
  const event = { title: 'MTG', start: '2026-10-29T12:30', end: null };
  assert.deepEqual(readTryResult({ current: detection, draft: { ...detection, event } }, false), { reply: false, current: detection, draft: { ...detection, event } });
  assert.deepEqual(readTryResult({ current: { done: true }, draft: { done: false } }, true), { reply: true, current: { done: true }, draft: { done: false } });
  for (const bad of [
    null,
    { current: detection },
    { current: detection, draft: { ...detection, task: null } },
    { current: detection, draft: { ...detection, event: { title: 'x', start: 1, end: null } } },
    { current: detection, draft: { ...detection, event: undefined } },
  ]) {
    assert.throws(() => readTryResult(bad, false));
  }
  assert.throws(() => readTryResult({ current: { done: true }, draft: { done: 'no' } }, true));
  assert.throws(() => readTryResult({ current: { done: true }, draft: { done: false } }, false));
  assert.throws(() => readTryResult({ current: detection, draft: detection }, true));
});

test('the differences of two detections', () => {
  assert.deepEqual([...detectionDifferences(detection, detection)], []);
  assert.deepEqual([...detectionDifferences(detection, { ...detection, importance: 'high', event: { title: 'x', start: '2026-10-29T12:30', end: null } })], ['importance', 'event']);
  assert.deepEqual([...detectionDifferences(detection, { ...detection, decision: 'none', task: '', summary: 'other' })], ['decision', 'task', 'summary']);
  // The task of two detections that need nothing is not compared.
  assert.deepEqual([...detectionDifferences({ ...detection, decision: 'none' }, { ...detection, decision: 'none', task: 'x' })], []);
});

test('labels and error messages', () => {
  assert.deepEqual(['task', 'candidate', 'none'].map(decisionLabel), ['タスク', 'タスク候補', '対応不要']);
  assert.deepEqual(['high', 'normal', 'low'].map(importanceLabel), ['高', '中', '低']);
  assert.equal(isReplyScope('slack.reply'), true);
  assert.equal(isReplyScope('slack.detect'), false);
  assert.equal(tryErrorMessage('no_reply'), '自分の返信がまだないため、試せません。');
  assert.equal(tryErrorMessage('source_unavailable'), '元の会話を読み直せませんでした（削除された、または古くて取得できない可能性があります）。');
  assert.equal(tryErrorMessage(null), tryErrorMessage('unknown'));
});

test('the client lists the samples and tries with the draft instructions', async () => {
  const calls = [];
  const client = createAIInstructionsClient(async (path, init) => {
    calls.push({ path, init });
    return path.includes('samples') ? { samples: [] } : { current: { done: true }, draft: { done: true } };
  });
  assert.deepEqual(await client.samples('gmail.reply'), []);
  assert.equal(calls[0].path, '/api/v1/ai/instructions/samples?scope=gmail.reply');
  assert.deepEqual(await client.tryInstruction('gmail.reply', 'item', 'c', 'i'), { reply: true, current: { done: true }, draft: { done: true } });
  assert.equal(calls[1].path, '/api/v1/ai/instructions/try');
  assert.equal(calls[1].init.method, 'POST');
  assert.deepEqual(JSON.parse(calls[1].init.body), { scope: 'gmail.reply', item_id: 'item', common: 'c', instruction: 'i' });
});
