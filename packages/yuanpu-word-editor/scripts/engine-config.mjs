import { readFile } from 'node:fs/promises';
import { homedir } from 'node:os';
import { join } from 'node:path';

export async function engineConfig() {
  const enginePath = process.env.YUANPU_WORD_ENGINE_PATH;
  const engineSha256 = process.env.YUANPU_WORD_ENGINE_SHA256;
  if (enginePath || engineSha256) {
    if (!enginePath || !engineSha256) throw new Error('Set both YUANPU_WORD_ENGINE_PATH and YUANPU_WORD_ENGINE_SHA256.');
    return { enginePath, engineSha256 };
  }
  const configPath = process.env.YUANPU_WORD_ENGINE_CONFIG ?? join(homedir(), '.yuanpu', 'word-editor', 'engine.json');
  const config = JSON.parse(await readFile(configPath, 'utf8'));
  if (config.platform !== process.platform || config.arch !== process.arch) {
    throw new Error('The configured Word engine is for a different platform or architecture.');
  }
  return { enginePath: config.enginePath, engineSha256: config.engineSha256 };
}
