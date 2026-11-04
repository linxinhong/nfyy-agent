// Assemble the WordBridge payload that the macOS app bundle ships under
// Contents/Resources/wordbridge:
//   python/  portable CPython with the hash-locked dependency closure installed
//   app/     WordBridge sources and the Office add-in assets
//   payload.json
// Every Mach-O file lands inside the .app, so electron-builder signs and
// notarizes it with the app. No wheels or archives are left for later install.
//
// Other platforms (and Intel Macs, which lack wheels for the locked closure)
// get an empty payload directory; the desktop host then leaves WordBridge off.
import { spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { cpSync, createReadStream, existsSync, mkdirSync, readdirSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const desktopRoot = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const sourceRoot = resolve(desktopRoot, '../wordbridge');
const output = resolve(sourceRoot, 'dist');

const PYTHON = {
  version: '3.12.7',
  release: '20241016',
  'darwin-arm64': {
    triple: 'aarch64-apple-darwin',
    sha256: '4c18852bf9c1a11b56f21bcf0df1946f7e98ee43e9e4c0c5374b2b3765cf9508',
    requirements: 'requirements-macos-arm64.txt',
  },
};

const target = `${process.platform}-${process.arch}`;
rmSync(output, { recursive: true, force: true });
mkdirSync(output, { recursive: true });

const pin = PYTHON[target];
if (!pin) {
  if (process.platform === 'darwin') {
    console.warn(`[wordbridge] ${target} is not supported; packaging without WordBridge.`);
  }
  writeFileSync(join(output, 'UNSUPPORTED'), `${target}\n`);
  process.exit(0);
}

function run(command, args, options = {}) {
  const result = spawnSync(command, args, { stdio: 'inherit', ...options });
  if (result.error) throw result.error;
  if (result.status !== 0) throw new Error(`${command} ${args.join(' ')} exited with ${result.status}`);
}

async function sha256(path) {
  const hash = createHash('sha256');
  for await (const chunk of createReadStream(path)) hash.update(chunk);
  return hash.digest('hex');
}

async function download(url, destination, expected) {
  if (existsSync(destination) && await sha256(destination) === expected) return;
  mkdirSync(dirname(destination), { recursive: true });
  const response = await fetch(url);
  if (!response.ok) throw new Error(`Download failed (${response.status}): ${url}`);
  const temporary = `${destination}.partial`;
  writeFileSync(temporary, Buffer.from(await response.arrayBuffer()));
  const actual = await sha256(temporary);
  if (actual !== expected) {
    rmSync(temporary, { force: true });
    throw new Error(`Checksum mismatch for ${url}: expected ${expected}, got ${actual}`);
  }
  rmSync(destination, { force: true });
  cpSync(temporary, destination);
  rmSync(temporary, { force: true });
}

const tarball = `cpython-${PYTHON.version}+${PYTHON.release}-${pin.triple}-install_only.tar.gz`;
const url = `https://github.com/astral-sh/python-build-standalone/releases/download/${PYTHON.release}/${encodeURIComponent(tarball)}`;
const cached = join(tmpdir(), 'yuanpu-wordbridge-cache', tarball);
console.log(`[wordbridge] Python ${PYTHON.version} (${target})`);
await download(url, cached, pin.sha256);

const pythonRoot = join(output, 'python');
mkdirSync(pythonRoot, { recursive: true });
run('tar', ['-xzf', cached, '-C', pythonRoot, '--strip-components=1']);
const python = join(pythonRoot, 'bin', 'python3');
const minor = PYTHON.version.split('.').slice(0, 2).join('.');
const stdlib = join(pythonRoot, 'lib', `python${minor}`);

// Install exactly the locked closure: binary wheels only, every hash checked,
// and no resolver step that could pull an unpinned package.
console.log('[wordbridge] Installing hash-locked dependencies');
run(python, [
  '-I', '-m', 'pip', 'install',
  '--disable-pip-version-check', '--no-warn-script-location', '--no-compile',
  '--require-hashes', '--only-binary=:all:', '--no-deps',
  '-r', join(sourceRoot, pin.requirements),
]);
run(python, ['-I', '-m', 'pip', 'check', '--disable-pip-version-check']);

// Drop parts the bridge never loads: GUI toolkits, tests, headers, static libs,
// and bundled wheels (nested archives only complicate notarization).
for (const relative of [
  'include', 'share',
  `lib/python${minor}/test`, `lib/python${minor}/idlelib`, `lib/python${minor}/tkinter`,
  `lib/python${minor}/turtledemo`, `lib/python${minor}/ensurepip`,
  `lib/python${minor}/config-${minor}-darwin`,
]) rmSync(join(pythonRoot, relative), { recursive: true, force: true });
for (const name of readdirSync(join(pythonRoot, 'lib'))) {
  if (/^(tcl|tk|itcl|thread)\d|^lib(tcl|tk)/.test(name)) rmSync(join(pythonRoot, 'lib', name), { recursive: true, force: true });
}
for (const name of readdirSync(join(stdlib, 'lib-dynload'))) {
  if (name.startsWith('_tkinter')) rmSync(join(stdlib, 'lib-dynload', name), { force: true });
}

const appRoot = join(output, 'app');
mkdirSync(appRoot, { recursive: true });
for (const entry of ['wordbridge', 'addin', 'run_bridge.py']) {
  cpSync(join(sourceRoot, entry), join(appRoot, entry), {
    recursive: true,
    filter: (path) => !/(^|[\\/])(__pycache__|\.env|certs)([\\/]|$)|\.(pyc|log|db)$/.test(path),
  });
}

// The bundle is read-only once signed, so precompile everything and never let
// Python check source timestamps or write bytecode at runtime (it runs with -B).
console.log('[wordbridge] Precompiling bytecode');
run(python, ['-I', '-m', 'compileall', '-q', '-j', '0', '--invalidation-mode', 'unchecked-hash', appRoot]);
spawnSync(python, ['-I', '-m', 'compileall', '-q', '-j', '0', '--invalidation-mode', 'unchecked-hash', stdlib], {
  stdio: 'inherit',
});

// Import smoke on the build machine: fails the package step instead of the user's first launch.
run(python, ['-I', '-B', '-c', [
  'import sys',
  `sys.path.insert(0, ${JSON.stringify(appRoot)})`,
  'import wordbridge.ws_server, wordbridge.mcp_server, wordbridge.addin_server, uvicorn, fastmcp',
  'print("[wordbridge] import smoke ok")',
].join('; ')]);

writeFileSync(join(output, 'payload.json'), `${JSON.stringify({
  target,
  python: PYTHON.version,
  executable: 'python/bin/python3',
  entry: 'app/run_bridge.py',
}, null, 2)}\n`);
console.log(`[wordbridge] Payload ready at ${output}`);
