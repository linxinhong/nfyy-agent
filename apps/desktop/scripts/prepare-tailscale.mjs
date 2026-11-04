// Build the userspace tailscaled + tailscale CLI that the macOS bundle ships under
// Contents/Resources/tailscale. They are compiled from the pinned upstream module;
// the Go checksum database verifies every module, so no prebuilt binary is trusted.
// electron-builder signs and notarizes both Mach-O files with the app.
//
// Other platforms get an empty directory and the Runtime leaves Tailscale off.
// Set YUANPU_TAILSCALE_SKIP=1 to package a macOS build without Tailscale.
import { spawnSync } from 'node:child_process';
import { mkdirSync, rmSync, writeFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

export const TAILSCALE_VERSION = 'v1.104.5';

const desktopRoot = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const output = resolve(desktopRoot, 'dist-tailscale');
rmSync(output, { recursive: true, force: true });
mkdirSync(output, { recursive: true });

if (process.platform !== 'darwin' || process.env.YUANPU_TAILSCALE_SKIP === '1') {
  console.log('[tailscale] not bundled for this target.');
  process.exit(0);
}
const goarch = { arm64: 'arm64', x64: 'amd64' }[process.arch];
if (!goarch) throw new Error(`[tailscale] unsupported macOS architecture ${process.arch}`);

const env = {
  ...process.env,
  GOBIN: output,
  GOOS: 'darwin',
  GOARCH: goarch,
  // Tailscale pins a newer Go than most runners ship; the toolchain download is
  // itself verified through the checksum database.
  GOTOOLCHAIN: 'auto',
  GOFLAGS: '-trimpath',
};
for (const key of ['GONOSUMDB', 'GONOSUMCHECK', 'GOINSECURE', 'GOPRIVATE', 'GONOPROXY']) delete env[key];
delete env.GOSUMDB;

const build = spawnSync('go', ['install', '-ldflags=-s -w',
  `tailscale.com/cmd/tailscaled@${TAILSCALE_VERSION}`, `tailscale.com/cmd/tailscale@${TAILSCALE_VERSION}`],
{ env, stdio: 'inherit' });
if (build.error?.code === 'ENOENT') throw new Error('[tailscale] Go is required to build the bundled tailscaled (or set YUANPU_TAILSCALE_SKIP=1).');
if (build.status !== 0) throw new Error(`[tailscale] go install failed (${build.status}).`);

for (const name of ['tailscaled', 'tailscale']) {
  const version = spawnSync(join(output, name), ['--version'], { encoding: 'utf8' });
  if (version.status !== 0 || !version.stdout.startsWith(TAILSCALE_VERSION.slice(1))) {
    throw new Error(`[tailscale] ${name} --version did not report ${TAILSCALE_VERSION}: ${version.stdout}${version.stderr}`);
  }
}
writeFileSync(join(output, 'VERSION'), `${TAILSCALE_VERSION}\n`);
console.log(`[tailscale] built ${TAILSCALE_VERSION} for darwin-${goarch}`);
