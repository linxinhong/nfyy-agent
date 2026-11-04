import assert from 'node:assert/strict';
import { existsSync } from 'node:fs';
import { chmod, mkdir, mkdtemp, readdir, readFile, rm, writeFile } from 'node:fs/promises';
import { createServer } from 'node:http';
import { connect as netConnect } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';

import { installTailnetRouting, tailnetMatcher } from '../src/tailnet-routing.ts';
import { keyFingerprint, statusView, TailscaleNode, tailscaleHostname } from '../src/tailscale-node.ts';

const fixtureStatus = (backend) => ({
  BackendState: backend,
  CurrentTailnet: { Name: 'fixture@example.com', MagicDNSSuffix: 'fixture.ts.net' },
  Self: { DNSName: 'mindlink-test.fixture.ts.net.', TailscaleIPs: ['100.100.1.2', 'fd7a:115c:a1e0::2'] },
  Peer: { node: { DNSName: 'gateway.fixture.ts.net.', HostName: 'gateway', PrimaryRoutes: ['192.168.50.0/24', '0.0.0.0/0'] } },
});

test('only tailnet destinations match: tailnet IPs, MagicDNS names, peers and subnet routes', () => {
  const view = statusView(fixtureStatus('Running'));
  const match = tailnetMatcher(view);
  for (const host of ['100.101.2.3', '[fd7a:115c:a1e0::9]', 'gateway', 'gateway.fixture.ts.net', 'GATEWAY.fixture.ts.net.',
    'other.fixture.ts.net', '192.168.50.20']) assert.equal(match(host), true, host);
  // Public hosts, other tailnets' Funnel names and an advertised exit route stay direct.
  for (const host of ['api.openai.com', '8.8.8.8', '192.168.51.1', 'someone.else.ts.net', '127.0.0.1', '[::1]']) {
    assert.equal(match(host), false, host);
  }
});

test('status maps backend states and never needs the key', () => {
  const running = statusView(fixtureStatus('Running')).status;
  assert.deepEqual(running, { state: 'connected', ipv4: '100.100.1.2', dnsName: 'mindlink-test.fixture.ts.net', tailnet: 'fixture@example.com' });
  assert.equal(statusView(fixtureStatus('NeedsLogin')).status.state, 'needs-login');
  assert.equal(statusView(fixtureStatus('NeedsMachineAuth')).status.state, 'needs-login');
  assert.equal(statusView({ BackendState: 'Starting' }).status.state, 'starting');
  assert.equal(tailscaleHostname('Alice’s MacBook Pro.local'), 'mindlink-alice-s-macbook-pro');
  assert.equal(tailscaleHostname('---'), 'mindlink-mac');
});

test('fetch tunnels tailnet hosts through the proxy and leaves other hosts direct', async (t) => {
  const target = createServer((request, response) => response.end(`hello ${request.url}`));
  await new Promise((resolveListen) => target.listen(0, '127.0.0.1', resolveListen));
  const targetPort = target.address().port;
  const tunnelled = [];
  const proxy = createServer((_request, response) => { response.statusCode = 405; response.end(); });
  proxy.on('connect', (request, client, head) => {
    tunnelled.push(request.url);
    const upstream = netConnect(targetPort, '127.0.0.1', () => {
      client.write('HTTP/1.1 200 Connection Established\r\n\r\n');
      if (head.length) upstream.write(head);
      upstream.pipe(client);
      client.pipe(upstream);
    });
    upstream.on('error', () => client.destroy());
    client.on('error', () => upstream.destroy());
  });
  await new Promise((resolveListen) => proxy.listen(0, '127.0.0.1', resolveListen));
  const proxyPort = proxy.address().port;
  let connected = true;
  const uninstall = await installTailnetRouting({
    proxyPortFor: (hostname) => (connected && hostname.endsWith('.fixture.ts.net') ? proxyPort : undefined),
  });
  t.after(async () => { await uninstall(); target.close(); proxy.close(); });

  const viaTailnet = await fetch('http://gateway.fixture.ts.net:8080/v1/models');
  assert.equal(await viaTailnet.text(), 'hello /v1/models');
  assert.deepEqual(tunnelled, ['gateway.fixture.ts.net:8080']);

  const direct = await fetch(`http://127.0.0.1:${targetPort}/direct`);
  assert.equal(await direct.text(), 'hello /direct');
  assert.equal(tunnelled.length, 1);

  // A disconnected node routes nothing, so tailnet names fail like any unknown host.
  connected = false;
  await assert.rejects(fetch('http://gateway.fixture.ts.net:8080/again'));
  assert.equal(tunnelled.length, 1);
});

async function fakeBinaries(root) {
  const bin = join(root, 'bin');
  await mkdir(bin, { recursive: true });
  await writeFile(join(bin, 'tailscaled'), `#!/bin/sh
for a in "$@"; do case "$a" in --socket=*) sock="\${a#--socket=}";; esac; done
echo "$@" > "$(dirname "$0")/daemon-args"
: > "$sock"
trap 'exit 0' TERM
while :; do sleep 0.1; done
`);
  await writeFile(join(bin, 'tailscale'), `#!/bin/sh
dir=$(dirname "$0")
shift
echo "$@" >> "$dir/calls"
case "$1" in
  status) b=$(cat "$dir/backend" 2>/dev/null || echo NeedsLogin)
    printf '%s' '${JSON.stringify(fixtureStatus('__B__'))}' | sed "s/__B__/$b/";;
  up) for a; do case "$a" in --auth-key=file:*) cat "\${a#--auth-key=file:}" > "$dir/seen-key";; esac; done
    echo Running > "$dir/backend";;
  logout) echo NeedsLogin > "$dir/backend";;
esac
`);
  await chmod(join(bin, 'tailscaled'), 0o755);
  await chmod(join(bin, 'tailscale'), 0o755);
  return bin;
}

test('the node is offered only on macOS with both bundled binaries', { skip: process.platform === 'win32' }, async (t) => {
  const root = await mkdtemp(join(tmpdir(), 'ts-env-'));
  t.after(() => rm(root, { recursive: true, force: true }));
  const bin = await fakeBinaries(root);
  const state = join(root, 'state');
  assert.ok(TailscaleNode.fromEnvironment(state, { YUANPU_TAILSCALE_ROOT: bin }, 'darwin'));
  assert.equal(TailscaleNode.fromEnvironment(state, { YUANPU_TAILSCALE_ROOT: bin }, 'linux'), undefined);
  assert.equal(TailscaleNode.fromEnvironment(state, {}, 'darwin'), undefined);
  assert.equal(TailscaleNode.fromEnvironment(state, { YUANPU_TAILSCALE_ROOT: root }, 'darwin'), undefined);
  assert.equal(TailscaleNode.fromEnvironment(state, { YUANPU_TAILSCALE_ROOT: bin, YUANPU_TAILSCALE_DISABLED: '1' }, 'darwin'), undefined);
});

test('applying a key joins once, keeps the key off argv and disk, and logout clears state',
  { skip: process.platform === 'win32' }, async (t) => {
    const root = await mkdtemp(join(tmpdir(), 'ts-node-'));
    const bin = await fakeBinaries(root);
    const stateRoot = join(root, 'state');
    const node = new TailscaleNode({ binaryRoot: bin, stateRoot, hostname: 'mindlink-test', log: () => {} });
    t.after(async () => { await node.stop(); await rm(root, { recursive: true, force: true }); });

    const status = await node.apply('tskey-auth-fixture-1');
    assert.equal(status.state, 'connected', JSON.stringify(status));
    assert.equal(status.ipv4, '100.100.1.2');
    assert.equal(await readFile(join(bin, 'seen-key'), 'utf8'), 'tskey-auth-fixture-1');
    const calls = await readFile(join(bin, 'calls'), 'utf8');
    assert.doesNotMatch(calls, /tskey-auth/);
    assert.match(calls, /up --auth-key=file:\S+ --hostname=mindlink-test --accept-routes/);
    assert.match(await readFile(join(bin, 'daemon-args'), 'utf8'),
      /--tun=userspace-networking .*--outbound-http-proxy-listen=127\.0\.0\.1:\d+ --no-logs-no-support/);
    assert.deepEqual((await readdir(stateRoot)).filter((name) => name.startsWith('.auth-')), []);
    assert.equal((await readFile(join(stateRoot, 'key.sha256'), 'utf8')).trim(), keyFingerprint('tskey-auth-fixture-1'));
    assert.ok(node.proxyPortFor('gateway.fixture.ts.net'));
    assert.equal(node.proxyPortFor('api.openai.com'), undefined);

    // Same key and an already-running node: no second `up`.
    await node.apply('tskey-auth-fixture-1');
    assert.equal((await readFile(join(bin, 'calls'), 'utf8')).match(/^up /gm).length, 1);

    // A new key logs out the old identity first.
    await node.apply('tskey-auth-fixture-2');
    const rotated = await readFile(join(bin, 'calls'), 'utf8');
    assert.equal(rotated.match(/^up /gm).length, 2);
    assert.match(rotated, /^logout$/m);

    const removed = await node.apply(undefined);
    assert.equal(removed.state, 'stopped');
    assert.equal(node.proxyPortFor('gateway.fixture.ts.net'), undefined);
    assert.equal(existsSync(join(stateRoot, 'state')), false);
    assert.equal(existsSync(join(stateRoot, 'key.sha256')), false);
    assert.equal(existsSync(join(stateRoot, 'tailscaled.pid')), false);
  });
