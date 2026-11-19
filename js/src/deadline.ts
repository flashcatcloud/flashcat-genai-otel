/**
 * Deadlines for flush and shutdown (contract §3 and §9).
 *
 * `shutdown_timeout` sets one deadline for flushing and shutting down both traces and metrics.
 * Each request uses the shorter of the export timeout and the time left until the earliest deadline. Once the deadline passes, skip new requests.
 * At the deadline, destroy all in-flight export requests. Completing the requests clears the timers
 * in BatchSpanProcessor, TracerProvider, and MetricReader (30 seconds each, still referenced).
 * Pending requests and retry timers would otherwise keep the event loop alive after the wait ends (measured Nov 16, 2026: 15.9 s for refused connections and 20.3 s for blackholed requests).
 */

import { performance } from 'node:perf_hooks';
import { state } from './state';

export interface Deadline {
  readonly at: number;
  /** Time remaining in milliseconds, clamped to zero. */
  remaining(): number;
  /**
   * Wait for p until the deadline. At the deadline, abort all in-flight requests, including those started
   * before the deadline applied, and return false. Also return false if p rejects or settles because the abort fired.
   */
  wait(p: Promise<unknown>): Promise<boolean>;
  /**
   * Stop applying this deadline to later requests. keepIfExpired retains an expired deadline and drops all subsequent exports immediately.
   * Shutdown uses this to prevent late exports, such as the final BatchSpanProcessor batch, from waiting another 10 seconds.
   */
  close(keepIfExpired?: boolean): void;
}

/**
 * Count spans dropped at the flush or shutdown deadline in export.dropped_timeout (contract §3 and §9).
 * Ordinary failures such as refused connections count as network errors. Match the error by name because ESM and CJS each have their own class.
 */
export class DeadlineError extends Error {
  constructor(message = 'flush / shutdown 时限已到，放弃导出') {
    super(message);
    this.name = 'DeadlineError';
  }
}

export function isDeadlineError(e: unknown): boolean {
  return e instanceof Error && e.name === 'DeadlineError';
}

/** Start a bounded flush or shutdown. Call close() when done. */
export function openDeadline(timeoutMs: number): Deadline {
  const st = state();
  const ms = Math.max(0, timeoutMs);
  const entry = { at: performance.now() + ms };
  st.deadlines.add(entry);
  // Abort in-flight requests at the deadline and mark the deadline expired. wait() then returns false even if the abort settles the pending promise.
  let expired = false;
  const expire = () => {
    if (expired) return;
    expired = true;
    // Node timers can fire slightly before at. Once the abort fires, later requests and backoffs must see an exhausted budget.
    entry.at = Math.min(entry.at, performance.now());
    abortInflight();
  };
  // The fallback timer is unrefed. In-flight requests keep the event loop alive long enough for it to fire.
  const timer = setTimeout(expire, ms);
  timer.unref?.();
  const remaining = () => Math.max(0, entry.at - performance.now());
  return {
    at: entry.at,
    remaining,
    // Abort in the wait timeout callback itself. If wait() returned and close() cleared the fallback timer first,
    // requests started before the deadline applied could still hang for the 10-second export timeout (observed up to 9 seconds after shutdown returned).
    wait: (p) =>
      new Promise<boolean>((resolve) => {
        const t = setTimeout(() => {
          expire();
          resolve(false);
        }, remaining());
        t.unref?.();
        p.then(
          () => {
            clearTimeout(t);
            resolve(!expired);
          },
          () => {
            clearTimeout(t);
            resolve(false);
          },
        );
      }),
    close: (keepIfExpired = false) => {
      clearTimeout(timer);
      if (!(keepIfExpired && remaining() === 0)) st.deadlines.delete(entry);
    },
  };
}

/** Request budget in milliseconds, capped by the export timeout and the earliest deadline. A value of zero or less means skip the request. */
export function requestBudget(timeoutMillis: number): number {
  let budget = timeoutMillis;
  const now = performance.now();
  for (const d of state().deadlines) budget = Math.min(budget, d.at - now);
  return budget;
}

/**
 * Retry only during bounded flush or shutdown. In addition to the backoff, reserve the previous request's duration (at least 1 ms) for another attempt.
 * The next request's duration is unknown, so cap its actual timeout at the remaining budget. Normal operation has no retries.
 */
export function waitForRetry(delayMs: number, requestMillis: number): Promise<boolean> {
  const reserve = Math.max(1, requestMillis);
  const canRetry = (delay: number) => state().deadlines.size > 0 && requestBudget(Infinity) > delay + reserve;
  if (!canRetry(delayMs)) return Promise.resolve(false);
  return new Promise((resolve) => {
    const done = (retry: boolean) => {
      clearTimeout(timer);
      untrack();
      resolve(retry);
    };
    // Keep the timer referenced during backoff. With no request socket open, the process could otherwise exit before the retry.
    const timer = setTimeout(() => done(canRetry(0)), delayMs);
    const untrack = trackInflight(() => done(false));
  });
}

/** Track an in-flight export request and return a function to unregister it. */
export function trackInflight(abort: () => void): () => void {
  const set = state().inflight;
  set.add(abort);
  return () => set.delete(abort);
}

/** Abort all in-flight export requests and retry waits. Count affected spans in dropped_timeout. */
export function abortInflight(): void {
  for (const abort of [...state().inflight]) {
    try {
      abort();
    } catch {
      // Ignore abort errors.
    }
  }
}
