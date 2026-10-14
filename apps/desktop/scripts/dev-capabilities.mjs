import { access } from 'node:fs/promises';
import { dirname, join, resolve } from 'node:path';

export async function resolveDevCapabilityEnvironment(root, env, buildArtifacts) {
  const artifactRoot = resolve(root, env.YUANPU_ARTIFACT_ROOT
    || 'apps/python-capabilities/dist-artifact');
  const trustRootFile = resolve(root, env.YUANPU_CAPABILITY_TRUST_ROOT_FILE
    || join(artifactRoot, 'bundle', 'trust-root.json'));
  const requiredFiles = [
    join(artifactRoot, 'manifest.json'),
    join(dirname(trustRootFile), 'manifest.json'),
    trustRootFile,
  ];
  const checkFiles = () => Promise.all(requiredFiles.map((path) => access(path)));
  try {
    await checkFiles();
  } catch (error) {
    if (error.code !== 'ENOENT') throw error;
    if (env.YUANPU_ARTIFACT_ROOT || env.YUANPU_CAPABILITY_TRUST_ROOT_FILE) {
      throw new Error('Configured development capability manifest or trust root is missing.', { cause: error });
    }
    await buildArtifacts();
    await checkFiles();
  }
  return {
    YUANPU_ARTIFACT_ROOT: artifactRoot,
    YUANPU_CAPABILITY_TRUST_ROOT_FILE: trustRootFile,
  };
}
