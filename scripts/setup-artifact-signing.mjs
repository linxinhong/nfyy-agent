#!/usr/bin/env node
// Generate the production Python capability signing key pair and store the
// four YUANPU_ARTIFACT_* GitHub Actions secrets. Run this on a trusted
// machine with `gh` logged in; the private key never leaves that machine
// except as an encrypted GitHub secret.
//
// Usage: node scripts/setup-artifact-signing.mjs [--key-dir DIR] [--key-id ID] [--repo OWNER/NAME]
import { spawnSync } from 'node:child_process';
import { createPrivateKey, createPublicKey, generateKeyPairSync } from 'node:crypto';
import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { homedir } from 'node:os';
import { join, resolve } from 'node:path';
import { parseArgs } from 'node:util';

const { values } = parseArgs({
  options: {
    'key-dir': { type: 'string', default: join(homedir(), '.yuanpu-release-keys') },
    'key-id': { type: 'string', default: `yuanpu-release-${new Date().getUTCFullYear()}` },
    repo: { type: 'string', default: 'linxinhong/yuanpu-agent' },
  },
});

const keyDir = resolve(values['key-dir']);
const privatePath = join(keyDir, 'artifact-signing.pem');
const publicPath = join(keyDir, 'artifact-trust-root.pub.pem');
const baseUrl = `https://github.com/${values.repo}/releases/download`;

if (existsSync(privatePath)) {
  console.log(`Reusing existing private key at ${privatePath}`);
} else {
  mkdirSync(keyDir, { recursive: true, mode: 0o700 });
  const { privateKey, publicKey } = generateKeyPairSync('ed25519');
  writeFileSync(privatePath, privateKey.export({ type: 'pkcs8', format: 'pem' }), { mode: 0o600 });
  writeFileSync(publicPath, publicKey.export({ type: 'spki', format: 'pem' }), { mode: 0o644 });
  console.log(`Generated Ed25519 key pair in ${keyDir}`);
}

const privatePem = readFileSync(privatePath, 'utf8');
const publicPem = createPublicKey(createPrivateKey(privatePem)).export({ type: 'spki', format: 'pem' }).toString();
writeFileSync(publicPath, publicPem, { mode: 0o644 });

const secrets = {
  YUANPU_ARTIFACT_SIGNING_KEY_BASE64: Buffer.from(privatePem).toString('base64'),
  YUANPU_ARTIFACT_TRUST_ROOT_PUBLIC_KEY_BASE64: Buffer.from(publicPem).toString('base64'),
  YUANPU_ARTIFACT_SIGNING_KEY_ID: values['key-id'],
  YUANPU_ARTIFACT_BASE_URL: baseUrl,
};

for (const [name, value] of Object.entries(secrets)) {
  // Pass the value on stdin so it never appears in argv or shell history.
  const result = spawnSync('gh', ['secret', 'set', name, '--repo', values.repo], {
    input: value,
    stdio: ['pipe', 'inherit', 'inherit'],
  });
  if (result.error) throw result.error;
  if (result.status !== 0) process.exit(result.status ?? 1);
}

console.log(`
Stored 4 secrets on ${values.repo} (key id: ${values['key-id']}, base URL: ${baseUrl}).

Next:
  1. Back up ${privatePath} in your password manager.
     Installed apps trust only this key; losing it blocks capability updates.
  2. Then delete the local copy:  rm -r "${keyDir}"
`);
