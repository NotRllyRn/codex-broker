import assert from 'node:assert/strict';
import { test } from 'node:test';
import { nativePath, run } from '../cli.mjs';

test('selects all six native targets and rejects unsupported devices', () => {
  for (const platform of ['linux', 'darwin', 'win32']) {
    for (const arch of ['x64', 'arm64']) {
      assert.ok(nativePath(platform, arch).includes(`${platform}-${arch}`));
      assert.equal(nativePath(platform, arch).endsWith('.exe'), platform === 'win32');
    }
  }
  assert.throws(() => nativePath('freebsd', 'x64'), /Unsupported platform/);
  assert.throws(() => nativePath('linux', 'ia32'), /Unsupported platform/);
});

test('forwards arguments literally and keeps the private prompt attached to the terminal', () => {
  const args = ['install', '--name', 'Work laptop', '--ca', '/path with spaces/ca.pem'];
  assert.equal(run(args, (binary, received, options) => {
    assert.equal(binary, nativePath());
    assert.deepEqual(received, args);
    assert.deepEqual(options, { stdio: 'inherit' });
    return { status: 7 };
  }), 7);
});

test('reports launch failures and interrupted children', () => {
  assert.throws(() => run([], () => ({ error: new Error('missing binary') })), /missing binary/);
  assert.equal(run([], () => ({ status: null, signal: 'SIGTERM' })), 1);
});
