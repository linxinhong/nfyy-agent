import { EventEmitter } from 'node:events';
import { BlockList, connect as netConnect, isIP, type Socket } from 'node:net';
import { connect as tlsConnect } from 'node:tls';

/**
 * Sends only tailnet destinations through the bundled tailscaled's loopback HTTP
 * CONNECT proxy. Everything else keeps Node's own dispatcher, so a stopped or broken
 * tailscaled never affects public model gateways.
 */
export interface TailnetRoute {
  /** Loopback proxy port when the destination belongs to the connected tailnet. */
  proxyPortFor(hostname: string): number | undefined;
}

export interface TailnetPeerView {
  magicDnsSuffix?: string;
  peerNames: string[];
  subnetRoutes: string[];
}

const tailnetV4 = new BlockList();
tailnetV4.addSubnet('100.64.0.0', 10, 'ipv4');
const tailnetV6 = new BlockList();
tailnetV6.addSubnet('fd7a:115c:a1e0::', 48, 'ipv6');

function bare(hostname: string): string {
  return hostname.replace(/^\[|\]$/g, '').replace(/\.$/, '').toLowerCase();
}

/** Builds the destination test from `tailscale status --json` derived facts. */
export function tailnetMatcher(view: TailnetPeerView): (hostname: string) => boolean {
  const subnets = new BlockList();
  for (const route of view.subnetRoutes) {
    const [address, bits] = route.split('/');
    const family = isIP(address ?? '');
    const prefix = Number(bits);
    if (!family || !Number.isInteger(prefix)) continue;
    // A default route would capture all traffic; exit nodes are not used here.
    if (prefix === 0) continue;
    subnets.addSubnet(address!, prefix, family === 4 ? 'ipv4' : 'ipv6');
  }
  const suffix = view.magicDnsSuffix ? bare(view.magicDnsSuffix) : undefined;
  const names = new Set(view.peerNames.map(bare).filter(Boolean));
  return (hostname) => {
    const host = bare(hostname);
    const family = isIP(host);
    if (family === 4) return tailnetV4.check(host, 'ipv4') || subnets.check(host, 'ipv4');
    if (family === 6) return tailnetV6.check(host, 'ipv6') || subnets.check(host, 'ipv6');
    if (names.has(host)) return true;
    return Boolean(suffix && (host === suffix || host.endsWith(`.${suffix}`)));
  };
}

interface ConnectOptions {
  hostname: string;
  protocol: string;
  port: string | number;
  servername?: string | null;
}
type ConnectCallback = (error: Error | null, socket?: Socket) => void;

/** Opens a CONNECT tunnel through tailscaled, then TLS for https origins. */
export function tunnelConnect(proxyPort: number, options: ConnectOptions, callback: ConnectCallback,
  timeoutMs = 15_000): void {
  const host = bare(options.hostname);
  const port = Number(options.port) || (options.protocol === 'https:' ? 443 : 80);
  const authority = `${isIP(host) === 6 ? `[${host}]` : host}:${port}`;
  const socket = netConnect({ host: '127.0.0.1', port: proxyPort });
  let settled = false;
  const fail = (error: Error) => {
    if (settled) return;
    settled = true;
    socket.destroy();
    callback(error);
  };
  socket.setTimeout(timeoutMs, () => fail(new Error(`Tailnet connection to ${authority} timed out.`)));
  socket.once('error', fail);
  socket.once('connect', () => {
    socket.write(`CONNECT ${authority} HTTP/1.1\r\nHost: ${authority}\r\n\r\n`);
  });
  let head = Buffer.alloc(0);
  const onData = (chunk: Buffer) => {
    head = Buffer.concat([head, chunk]);
    const end = head.indexOf('\r\n\r\n');
    if (end < 0) {
      if (head.length > 16_384) fail(new Error('Tailnet proxy sent an oversized response.'));
      return;
    }
    socket.off('data', onData);
    const status = /^HTTP\/1\.[01] (\d{3})/.exec(head.subarray(0, end).toString('latin1'));
    if (status?.[1] !== '200') {
      fail(new Error(`Tailnet proxy refused ${authority} (${status?.[1] ?? 'invalid response'}).`));
      return;
    }
    const rest = head.subarray(end + 4);
    if (rest.length) socket.unshift(rest);
    socket.setTimeout(0);
    if (options.protocol !== 'https:') {
      settled = true;
      socket.off('error', fail);
      callback(null, socket);
      return;
    }
    const servername = options.servername || (isIP(host) ? undefined : host);
    const secure = tlsConnect({ socket, ...(servername ? { servername } : {}), ALPNProtocols: ['http/1.1'] });
    secure.setTimeout(timeoutMs, () => { secure.destroy(); fail(new Error(`Tailnet TLS to ${authority} timed out.`)); });
    secure.once('error', (error) => { secure.destroy(); fail(error); });
    secure.once('secureConnect', () => {
      if (settled) return;
      settled = true;
      secure.setTimeout(0);
      secure.removeAllListeners('error');
      socket.off('error', fail);
      callback(null, secure);
    });
  };
  socket.on('data', onData);
}

interface UndiciDispatcher {
  dispatch(options: { origin?: string | URL }, handler: unknown): boolean;
  close(...args: unknown[]): unknown;
  destroy(...args: unknown[]): unknown;
}

const globalDispatcherKey = Symbol.for('undici.globalDispatcher.1');

/**
 * Installs the selective dispatcher in front of Node's built-in fetch. Returns an
 * uninstall function that restores the previous dispatcher.
 */
export async function installTailnetRouting(route: TailnetRoute): Promise<() => Promise<void>> {
  const store = globalThis as unknown as Record<symbol, UndiciDispatcher | undefined>;
  // An aborted fetch makes Node create its default Agent without touching the network.
  await fetch('http://127.0.0.1:9/', { signal: AbortSignal.abort() }).catch(() => undefined);
  const base = store[globalDispatcherKey];
  if (!base) throw new Error('Node did not expose a global fetch dispatcher.');
  const Agent = base.constructor as new (options: Record<string, unknown>) => UndiciDispatcher;
  const tunnels = new Map<number, UndiciDispatcher>();
  const tunnelFor = (port: number) => {
    let agent = tunnels.get(port);
    if (!agent) {
      agent = new Agent({ connect: (options: ConnectOptions, callback: ConnectCallback) => tunnelConnect(port, options, callback) });
      tunnels.set(port, agent);
    }
    return agent;
  };
  class TailnetDispatcher extends EventEmitter {
    dispatch(options: { origin?: string | URL }, handler: unknown): boolean {
      let port: number | undefined;
      try {
        if (options.origin) port = route.proxyPortFor(new URL(String(options.origin)).hostname);
      } catch { port = undefined; }
      return (port ? tunnelFor(port) : base!).dispatch(options, handler);
    }
    async close(): Promise<void> {
      await Promise.allSettled([...tunnels.values()].map((agent) => agent.close()));
    }
    async destroy(): Promise<void> {
      await Promise.allSettled([...tunnels.values()].map((agent) => agent.destroy()));
    }
  }
  const dispatcher = new TailnetDispatcher();
  store[globalDispatcherKey] = dispatcher as unknown as UndiciDispatcher;
  return async () => {
    if (store[globalDispatcherKey] === (dispatcher as unknown as UndiciDispatcher)) store[globalDispatcherKey] = base;
    await dispatcher.close();
  };
}
