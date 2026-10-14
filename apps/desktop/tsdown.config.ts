import { defineConfig } from 'tsdown';

export default defineConfig({
  entry: [
    'src/main.ts',
    'src/preload.ts',
    'src/renderer-security.ts',
    'src/host-event-client.ts',
    'src/notification-host.ts',
    'src/browser-control-service.ts',
    'src/runtime-manager.ts',
    'src/runtime-updater.ts',
    'src/server-center.ts',
    'src/server-center-credential.ts',
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
