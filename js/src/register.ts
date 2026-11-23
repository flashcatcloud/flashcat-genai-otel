/**
 * Zero-code entry point for ESM and CJS apps: `node --import @flashcat/genai-otel/register app.js`. --import always loads this ESM build.
 *
 * 1. Register the ESM loader hook (`@opentelemetry/instrumentation/hook.mjs`, via import-in-the-middle)
 *       so it can patch model SDK imports. Use --import to run before application code.
 * 2. Call init() using settings from FLASHCAT_* and OTEL_* environment variables.
 * 3. Apply the ESM fallback for OpenInference and built-in MCP instrumentation.
 */

import { log } from './log';
import { boot } from './register-common';
import { state } from './state';
import { isMainThread } from 'node:worker_threads';

boot();

/**
 * OpenInference instrumentation for openai >=7 and LangChain depends on @opentelemetry/instrumentation 0.46.0.
 * Its import-in-the-middle 1.7 hook is separate from the 0.301 hook above (import-in-the-middle 3.x),
 * so ESM imports miss its hook. Following the OpenInference docs, import the target ESM build and
 * call manuallyInstrument(). The app later imports the same module instance with its prototype methods already wrapped.
 */
const ESM_MANUAL: Array<[lib: string, specifier: string]> = [
  ['openai', 'openai'],
  ['langchain', '@langchain/core/callbacks/manager'],
  // The MCP client uses subpath exports, so the hook can miss internal ESM files. The fallback is a no-op if the hook already patched them.
  ['mcp', '@modelcontextprotocol/sdk/client/index.js'],
  ['google-genai', '@google/genai'],
];
for (const [lib, specifier] of isMainThread ? ESM_MANUAL : []) {
  const rec = state().instances[lib];
  const manual = rec?.source === 'openinference' || rec?.source === 'flashcat';
  if (!rec || !manual || typeof rec.inst?.manuallyInstrument !== 'function') continue;
  try {
    const mod = await import(specifier);
    rec.inst.manuallyInstrument(mod);
  } catch (e) {
    log.warn(`${lib} 的 ESM 补丁没有打上（OpenInference manuallyInstrument）：${(e as Error)?.message}`);
  }
}
