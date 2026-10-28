import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { test } from 'node:test';
import vm from 'node:vm';

// loadWorker runs public/sw.js with a minimal worker scope and returns its
// top-level functions.
async function loadWorker() {
  const source = await readFile(new URL('../public/sw.js', import.meta.url), 'utf8');
  const self = { location: { origin: 'https://astovia.test' }, addEventListener() {} };
  const context = vm.createContext({ self, URL });
  vm.runInContext(source, context);
  return context;
}

test('a notification click reuses windows showing a screen', async () => {
  const { screenWindow } = await loadWorker();
  for (const path of ['/', '/candidates', '/candidates/', '/auto', '/auto/', '/tasks/abc', '/tasks/abc/', '/settings', '/settings/notifications']) {
    assert.equal(screenWindow(`https://astovia.test${path}`), true, path);
  }
  for (const url of ['https://astovia.test/consent?request=x', 'https://astovia.test/tasks', 'https://astovia.test/candidatesx', 'https://other.test/tasks/abc']) {
    assert.equal(screenWindow(url), false, url);
  }
});

test('a notification opens its same-origin path', async () => {
  const { target } = await loadWorker();
  assert.equal(target('/tasks/abc'), '/tasks/abc');
  assert.equal(target('/admin/auto'), '/auto');
  assert.equal(target('https://other.test/tasks/abc'), '/');
  assert.equal(target(1), '/');
});
