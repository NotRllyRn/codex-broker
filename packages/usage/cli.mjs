#!/usr/bin/env node
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { realpathSync } from 'node:fs';

export function nativePath(platform = process.platform, arch = process.arch) {
  if (!['linux', 'darwin', 'win32'].includes(platform) || !['x64', 'arm64'].includes(arch)) {
    throw new Error(`Unsupported platform: ${platform}/${arch}; use Linux, macOS or Windows on x64 or arm64.`);
  }
  return fileURLToPath(new URL(`./native/${platform}-${arch}/codex-broker-usage${platform === 'win32' ? '.exe' : ''}`, import.meta.url));
}

export function run(args, spawn = spawnSync) {
  const result = spawn(nativePath(), args, { stdio: 'inherit' });
  if (result.error) throw result.error;
  return result.status ?? 1;
}

if (process.argv[1] && realpathSync(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    process.exitCode = run(process.argv.slice(2));
  } catch (error) {
    console.error(`Usage sync: ${error.message}`);
    process.exitCode = 1;
  }
}
