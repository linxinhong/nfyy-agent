import { readFile, mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { spawn } from 'node:child_process';
import { createRequire } from 'node:module';

const serverRoot = fileURLToPath(new URL('../', import.meta.url));
const clientRoot = fileURLToPath(new URL('../../packages/yuanpu-server-client/', import.meta.url));
const require = createRequire(join(clientRoot, 'package.json'));
const { default: openapiTS, astToString } = require('openapi-typescript');
const temporary = await mkdtemp(join(tmpdir(), 'yuanpu-openapi-'));
try {
  const generatedGo = join(temporary, 'types.gen.go');
  await new Promise((resolve, reject) => {
    const child = spawn('go', ['tool', 'oapi-codegen', '-config', 'api/oapi-codegen.yaml', '-o', generatedGo, 'api/openapi.yaml'], { cwd: serverRoot, stdio: 'inherit' });
    child.on('error', reject);
    child.on('exit', (code) => code === 0 ? resolve() : reject(new Error(`Go generation failed: ${code}`)));
  });
  if (await readFile(generatedGo, 'utf8') !== await readFile(join(serverRoot, 'internal/api/types.gen.go'), 'utf8')) {
    throw new Error('Generated Go types are stale; run pnpm generate:server');
  }
  const schema = new URL('../api/openapi.yaml', import.meta.url);
  const generatedTS = astToString(await openapiTS(schema));
  // The CLI adds a fixed banner; compare the generated AST body without mutating source.
  const committedTS = await readFile(join(clientRoot, 'src/schema.gen.ts'), 'utf8');
  if (committedTS.slice(committedTS.indexOf('export interface paths')) !== generatedTS.slice(generatedTS.indexOf('export interface paths'))) {
    throw new Error('Generated TypeScript types are stale; run pnpm generate:server');
  }
  console.log('OpenAPI Go and TypeScript generated types are current.');
} finally {
  await rm(temporary, { recursive: true, force: true });
}
