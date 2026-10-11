import { defineConfig } from 'tsdown';

export default defineConfig({
  entry: ['src/index.ts'],
  dts: true,
  format: 'esm',
  outDir: 'dist',
  outExtensions: () => ({ js: '.mjs' }),
  platform: 'neutral',
  sourcemap: true,
});
