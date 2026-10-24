import assert from 'node:assert/strict';
import { test } from 'node:test';
import { phrases } from '../.test-build/phrases.js';

test('phrases keep a word with the hiragana after it', () => {
  assert.equal(phrases('間違っていたら未完了に戻してください。').join('|'), '間違っていたら|未完了に|戻してください。');
  assert.equal(phrases('Issue がクローズされたので、自動で完了にしました。').join('|'), 'Issue が|クローズされたので、|自動で|完了にしました。');
});

test('phrases break after punctuation and before an opening bracket', () => {
  assert.equal(phrases('ものを、まとめて確認できます。').join('|'), 'ものを、|まとめて|確認できます。');
  assert.equal(phrases('メニューから「ホーム画面に追加」します').join('|'), 'メニューから|「ホーム画面に|追加」します');
});

test('phrases return the whole text joined back unchanged', () => {
  const text = 'Claude や ChatGPT などの AI クライアントから、MCP で使えます。';
  assert.equal(phrases(text).join(''), text);
  assert.deepEqual(phrases(''), []);
});

test('a short Latin word stays with the word after it', () => {
  assert.equal(phrases('などの AI クライアントから、').join('|'), 'などの |AI クライアントから、');
  assert.equal(phrases('Web Push の購読が切れた').join('|'), 'Web Push の|購読が|切れた');
  assert.equal(phrases('Claude や ChatGPT などの').join('|'), 'Claude や |ChatGPT などの');
});
