import { defineConfig } from 'tsdown';

export default defineConfig({
  entry: ['src/index.ts', 'src/renderer.ts'],
  format: 'esm',
  platform: 'neutral',
  deps: { neverBundle: [/^node:/] },
  dts: true,
  sourcemap: true,
});
