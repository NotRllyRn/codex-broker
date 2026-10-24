import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { runInNewContext } from 'node:vm';
import test from 'node:test';
const source = readFileSync(new URL('../internal/webassets/static/usage.js', import.meta.url), 'utf8');
const { cachedInputShare, rankedModels } = runInNewContext(source.replaceAll('export function', 'function') + '\n({cachedInputShare, rankedModels})', { Intl });

test('cache share uses total input, handles empty data and real zero reuse', () => {
  assert.equal(cachedInputShare({cached_input: 80, uncached_input: 20}), 0.8);
  assert.equal(cachedInputShare({cached_input: 0, uncached_input: 20}), 0);
  assert.equal(cachedInputShare({cached_input: 0, uncached_input: 0}), null);
});

test('models rank by selected metric without mutating data or treating missing costs as zero', () => {
  const models = [
    {name:'Unknown price',cost_known:false,cost_usd:100,processed_tokens:300},
    {name:'Free usage',cost_known:true,cost_usd:0,processed_tokens:100},
    {name:'Priced usage',cost_known:true,cost_usd:2,processed_tokens:200},
  ];
  assert.equal(rankedModels(models, 'cost').map(m=>m.name).join(','), 'Priced usage,Free usage,Unknown price');
  assert.equal(rankedModels(models, 'tokens')[0].name, 'Unknown price');
  assert.equal(models[0].name, 'Unknown price');
});

test('usage defaults to all client identities and model families are independent of agents', () => {
  const template = readFileSync(new URL('../internal/webassets/templates/dashboard.html', import.meta.url), 'utf8');
  assert.match(template, /data-usage-identity[^>]*><option value="all" selected>Client usage/);
  assert.match(template, /data-usage-provider[^>]*aria-label="Model family \(across all providers and agents\)"/);
  assert.match(template, /value="openai" selected>OpenAI \/ Codex models/);
  assert.doesNotMatch(template, /data-usage-device[^>]*disabled/);
});
