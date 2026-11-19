import { defineConfig, type Options } from 'tsup';

const common: Options = {
  dts: true,
  shims: true,
  splitting: true,
  target: 'node22',
  platform: 'node',
  outDir: 'dist',
  sourcemap: false,
};

// Configure the formats separately. ESM register needs top-level await, and the CJS build uses a separate entry point.
export default defineConfig([
  {
    ...common,
    clean: true,
    entry: { index: 'src/index.ts', register: 'src/register.ts' },
    format: ['esm'],
    outExtension: () => ({ js: '.mjs' }),
  },
  {
    ...common,
    clean: false,
    entry: { index: 'src/index.ts', register: 'src/register-cjs.ts' },
    format: ['cjs'],
    outExtension: () => ({ js: '.cjs' }),
  },
]);
