/**
 * Shared setup for the ESM and CJS zero-code entry points.
 *
 * 1. Register the ESM loader hook (`@opentelemetry/instrumentation/hook.mjs`, via import-in-the-middle)
 *       so it can patch model SDK imports. Use --import to run before application code.
 * 2. Call init() using settings from FLASHCAT_* and OTEL_* environment variables.
 */

import { register } from 'node:module';
import { isMainThread } from 'node:worker_threads';
import { init } from './index';
import { log } from './log';

function disabled(): boolean {
  return ['FLASHCAT_DISABLED', 'OTEL_SDK_DISABLED'].some((k) =>
    ['1', 'true', 'yes', 'on'].includes((process.env[k] ?? '').trim().toLowerCase()),
  );
}

/**
 * Start on the main thread only. `module.register()` starts a loader thread, and Node runs --require preloads
 * again there (--import does not), which would repeat init and hook registration. Public APIs cannot distinguish
 * loader threads from application workers, so workers must call init() explicitly when needed.
 */
export function boot(): void {
  if (!isMainThread) return;
  if (!disabled()) {
    try {
      register('@opentelemetry/instrumentation/hook.mjs', import.meta.url);
    } catch (e) {
      log.warn(`注册 ESM 加载钩子失败，ESM 应用里的模型 SDK 不会被打补丁：${(e as Error)?.message}`);
    }
  }

  try {
    init();
  } catch (e) {
    // Environment-based configuration does not throw ConfigError. Catch unexpected failures so zero-code setup cannot prevent startup.
    log.warn(`init 失败：${(e as Error)?.name}: ${(e as Error)?.message}`);
  }
}
