import { copyFile, mkdir, readFile } from 'node:fs/promises';
import { dirname, resolve, sep } from 'node:path';
import { fileURLToPath } from 'node:url';

const desktopRoot = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const appRoot = resolve(desktopRoot, '../app');
const iconRoot = resolve(appRoot, 'src/assets');
const config = JSON.parse(await readFile(resolve(appRoot, 'emo.json'), 'utf8'));
const iconName = typeof config.brand?.icon === 'string' ? config.brand.icon.trim() : '';
if (!iconName) {
  // No custom brand icon: electron-builder will use the default Electron icon.
  process.exit(0);
}
const icon = resolve(iconRoot, iconName);
if (!icon.startsWith(`${iconRoot}${sep}`) || !icon.endsWith('.png')) {
  throw new Error('emo.json brand.icon must point to a PNG under apps/app/src/assets');
}
const output = resolve(desktopRoot, 'dist/emo-icon.png');
await mkdir(dirname(output), { recursive: true });
await copyFile(icon, output);
