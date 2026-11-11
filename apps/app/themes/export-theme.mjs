import { mkdir, readFile, writeFile } from 'node:fs/promises';
import { homedir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const themeRoot = dirname(fileURLToPath(import.meta.url));
const outputRoot = resolve(process.argv[2] ?? join(homedir(), '.yuanpu', 'themes', 'mindlink-test'));
await mkdir(outputRoot, { recursive: true });
const stylesheet = await readFile(join(themeRoot, 'mindlink.css'), 'utf8');
await Promise.all([
  writeFile(join(outputRoot, 'tokens.css'), await readFile(join(themeRoot, 'tokens.css'))),
  writeFile(join(outputRoot, 'theme.css'), stylesheet),
  writeFile(join(outputRoot, 'theme.json'), `${JSON.stringify({
    id: 'mindlink-test',
    name: 'MindLink（测试）',
    version: 1,
    temporary: true,
    base: 'tokens.css',
    stylesheet: 'theme.css',
    selector: "data-yuanpu-theme='mindlink'",
  }, null, 2)}\n`),
]);
console.log(outputRoot);
