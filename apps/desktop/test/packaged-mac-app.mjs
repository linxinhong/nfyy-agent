import { readdirSync } from 'node:fs';
import { join, resolve } from 'node:path';

export const packagedReleaseRoot = resolve(import.meta.dirname, '../release');

/** Resolve the packaged .app by listing the release dir, so Unicode names match the filesystem. */
export function findPackagedMacApp(arch = process.arch) {
  const dir = join(packagedReleaseRoot, `mac-${arch}`);
  const apps = readdirSync(dir).filter((name) => name.endsWith('.app'));
  if (apps.length !== 1) {
    throw new Error(`Expected one packaged .app in ${dir}, found: ${apps.join(', ') || '(none)'}`);
  }
  return join(dir, apps[0]);
}

export function packagedResourcesDir() {
  if (process.platform === 'darwin') {
    return join(findPackagedMacApp(), 'Contents', 'Resources');
  }
  return join(
    packagedReleaseRoot,
    process.platform === 'win32' ? 'win-unpacked' : 'linux-unpacked',
    'resources',
  );
}

export function packagedMacExecutable() {
  return join(findPackagedMacApp(), 'Contents', 'MacOS', 'nanyi-xiaozhu');
}
