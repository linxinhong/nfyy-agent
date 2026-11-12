const { cpSync, existsSync, mkdirSync, readdirSync } = require('node:fs');
const { join } = require('node:path');

function resourceDir(context) {
  if (context.electronPlatformName === 'darwin') {
    const product = context.packager.appInfo.productFilename;
    return join(context.appOutDir, `${product}.app`, 'Contents', 'Resources');
  }
  return join(context.appOutDir, 'resources');
}

function copyIfMissing(src, dest) {
  if (existsSync(dest)) return;
  if (!existsSync(src)) {
    throw new Error(`Pack source missing: ${src}`);
  }
  mkdirSync(dest, { recursive: true });
  cpSync(src, dest, { recursive: true });
}

module.exports = async function afterPack(context) {
  const resources = resourceDir(context);
  const projectDir = context.packager.projectDir;

  const runtimeSrc = join(projectDir, '../runtime/dist-native/bin');
  const runtimeDest = join(resources, 'runtime');
  if (!existsSync(runtimeDest) || !readdirSync(runtimeDest).some((name) => name.startsWith('YuanpuAgentRuntime-'))) {
    mkdirSync(runtimeDest, { recursive: true });
    if (!existsSync(runtimeSrc)) {
      throw new Error(`Runtime binaries missing at ${runtimeSrc}`);
    }
    for (const name of readdirSync(runtimeSrc)) {
      if (name.startsWith('YuanpuAgentRuntime-')) {
        cpSync(join(runtimeSrc, name), join(runtimeDest, name));
      }
    }
  }

  copyIfMissing(
    join(projectDir, '../python-capabilities/dist-artifact/bundle'),
    join(resources, 'capabilities', 'builtin.python.echo'),
  );
  copyIfMissing(join(projectDir, '../app/dist'), join(resources, 'app'));

  const runtimes = existsSync(runtimeDest)
    ? readdirSync(runtimeDest).filter((name) => name.startsWith('YuanpuAgentRuntime-'))
    : [];
  if (runtimes.length === 0) {
    throw new Error(`Packaged Runtime missing under ${runtimeDest}`);
  }
  console.log(`[after-pack] resources ready at ${resources}: runtime=${runtimes.join(',')}`);
};
