/**
 * Package-local require. The ESM build gets it from createRequire(import.meta.url).
 * In the CJS build, tsup shims replace import.meta.url with pathToFileURL(__filename).
 * Load optional dependencies synchronously (instrumentation packages, ai, @ai-sdk/otel, and others), so init stays synchronous.
 */
import { createRequire } from 'node:module';

export const localRequire: NodeJS.Require = createRequire(import.meta.url);
