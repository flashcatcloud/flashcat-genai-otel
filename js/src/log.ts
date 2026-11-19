/**
 * Log to stderr, leaving stdout for application output.
 * init logs a one-line summary (contract §8). Debug mode logs full diagnostics.
 */

import { state } from './state';

function write(level: string, msg: string): void {
  try {
    process.stderr.write(`[flashcat-otel] ${level} ${msg}\n`);
  } catch {
    // Keep SDK logging failures from affecting the application.
  }
}

export const log = {
  info: (msg: string) => write('INFO', msg),
  warn: (msg: string) => write('WARN', msg),
  /** Log each cause (key) once per process (contract §8). */
  warnOnce: (key: string, msg: string) => {
    const warned = state().warned;
    if (warned.has(key)) return;
    warned.add(key);
    write('WARN', msg);
  },
};
