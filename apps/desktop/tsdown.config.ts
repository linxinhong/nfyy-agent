import { defineConfig } from 'tsdown';

export default defineConfig({
  entry: [
    'src/main.ts',
    'src/preload.ts',
    'src/center-portal-preload.ts',
    'src/renderer-security.ts',
    'src/host-event-client.ts',
    'src/notification-host.ts',
    'src/browser-control-service.ts',
    'src/runtime-manager.ts',
    'src/runtime-updater.ts',
    'src/server-center.ts',
    'src/knowledge-center-status.ts',
    'src/knowledge-center-key.ts',
    'src/knowledge-center-portal.ts',
    'src/server-center-credential.ts',
    'src/server-center-endpoint.ts',
    'src/wordbridge-host.ts',
  ],
  format: 'cjs',
  outDir: 'dist',
  outExtensions: () => ({ js: '.cjs' }),
  platform: 'node',
  sourcemap: true,
  deps: {
    alwaysBundle: ['@yuanpu-agent/protocol', '@yuanpu-agent/server-client'],
    neverBundle: ['electron', 'electron-updater'],
  },
});
