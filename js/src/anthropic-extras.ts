/**
 * Add token usage and response attributes to upstream `@opentelemetry/instrumentation-anthropic` for @anthropic-ai/sdk 0.65–0.133.
 * Version 0.2.0 (verified Nov 18, 2026) records only operation, provider, and request model. Cost calculation also needs usage.
 *
 * Additional attributes follow the GenAI semantic conventions:
 * - `gen_ai.usage.input_tokens` includes input, cache reads, and cache creation because Anthropic reports cached input separately.
 * - `gen_ai.usage.cache_read.input_tokens` and `gen_ai.usage.cache_creation.input_tokens` are set when positive.
 * - `gen_ai.usage.output_tokens`, `gen_ai.response.model`, `gen_ai.response.id`, and `gen_ai.response.finish_reasons`.
 * Content capture is unavailable here. Use @anthropic-ai/sdk 0.134 or later for built-in content capture.
 *
 * As in openai-extras.ts, override only the internal methods on this instrumentation instance.
 * Upstream resolves `this.xxx(...)` at runtime, so there is no need for extra spans or SDK method wrappers.
 * - Non-streaming: wrap `parse` before upstream does, save the parsed Message on upstream state, and pass through the result or error.
 * - Streaming: wrap the iterator passed upstream, observe message_start and message_delta, and yield chunks unchanged.
 *     `messages.stream()` uses the same path.
 * - Write the captured values to the span before upstream `_endSpan(state)` ends it.
 * Method names and signatures were verified against 0.2.0 (declared as ^0.2.0). Skip missing methods and ignore instrumentation errors.
 */

const KEY = Symbol('flashcat.anthropic.usage');

interface Collected {
  input?: number;
  output?: number;
  cacheRead?: number;
  cacheWrite?: number;
  model?: string;
  id?: string;
  stopReason?: string;
}

function collected(state: any): Collected {
  if (!state[KEY]) state[KEY] = {};
  return state[KEY];
}

const num = (v: unknown) => (typeof v === 'number' && Number.isFinite(v) && v >= 0 ? v : undefined);

/** Record token counts from usage, with later values overriding earlier ones. output_tokens in message_delta is cumulative. */
function takeUsage(c: Collected, usage: any): void {
  if (!usage || typeof usage !== 'object') return;
  c.input = num(usage.input_tokens) ?? c.input;
  c.output = num(usage.output_tokens) ?? c.output;
  c.cacheRead = num(usage.cache_read_input_tokens) ?? c.cacheRead;
  c.cacheWrite = num(usage.cache_creation_input_tokens) ?? c.cacheWrite;
}

function takeMessage(state: any, msg: any): void {
  if (!msg || typeof msg !== 'object' || msg.type !== 'message') return;
  const c = collected(state);
  takeUsage(c, msg.usage);
  if (typeof msg.model === 'string') c.model = msg.model;
  if (typeof msg.id === 'string') c.id = msg.id;
  if (typeof msg.stop_reason === 'string') c.stopReason = msg.stop_reason;
}

function takeEvent(state: any, ev: any): void {
  if (!ev || typeof ev !== 'object') return;
  if (ev.type === 'message_start') takeMessage(state, ev.message);
  else if (ev.type === 'message_delta') {
    const c = collected(state);
    takeUsage(c, ev.usage);
    if (typeof ev.delta?.stop_reason === 'string') c.stopReason = ev.delta.stop_reason;
  }
}

function writeCollected(state: any): void {
  const c: Collected | undefined = state?.[KEY];
  const span = state?.span;
  if (!c || !span || typeof span.setAttribute !== 'function') return;
  const cacheRead = c.cacheRead ?? 0;
  const cacheWrite = c.cacheWrite ?? 0;
  if (c.input !== undefined) span.setAttribute('gen_ai.usage.input_tokens', c.input + cacheRead + cacheWrite);
  if (c.output !== undefined) span.setAttribute('gen_ai.usage.output_tokens', c.output);
  if (cacheRead > 0) span.setAttribute('gen_ai.usage.cache_read.input_tokens', cacheRead);
  if (cacheWrite > 0) span.setAttribute('gen_ai.usage.cache_creation.input_tokens', cacheWrite);
  if (c.model) span.setAttribute('gen_ai.response.model', c.model);
  if (c.id) span.setAttribute('gen_ai.response.id', c.id);
  if (c.stopReason) span.setAttribute('gen_ai.response.finish_reasons', [c.stopReason]);
}

function override(inst: any, name: string, make: (orig: (...a: any[]) => any) => (...a: any[]) => any): boolean {
  const orig = inst?.[name];
  if (typeof orig !== 'function') return false;
  inst[name] = make(orig);
  return true;
}

/** Return the names of the overrides that took effect for tests and diagnostics. */
export function augmentUpstreamAnthropic(inst: any): string[] {
  const done: string[] = [];
  const mark = (name: string, ok: boolean) => ok && done.push(name);

  // Arguments: (apiPromise, state). Wrap parse first so upstream's then runs after the returned promise settles
  // and the Message is available by the time upstream calls _endSpan.
  mark(
    '_observeAPIPromise',
    override(inst, '_observeAPIPromise', (orig) =>
      function (this: any, apiPromise: any, state: any, ...rest: any[]) {
        try {
          const parse = apiPromise?.parse;
          if (typeof parse === 'function') {
            apiPromise.parse = function (this: any, ...a: any[]) {
              const p = parse.apply(apiPromise, a);
              return p && typeof p.then === 'function'
                ? p.then((v: any) => {
                    try {
                      takeMessage(state, v);
                    } catch {
                      // Ignore instrumentation errors.
                    }
                    return v;
                  })
                : p;
            };
          }
        } catch {
          // Ignore instrumentation errors.
        }
        return orig.call(this, apiPromise, state, ...rest);
      },
    ),
  );

  // Arguments: (iterator, state, stream). Observe events and pass chunks unchanged to upstream and the application.
  mark(
    '_streamIterator',
    override(inst, '_streamIterator', (orig) =>
      function (this: any, iterator: any, state: any, ...rest: any[]) {
        let it = iterator;
        try {
          if (iterator && typeof iterator.next === 'function') {
            it = Object.create(iterator);
            it.next = async (...a: any[]) => {
              const r = await iterator.next(...a);
              try {
                if (r && !r.done) takeEvent(state, r.value);
              } catch {
                // Ignore instrumentation errors.
              }
              return r;
            };
            // Call return and throw on the original async generator so early termination closes the underlying connection.
            if (typeof iterator.return === 'function') it.return = (...a: any[]) => iterator.return(...a);
            if (typeof iterator.throw === 'function') it.throw = (...a: any[]) => iterator.throw(...a);
          }
        } catch {
          it = iterator;
        }
        return orig.call(this, it, state, ...rest);
      },
    ),
  );

  mark(
    '_endSpan',
    override(inst, '_endSpan', (orig) =>
      function (this: any, state: any, ...rest: any[]) {
        try {
          if (state && !state.ended) writeCollected(state);
        } catch {
          // Ignore instrumentation errors.
        }
        return orig.call(this, state, ...rest);
      },
    ),
  );
  return done;
}
