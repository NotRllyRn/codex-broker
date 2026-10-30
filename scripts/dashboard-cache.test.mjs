import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { runInNewContext } from 'node:vm';
import test from 'node:test';

const source = readFileSync(new URL('../internal/webassets/static/cache.js', import.meta.url), 'utf8');
function fixture({ storageFails = false } = {}) {
  let now = 1000, calls = [], replies = [], stored;
  const api = runInNewContext(source.replaceAll('export ', '') + '\n({configureCache, cachedRequest, clearCache, invalidateCache})', {
    Date: { now: () => now },
    sessionStorage: {
      getItem: () => { if (storageFails) throw Error('disabled'); return stored; },
      setItem: (_, value) => { if (storageFails) throw Error('full'); stored = value; },
      removeItem: () => { stored = undefined; },
    },
    fetch: async (url, options) => { calls.push({url, ...options}); return replies.shift(); },
  });
  api.configureCache('session-one');
  return {
    ...api, calls, advance: () => { now += 6000; }, saved: () => stored,
    reply: (status = 200, data = {value: 1}, remaining) => replies.push({status, ok: status === 200, headers: new Headers({ETag: '"v1"', ...(remaining === undefined ? {} : {'X-Dashboard-TTL': String(remaining)})}), json: async () => data}),
  };
}

test('reload reuses fresh tab cache, then revalidates unchanged data without a body', async () => {
  const f = fixture(); f.reply();
  await f.cachedRequest('/data');
  f.configureCache('session-one');
  assert.equal((await f.cachedRequest('/data')).value, 1);
  assert.equal(f.calls.length, 1);
  f.advance(); f.reply(304);
  assert.equal((await f.cachedRequest('/data')).value, 1);
  assert.equal(f.calls[1].headers['If-None-Match'], '"v1"');
  f.reply(200, {value: 2});
  assert.equal((await f.cachedRequest('/data', {force: true})).value, 2);
  assert.equal(f.calls[2].headers['X-Dashboard-Refresh'], 'true');
});

test('sessions, state invalidation and authentication failure cannot reuse stale data', async () => {
  const f = fixture(); f.reply(); await f.cachedRequest('/data');
  f.configureCache('session-two'); f.reply(200, {value: 2});
  assert.equal((await f.cachedRequest('/data')).value, 2);
  assert.equal(f.calls[1].headers['If-None-Match'], undefined);
  f.invalidateCache(); f.reply(401);
  await assert.rejects(f.cachedRequest('/data'), /session expired/);
  assert.equal(f.saved(), undefined);
});

test('cache stays bounded and tolerates disabled browser storage', async () => {
  const f = fixture();
  for (let i = 0; i < 40; i++) { f.reply(); await f.cachedRequest(`/data/${i}`); }
  assert.equal(Object.keys(JSON.parse(f.saved()).entries).length, 32);
  const disabled = fixture({storageFails: true}); disabled.reply();
  assert.equal((await disabled.cachedRequest('/data')).value, 1);
});

test('browser cache does not extend the lifetime of an old server snapshot', async () => {
  const f = fixture(); f.reply(200, {value: 1}, 0);
  await f.cachedRequest('/data');
  f.reply(200, {value: 2});
  assert.equal((await f.cachedRequest('/data')).value, 2);
  assert.equal(f.calls.length, 2);
});
