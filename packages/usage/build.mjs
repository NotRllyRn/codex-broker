import { mkdirSync } from 'node:fs';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const root = fileURLToPath(new URL('../../', import.meta.url));
for (const [platform, goos] of [['linux', 'linux'], ['darwin', 'darwin'], ['win32', 'windows']]) {
  for (const [arch, goarch] of [['x64', 'amd64'], ['arm64', 'arm64']]) {
    const directory = fileURLToPath(new URL(`./native/${platform}-${arch}/`, import.meta.url));
    mkdirSync(directory, { recursive: true });
    const result = spawnSync('go', ['build', '-trimpath', '-ldflags=-s -w', '-o', `${directory}codex-broker-usage${platform === 'win32' ? '.exe' : ''}`, './cmd/codex-broker-usage'], {
      cwd: root, stdio: 'inherit', env: { ...process.env, CGO_ENABLED: '0', GOOS: goos, GOARCH: goarch },
    });
    if (result.error) throw result.error;
    if (result.status !== 0) process.exit(result.status ?? 1);
  }
}
