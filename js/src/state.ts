/**
 * Store process-wide state on globalThis. The --import @flashcat/otel/register entry point loads the ESM build,
 * while require('@flashcat/otel') in a CommonJS app loads a separate CJS module instance.
 * Sharing state keeps init idempotent and gives diagnostics the same data in both builds.
 */

export class Counter {
  value = 0;
  inc(n = 1): void {
    this.value += n;
  }
}

export interface ExportCounts {
  [bucket: string]: number;
}

export class ExportStats {
  counts: ExportCounts = { ok: 0, network: 0, dropped_queue_full: 0, dropped_timeout: 0 };
  lastError: string | null = null;

  inc(bucket: string, n = 1, error?: string): void {
    this.counts[bucket] = (this.counts[bucket] ?? 0) + n;
    if (error) this.lastError = error;
  }

  snapshot(): Record<string, number | string | null> {
    return { ...this.counts, last_error: this.lastError };
  }
}

export interface SharedState {
  initialized: boolean;
  diag?: Record<string, any>;
  /** Resolved content capture switch. spanProcessor() reads it in onEnd because it can be created before init. */
  contentEnabled: boolean;
  stats: ExportStats;
  orphans: Counter;
  /** Count of inference spans nested under semantic API llm or embedding spans (contract §4.1). */
  nested: Counter;
  masked: Counter;
  truncated: Counter;
  /** Flashcat SpanProcessors created in this process, either by spanProcessor() or in scenario A. */
  processors: Array<{ forceFlush(): Promise<void>; shutdown(): Promise<void> }>;
  /** Configuration problems found by spanProcessor() before init. Merge into problems in init and diagnostics. */
  pendingProblems: string[];
  /** Whether the user has called spanProcessor(). */
  userProcessor: boolean;
  /** SDK-owned providers from scenario A. shutdown closes only these. */
  ownTracerProvider?: { forceFlush(): Promise<void>; shutdown(): Promise<void> };
  ownMeterProvider?: { forceFlush(): Promise<void>; shutdown(): Promise<void> };
  /** Number of spans ended since the last exit flush, used to prevent a beforeExit loop. */
  endedSinceExitFlush: number;
  exitHooksInstalled: boolean;
  shutdownTimeoutMs: number;
  /** Active flush or shutdown deadlines, in performance.now() milliseconds. See deadline.ts. */
  deadlines: Set<{ at: number }>;
  /** Abort functions for in-flight exports. Call all of them at the deadline. */
  inflight: Set<() => void>;
  /** Explicit calls, beforeExit, and SIGTERM share the result of a single shutdown. */
  shutdownPromise?: Promise<boolean>;
  /** Causes already logged at WARN. Log each once across both ESM and CJS builds (contract §8). */
  warned: Set<string>;
  /** Installed instrumentation instances by lib, used by the ESM fallback in register. */
  instances: Record<string, { source: string; inst: any }>;
}

const KEY = Symbol.for('flashcat.otel.state');

export function state(): SharedState {
  const g = globalThis as any;
  if (!g[KEY]) {
    const s: SharedState = {
      initialized: false,
      contentEnabled: false,
      stats: new ExportStats(),
      orphans: new Counter(),
      nested: new Counter(),
      masked: new Counter(),
      truncated: new Counter(),
      processors: [],
      pendingProblems: [],
      userProcessor: false,
      endedSinceExitFlush: 0,
      exitHooksInstalled: false,
      shutdownTimeoutMs: 5000,
      deadlines: new Set(),
      inflight: new Set(),
      warned: new Set(),
      instances: {},
    };
    Object.defineProperty(g, KEY, { value: s, enumerable: false, configurable: true, writable: false });
  }
  return g[KEY] as SharedState;
}
