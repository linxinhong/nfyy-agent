import { spawn } from 'node:child_process';
import { mkdir } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';
import { join } from 'node:path';

const serverRoot = fileURLToPath(new URL('../', import.meta.url));
const executable = process.platform === 'win32' ? 'yuanpu-server.exe' : 'yuanpu-server';
const commands = {
  build: ['build', '-trimpath', '-o', `dist/${executable}`, './cmd/yuanpu-server'],
  generate: ['generate', './api/...'],
  test: ['test', './...'],
  vet: ['vet', './...'],
};
const command = process.argv[2];
const launch = command === 'dev' || command === 'migrate';
if (!launch && !Object.hasOwn(commands, command)) throw new Error(`Expected build, dev, migrate, generate, test or vet`);
let interrupted = false;
async function run(program, args) {
  return new Promise((resolve) => {
    const child = spawn(program, args, { cwd: serverRoot, stdio: 'inherit' });
    let force;
    const forward = (signal) => {
      interrupted = true;
      child.kill(signal);
      force ??= setTimeout(() => child.kill('SIGKILL'), 12_000);
    };
    const onInterrupt = () => forward('SIGINT');
    const onTerminate = () => forward('SIGTERM');
    process.on('SIGINT', onInterrupt);
    process.on('SIGTERM', onTerminate);
    child.on('error', (error) => console.error(`Command failed. Install Go 1.26 or newer: ${error.message}`));
    child.on('close', (code, signal) => {
      clearTimeout(force);
      process.off('SIGINT', onInterrupt);
      process.off('SIGTERM', onTerminate);
      resolve(typeof code === 'number' && code >= 0 ? code : 1);
    });
  });
}
if (command === 'build' || launch) await mkdir(new URL('../dist/', import.meta.url), { recursive: true });
let code = await run('go', commands[launch ? 'build' : command]);
if (launch && code === 0 && !interrupted) {
  // Launch the binary directly so a signal to this wrapper cannot orphan a go-run child.
  code = await run(join(serverRoot, 'dist', executable), [command === 'dev' ? 'serve' : 'migrate']);
}
process.exitCode = code;
