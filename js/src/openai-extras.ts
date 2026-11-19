/**
 * Add three attributes to upstream `@opentelemetry/instrumentation-openai` spans for openai 4.19 through 6.x:
 *
 * - `gen_ai.usage.cache_read.input_tokens`: read `usage.prompt_tokens_details.cached_tokens` for Chat and
 *     `usage.input_tokens_details.cached_tokens` for Responses, from the result or the final usage chunk / `response.completed` event when streaming.
 *     Cached tokens are already included in the input total. As in the Go middleware, record the breakdown only when positive.
 * - `gen_ai.response.time_to_first_chunk`: seconds from request start to the first streaming chunk with text, a refusal, or a tool call.
 *     Follow the Go middleware's markFirst rule and use the upstream startNow timestamp taken before the request.
 * - `gen_ai.request.stream`: true / false.
 *
 * Upstream 0.23.0 and main omit all three (verified Nov 17, 2026), which overstates cached input costs and leaves Node.js without time to first token (TTFT).
 *
 * Override internal methods on this instrumentation instance without adding spans or wrapping the OpenAI SDK again.
 * Upstream resolves `self.xxx(...)` at runtime, so the overrides also apply to modules loaded before init.
 * Set request.stream after span start and cached tokens before upstream handles a non-streaming result. Observe the upstream streaming iterator and yield chunks unchanged.
 * The application's return value and stream bypass these overrides. Write attributes before upstream ends the span.
 * Names and signatures were verified against 0.23.0 (^0.23.0 resolves to 0.23.x). Skip missing methods and ignore instrumentation errors.
 *
 * Upstream emits content as Logs API events, while the Flashcat backend reads span attributes. See bridgeContentEvents().
 *
 * OpenAI 7.x uses OpenInference 4.4.5, which already records cache reads in `llm.token_count.prompt_details.cache_read` for the backend.
 * It omits streaming TTFT, and streaming runs in a private module function. See augmentOpenInferenceOpenAI() for that case.
 */

import { context as otelContext, trace } from '@opentelemetry/api';
import { state } from './state';

const CACHE_READ = 'gen_ai.usage.cache_read.input_tokens';
const TTFT = 'gen_ai.response.time_to_first_chunk';
const STREAM = 'gen_ai.request.stream';

// Responses streaming events that count as first output, matching the Go middleware.
const RESPONSES_DELTA_EVENTS = new Set([
  'response.output_text.delta',
  'response.refusal.delta',
  'response.function_call_arguments.delta',
  'response.reasoning_text.delta',
  'response.reasoning_summary_text.delta',
  'response.audio.delta',
]);

function setCacheRead(span: any, cached: unknown): void {
  if (typeof cached === 'number' && cached > 0) span.setAttribute(CACHE_READ, cached);
}

/** Observe chunks while forwarding next, return, and throw unchanged. Observation errors leave the stream unaffected. */
export function observeIterator<T>(iterator: AsyncIterator<T>, onItem: (item: T) => void): AsyncIterator<T> {
  const wrapped: AsyncIterator<T> = {
    next: (...args: [] | [any]) =>
      iterator.next(...args).then((r) => {
        if (!r.done) {
          try {
            onItem(r.value);
          } catch {
            // Keep instrumentation errors from affecting the application.
          }
        }
        return r;
      }),
  };
  if (typeof iterator.return === 'function') wrapped.return = (v?: any) => iterator.return!(v);
  if (typeof iterator.throw === 'function') wrapped.throw = (e?: any) => iterator.throw!(e);
  return wrapped;
}

function chatChunkHasOutput(chunk: any): boolean {
  const choices = chunk?.choices;
  if (!Array.isArray(choices)) return false;
  return choices.some((c: any) => {
    // Legacy Completions chunks carry output in choices[].text.
    if (c?.text) return true;
    const d = c?.delta;
    return !!d && (!!d.content || !!d.refusal || (Array.isArray(d.tool_calls) && d.tool_calls.length > 0));
  });
}

function responsesEventHasOutput(event: any): boolean {
  if (RESPONSES_DELTA_EVENTS.has(event?.type)) return !!event.delta;
  return event?.type === 'response.output_item.added' && event.item?.type === 'function_call' && !!event.item?.name;
}

/** For streaming, record TTFT on the first chunk with output and cached tokens when a chunk carries usage. */
function streamObserver(span: any, startNow: unknown, hasOutput: (item: any) => boolean, cachedOf: (item: any) => unknown) {
  let first = false;
  return (item: any) => {
    if (!first && typeof startNow === 'number' && hasOutput(item)) {
      first = true;
      span.setAttribute(TTFT, (performance.now() - startNow) / 1000);
    }
    setCacheRead(span, cachedOf(item));
  };
}

/** Override one instance method, if present. */
function override(inst: any, name: string, make: (orig: (...a: any[]) => any) => (...a: any[]) => any): boolean {
  const orig = inst?.[name];
  if (typeof orig !== 'function') return false;
  inst[name] = make(orig);
  return true;
}

/** Add attributes to an upstream OpenAIInstrumentation instance. Return the methods overridden, or an empty array if none were present. */
export function augmentUpstreamOpenAI(inst: any): string[] {
  const done: string[] = [];
  const mark = (name: string, ok: boolean) => ok && done.push(name);

  for (const name of ['_startChatCompletionsSpan', '_startResponsesSpan']) {
    mark(
      name,
      override(inst, name, (orig) =>
        function (this: any, params: any, ...rest: any[]) {
          const info = orig.call(this, params, ...rest);
          try {
            info?.span?.setAttribute(STREAM, !!params?.stream);
          } catch {
            // Ignore instrumentation errors.
          }
          return info;
        },
      ),
    );
  }

  const results: Array<[string, (result: any) => unknown]> = [
    ['_onChatCompletionsCreateResult', (r) => r?.usage?.prompt_tokens_details?.cached_tokens],
    ['_onResponsesCreateResult', (r) => r?.usage?.input_tokens_details?.cached_tokens],
  ];
  for (const [name, cachedOf] of results) {
    mark(
      name,
      // Arguments: (span, startNow, commonAttrs, result, config, ctx).
      override(inst, name, (orig) =>
        function (this: any, span: any, startNow: any, commonAttrs: any, result: any, ...rest: any[]) {
          try {
            setCacheRead(span, cachedOf(result));
          } catch {
            // Ignore instrumentation errors.
          }
          return orig.call(this, span, startNow, commonAttrs, result, ...rest);
        },
      ),
    );
  }

  const streams: Array<[string, (item: any) => boolean, (item: any) => unknown]> = [
    ['_onChatCompletionsStreamIterator', chatChunkHasOutput, (c) => c?.usage?.prompt_tokens_details?.cached_tokens],
    [
      '_onResponsesStreamIterator',
      responsesEventHasOutput,
      (e) => (e?.type === 'response.completed' ? e.response?.usage?.input_tokens_details?.cached_tokens : undefined),
    ],
  ];
  for (const [name, hasOutput, cachedOf] of streams) {
    mark(
      name,
      // Arguments: (iterator, span, startNow, config, commonAttrs, ctx). Return the upstream async generator.
      override(inst, name, (orig) =>
        function (this: any, iterator: any, span: any, startNow: any, ...rest: any[]) {
          let it = iterator;
          try {
            if (span && iterator && typeof iterator.next === 'function') {
              it = observeIterator(iterator, streamObserver(span, startNow, hasOutput, cachedOf));
            }
          } catch {
            it = iterator;
          }
          return orig.call(this, it, span, startNow, ...rest);
        },
      ),
    );
  }
  mark('logs_to_span_content', bridgeContentEvents(inst));
  return done;
}

type Part = Record<string, unknown>;
interface Message {
  role: string;
  parts: Part[];
  finish_reason?: string;
}

/** Messages stored per span and garbage-collected along with it. */
const pendingContent = new WeakMap<object, { input: Message[]; output: Message[] }>();

function textPart(content: unknown): Part[] {
  return typeof content === 'string' && content !== '' ? [{ type: 'text', content }] : [];
}

function toolCallParts(calls: unknown): Part[] {
  if (!Array.isArray(calls)) return [];
  return calls.map((tc: any) => ({ type: 'tool_call', id: tc?.id, name: tc?.function?.name, arguments: tc?.function?.arguments }));
}

/**
 * Copy upstream log events to their span before it ends. In the legacy format, accumulate gen_ai.{system,user,assistant,tool}.message
 * as input and gen_ai.choice as output. Responses uses gen_ai.client.inference.operation.details with
 * gen_ai.input.messages and gen_ai.output.messages already present. Write only when content capture is on and mask after span end.
 */
function writeContent(record: any): void {
  if (!state().contentEnabled || !record) return;
  const span: any = trace.getSpan(record.context ?? otelContext.active());
  if (!span || (typeof span.isRecording === 'function' && !span.isRecording())) return;
  const attrs = record.attributes ?? {};
  const name = record.eventName ?? attrs['event.name'];
  const body = record.body ?? {};
  if (name === 'gen_ai.client.inference.operation.details') {
    for (const key of ['gen_ai.input.messages', 'gen_ai.output.messages']) {
      if (attrs[key] != null) span.setAttribute(key, typeof attrs[key] === 'string' ? attrs[key] : JSON.stringify(attrs[key]));
    }
    return;
  }
  let acc = pendingContent.get(span);
  if (!acc) {
    acc = { input: [], output: [] };
    pendingContent.set(span, acc);
  }
  switch (name) {
    case 'gen_ai.system.message':
    case 'gen_ai.user.message':
      acc.input.push({ role: name === 'gen_ai.system.message' ? 'system' : 'user', parts: textPart(body.content) });
      break;
    case 'gen_ai.assistant.message':
      acc.input.push({ role: 'assistant', parts: [...textPart(body.content), ...toolCallParts(body.tool_calls)] });
      break;
    case 'gen_ai.tool.message':
      acc.input.push({ role: 'tool', parts: [{ type: 'tool_call_response', id: body.id, response: body.content }] });
      break;
    case 'gen_ai.choice': {
      const m = body.message ?? {};
      const msg: Message = { role: 'assistant', parts: [...textPart(m.content), ...toolCallParts(m.tool_calls)] };
      if (body.finish_reason != null) msg.finish_reason = String(body.finish_reason);
      acc.output[typeof body.index === 'number' ? body.index : acc.output.length] = msg;
      break;
    }
    default:
      return;
  }
  if (acc.input.length) span.setAttribute('gen_ai.input.messages', JSON.stringify(acc.input));
  const out = acc.output.filter(Boolean);
  if (out.length) span.setAttribute('gen_ai.output.messages', JSON.stringify(out));
}

/**
 * Upstream emits content through this.logger. Give this instance a LoggerProvider that forwards every record to the original logger
 * without disturbing the application's logging pipeline and also copies its content to the same call's span.
 * Upstream emits with the call's span context and before span.end(), so the span is still writable.
 */
function bridgeContentEvents(inst: any): boolean {
  if (typeof inst?.setLoggerProvider !== 'function') return false;
  const original = inst.logger;
  const tee = {
    emit(record: any) {
      try {
        original?.emit?.(record);
      } catch {
        // Ignore instrumentation errors.
      }
      try {
        writeContent(record);
      } catch {
        // Ignore instrumentation errors.
      }
    },
  };
  inst.setLoggerProvider({ getLogger: () => tee });
  return true;
}

/** Observe the Stream iterator, including tee(), and record TTFT on the first chunk with output. */
function observeStream(stream: any, span: any, start: number, hasOutput: (item: any) => boolean): void {
  const orig = stream?.iterator;
  if (typeof orig !== 'function') return;
  let first = false;
  stream.iterator = function (this: any, ...args: any[]) {
    return observeIterator(orig.apply(this, args), (item: any) => {
      if (!first && hasOutput(item)) {
        first = true;
        span.setAttribute(TTFT, (performance.now() - start) / 1000);
      }
    });
  };
}

/**
 * Add TTFT to OpenInference spans for OpenAI 7.x. The instrumentation uses its instance's `_wrap` (shimmer.wrap)
 * to patch `create` on Chat.Completions, Completions, and Responses and calls the original method with its span active.
 * Intercept `_wrap` to put the observer inside the OpenInference wrapper, so the model call still produces one span.
 * Observe only streaming calls with an active span from this instrumentation. Chain through OpenAI's `_thenUnwrap`, as OpenInference does,
 * to preserve the APIPromise type and parse the response once. On unpatch, shimmer restores the original method.
 */
export function augmentOpenInferenceOpenAI(inst: any): boolean {
  const origWrap = inst?._wrap;
  if (typeof origWrap !== 'function') return false;
  const scope = inst.instrumentationName;
  inst._wrap = function (this: any, nodule: any, name: any, wrapper: any) {
    let w = wrapper;
    try {
      const kind = name === 'create' ? nodule?.constructor?.name : undefined;
      if ((kind === 'Completions' || kind === 'Responses') && typeof wrapper === 'function') {
        const hasOutput = kind === 'Responses' ? responsesEventHasOutput : chatChunkHasOutput;
        w = (original: any) => wrapper(withFirstChunk(original, scope, hasOutput));
      }
    } catch {
      w = wrapper;
    }
    return origWrap.call(this, nodule, name, w);
  };
  return true;
}

function withFirstChunk(original: any, scope: string, hasOutput: (item: any) => boolean): any {
  if (typeof original !== 'function') return original;
  return function (this: any, ...args: any[]) {
    const res = original.apply(this, args);
    try {
      if (!args[0]?.stream || typeof res?._thenUnwrap !== 'function') return res;
      const active: any = trace.getActiveSpan();
      // OpenInference uses OISpan. The underlying SDK span is on .span.
      const real = active?.span ?? active;
      if (!active || real?.instrumentationScope?.name !== scope) return res;
      const start = performance.now();
      return res._thenUnwrap((stream: any) => {
        try {
          observeStream(stream, active, start, hasOutput);
        } catch {
          // Ignore instrumentation errors.
        }
        return stream;
      });
    } catch {
      return res;
    }
  };
}
