/**
 * Semantic API (contract §4 and §5).
 *
 * OTel JS genai-util 0.5.0 provides startInference, startEmbedding, and startTool but has no agent or workflow helpers.
 * Start spans directly with the tracer and set GenAI semantic convention attributes at span start.
 */

import {
  SpanKind,
  SpanStatusCode,
  context as otelContext,
  propagation,
  trace as traceApi,
  type Attributes,
  type Context,
  type Span,
} from '@opentelemetry/api';
import { CONV_KEY, FRAME_KEY, TAGS_KEY, USER_KEY, type SemanticFrame } from './processors';
import { state } from './state';
import { VERSION } from './version';

export const SCOPE = 'flashcat.otel';
const MAX_SERIALIZED = 32 * 1024; // Serialization limit for arguments and return values.
const TRUNCATED = '…[truncated]';

export interface AgentOptions {
  /** Written to gen_ai.agent.description. */
  description?: string;
}

export interface ToolOptions {
  /** Written to gen_ai.tool.description. */
  description?: string;
}

export interface ModelOptions {
  /** Written to gen_ai.provider.name, for example openai or dashscope. */
  provider?: string;
}

export interface Usage {
  inputTokens?: number;
  outputTokens?: number;
  cacheReadInputTokens?: number;
  /** Maps to Anthropic cache creation. */
  cacheWriteInputTokens?: number;
}

export interface AnnotateOptions {
  /** Agent / workflow / llm: string or [{ role, content }]. Tool: arguments. Retrieval: query text. */
  input?: unknown;
  /** Agent / workflow / llm: string or [{ role, content }]. Tool: result. Retrieval: document array. */
  output?: unknown;
  /** llm and embedding: gen_ai.response.model. */
  model?: string;
  /** llm accepts all four token counts. embedding accepts only inputTokens. */
  usage?: Usage;
  /** llm: gen_ai.response.finish_reasons. */
  finishReasons?: string[];
  /** Applies to all types as flashcat.tag.<k>. */
  tags?: Record<string, string | number | boolean>;
}

export interface SessionOptions {
  conversationId?: string;
  userId?: string;
  tags?: Record<string, string | number | boolean>;
  /** Write to baggage, which travels downstream in HTTP headers, including to model providers. Defaults to false. */
  propagate?: boolean;
}

/** Serialize with a size limit. Skip object internals and fall back to String() if serialization fails. */
export function serialize(value: unknown): string {
  let s: string;
  try {
    if (typeof value === 'string') s = value;
    else {
      const j = JSON.stringify(value);
      s = j === undefined ? String(value) : j;
    }
  } catch {
    try {
      s = String(value);
    } catch {
      s = Object.prototype.toString.call(value);
    }
  }
  if (s.length > MAX_SERIALIZED) s = s.slice(0, MAX_SERIALIZED) + TRUNCATED;
  return s;
}

/** Structured message JSON from annotate maps each {role, content} item to a message. Otherwise serialize the whole value as one text message. */
function messagesJSON(value: unknown, role: string, structured: boolean): string {
  const msg = (r: string, content: unknown) => ({
    role: r,
    parts: [{ type: 'text', content: typeof content === 'string' ? content : serialize(content) }],
  });
  if (
    structured &&
    Array.isArray(value) &&
    value.length > 0 &&
    value.every((m) => m != null && typeof m === 'object' && !Array.isArray(m) && 'role' in m)
  ) {
    return JSON.stringify(value.map((m: any) => msg(String(m.role), m.content)));
  }
  return JSON.stringify([msg(role, serialize(value))]);
}

function isPlainObject(v: unknown): v is Record<string, unknown> {
  return v != null && typeof v === 'object' && !Array.isArray(v);
}

/** Keep retrieval objects as-is and wrap other elements as {content}. If over the limit, drop items from the end and append a truncation marker (contract §4). */
function documentsJSON(value: unknown): string {
  const items = Array.isArray(value) ? value : [value];
  const docs: unknown[] = items.map((d) => (isPlainObject(d) ? d : { content: serialize(d) }));
  const dump = (ds: unknown[]) => {
    try {
      return JSON.stringify(ds) ?? '[]';
    } catch {
      return JSON.stringify(ds.map((d) => ({ content: serialize(d) })));
    }
  };
  let s = dump(docs);
  if (s.length > MAX_SERIALIZED) {
    while (docs.length && dump(docs).length > MAX_SERIALIZED) docs.pop();
    docs.push({ content: TRUNCATED });
    s = dump(docs);
  }
  return s;
}

function contentOn(): boolean {
  return state().contentEnabled;
}

function tracer() {
  return traceApi.getTracer(SCOPE, VERSION);
}

type Kind = 'agent' | 'tool' | 'workflow' | 'llm' | 'embedding' | 'retrieval';

interface KindSpec {
  kind: Kind;
  name: string;
  description?: string;
  provider?: string;
}

/** Active semantic span for one call. If annotate set output, keep it when the span ends. */
interface Frame extends SemanticFrame {
  span: Span;
  kind: Kind;
}

/** Set session attributes directly on semantic API spans so they are available even when the session processor cannot be attached to an existing provider (scenarios B and C). */
function sessionAttributes(ctx: Context): Attributes {
  const out: Attributes = {};
  const conv = ctx.getValue(CONV_KEY);
  const user = ctx.getValue(USER_KEY);
  const tags = ctx.getValue(TAGS_KEY) as Record<string, string> | undefined;
  if (conv) out['gen_ai.conversation.id'] = String(conv);
  if (user) out['user.id'] = String(user);
  if (tags) for (const [k, v] of Object.entries(tags)) out[`flashcat.tag.${k}`] = String(v);
  return out;
}

function startAttributes(spec: KindSpec): { spanName: string; attrs: Attributes } {
  if (spec.kind === 'llm' || spec.kind === 'embedding') {
    const op = spec.kind === 'llm' ? 'chat' : 'embeddings';
    const attrs: Attributes = { 'gen_ai.operation.name': op, 'gen_ai.request.model': spec.name };
    if (spec.provider) attrs['gen_ai.provider.name'] = spec.provider;
    return { spanName: `${op} ${spec.name}`, attrs };
  }
  if (spec.kind === 'retrieval') {
    return {
      spanName: `retrieval ${spec.name}`,
      attrs: { 'gen_ai.operation.name': 'retrieval', 'gen_ai.data_source.id': spec.name },
    };
  }
  if (spec.kind === 'agent') {
    const attrs: Attributes = { 'gen_ai.operation.name': 'invoke_agent', 'gen_ai.agent.name': spec.name };
    if (spec.description) attrs['gen_ai.agent.description'] = spec.description;
    return { spanName: `invoke_agent ${spec.name}`, attrs };
  }
  if (spec.kind === 'tool') {
    const attrs: Attributes = {
      'gen_ai.operation.name': 'execute_tool',
      'gen_ai.tool.name': spec.name,
      'gen_ai.tool.type': 'function',
    };
    if (spec.description) attrs['gen_ai.tool.description'] = spec.description;
    return { spanName: `execute_tool ${spec.name}`, attrs };
  }
  return {
    spanName: `invoke_workflow ${spec.name}`,
    attrs: { 'gen_ai.operation.name': 'invoke_workflow', 'gen_ai.workflow.name': spec.name },
  };
}

/** Write content to attributes according to the span type (contract §4). The caller checks the content capture switch. */
function writeInput(span: Span, kind: Kind, value: unknown, structured: boolean): void {
  if (kind === 'tool') span.setAttribute('gen_ai.tool.call.arguments', serialize(value));
  else if (kind === 'retrieval') span.setAttribute('gen_ai.retrieval.query.text', serialize(value));
  else if (kind !== 'embedding') span.setAttribute('gen_ai.input.messages', messagesJSON(value, 'user', structured));
}

function writeOutput(span: Span, kind: Kind, value: unknown, structured: boolean): void {
  if (kind === 'tool') span.setAttribute('gen_ai.tool.call.result', serialize(value));
  else if (kind === 'retrieval') span.setAttribute('gen_ai.retrieval.documents', documentsJSON(value));
  else if (kind !== 'embedding') span.setAttribute('gen_ai.output.messages', messagesJSON(value, 'assistant', structured));
}

function setInput(span: Span, spec: KindSpec, args: unknown[]): void {
  if (contentOn()) writeInput(span, spec.kind, args, false);
}

function setOutput(span: Span, kind: Kind, result: unknown): void {
  if (contentOn()) writeOutput(span, kind, result, false);
}

function errorType(err: unknown): string {
  if (err && typeof err === 'object') {
    const n = (err as any).constructor?.name || (err as any).name;
    if (typeof n === 'string' && n) return n;
  }
  return typeof err;
}

/** Set ERROR status and error.type, and record an exception event (contract §4). */
function failSpan(span: Span, err: unknown): void {
  try {
    span.setAttribute('error.type', errorType(err));
    if (err instanceof Error) span.recordException(err);
    else span.recordException({ name: errorType(err), message: String(err) });
    span.setStatus({ code: SpanStatusCode.ERROR, message: err instanceof Error ? err.message : String(err) });
    span.end();
  } catch {
    // Ignore instrumentation errors.
  }
}

function okSpan(span: Span, frame: Frame | undefined, result: unknown): void {
  try {
    if (frame && !frame.outputSet) setOutput(span, frame.kind, result);
    span.end();
  } catch {
    // Ignore instrumentation errors.
  }
}

function isNativePromise(v: unknown): v is Promise<unknown> {
  return v instanceof Promise && v.constructor === Promise;
}

function isThenable(v: unknown): v is PromiseLike<unknown> {
  return v != null && (typeof v === 'object' || typeof v === 'function') && typeof (v as any).then === 'function';
}

function isAsyncIterator(v: any): boolean {
  return v != null && typeof v === 'object' && typeof v.next === 'function' && typeof v[Symbol.asyncIterator] === 'function';
}

/** An object with [Symbol.asyncIterator] but no next method, such as a model SDK Stream. */
function isAsyncIterable(v: any): boolean {
  return v != null && typeof v === 'object' && typeof v.next !== 'function' && typeof v[Symbol.asyncIterator] === 'function';
}

/**
 * Preserve the object and APIs such as controller or tee() by overriding only its [Symbol.asyncIterator] method.
 * Wrap the first iteration. End the span immediately if the object is frozen or the override otherwise fails.
 */
function patchAsyncIterable(obj: any, span: Span, ctx: Context, frame: Frame | undefined): any {
  const orig = obj[Symbol.asyncIterator];
  let used = false;
  try {
    Object.defineProperty(obj, Symbol.asyncIterator, {
      configurable: true,
      writable: true,
      value: function (this: any) {
        const it = orig.call(this);
        if (used) return it;
        used = true;
        return wrapAsyncIterator(it, span, ctx, frame);
      },
    });
  } catch {
    okSpan(span, frame, undefined);
  }
  return obj;
}

function isSyncGenerator(v: any): boolean {
  return v != null && typeof v === 'object' && Object.prototype.toString.call(v) === '[object Generator]';
}

/**
 * Wrap an async iterator and run next, return, and throw in the span context so model calls made by the stream become child spans.
 * End the span on exhaustion, early return, or error. Use the original object as the prototype to preserve its other properties.
 */
function wrapAsyncIterator(it: any, span: Span, ctx: Context, frame: Frame | undefined): any {
  const items: unknown[] = [];
  let done = false;
  const finish = (err?: unknown, isErr = false) => {
    if (done) return;
    done = true;
    if (isErr) failSpan(span, err);
    else okSpan(span, frame, items);
  };
  const call = (fn: () => any) => otelContext.with(ctx, fn);
  const wrapper = Object.create(it);
  wrapper.next = function (...a: unknown[]) {
    let p: any;
    try {
      p = call(() => it.next(...a));
    } catch (e) {
      finish(e, true);
      throw e;
    }
    return Promise.resolve(p).then(
      (r: IteratorResult<unknown>) => {
        if (r && r.done) finish();
        else if (contentOn() && items.length < 10_000) items.push(r?.value);
        return r;
      },
      (e: unknown) => {
        finish(e, true);
        throw e;
      },
    );
  };
  wrapper.return = function (v?: unknown) {
    finish();
    if (typeof it.return !== 'function') return Promise.resolve({ done: true, value: v });
    return call(() => it.return(v));
  };
  wrapper.throw = function (e?: unknown) {
    if (typeof it.throw !== 'function') {
      finish(e, true);
      return Promise.reject(e);
    }
    let p: any;
    try {
      p = call(() => it.throw(e));
    } catch (err) {
      finish(err, true);
      throw err;
    }
    return Promise.resolve(p).then(
      (r: IteratorResult<unknown>) => {
        if (r && r.done) finish();
        return r;
      },
      (err: unknown) => {
        finish(err, true);
        throw err;
      },
    );
  };
  wrapper[Symbol.asyncIterator] = function () {
    return wrapper;
  };
  return wrapper;
}

/** Apply the same behavior to synchronous generators (contract §4). */
function wrapSyncGenerator(it: any, span: Span, ctx: Context, frame: Frame | undefined): any {
  const items: unknown[] = [];
  let done = false;
  const finish = (err?: unknown, isErr = false) => {
    if (done) return;
    done = true;
    if (isErr) failSpan(span, err);
    else okSpan(span, frame, items);
  };
  const step = (fn: () => IteratorResult<unknown>, collect: boolean) => {
    let r: IteratorResult<unknown>;
    try {
      r = otelContext.with(ctx, fn);
    } catch (e) {
      finish(e, true);
      throw e;
    }
    if (r && r.done) finish();
    else if (collect && contentOn() && items.length < 10_000) items.push(r?.value);
    return r;
  };
  const wrapper = Object.create(it);
  wrapper.next = (...a: unknown[]) => step(() => it.next(...a), true);
  wrapper.return = (v?: unknown) => {
    finish();
    return otelContext.with(ctx, () => it.return(v));
  };
  wrapper.throw = (e?: unknown) => step(() => it.throw(e), false);
  wrapper[Symbol.iterator] = () => wrapper;
  return wrapper;
}

/**
 * For a native Promise, return a chained promise that ends the span and preserves unhandled rejection semantics.
 * Observe other thenables without replacing them, such as a model SDK APIPromise with a withResponse method.
 * Wrap async iterators and synchronous generators. End the span immediately for other return values.
 */
function settle(result: unknown, span: Span, ctx: Context, frame: Frame | undefined): unknown {
  if (isNativePromise(result)) {
    return result.then(
      (v) => {
        if (isAsyncIterator(v)) return wrapAsyncIterator(v, span, ctx, frame);
        if (isAsyncIterable(v)) return patchAsyncIterable(v, span, ctx, frame);
        okSpan(span, frame, v);
        return v;
      },
      (e) => {
        failSpan(span, e);
        throw e;
      },
    );
  }
  if (isThenable(result)) {
    try {
      result.then(
        (v) => okSpan(span, frame, v),
        (e) => failSpan(span, e),
      );
    } catch (e) {
      failSpan(span, e);
    }
    return result;
  }
  if (isAsyncIterator(result)) return wrapAsyncIterator(result, span, ctx, frame);
  if (isAsyncIterable(result)) return patchAsyncIterable(result, span, ctx, frame);
  if (isSyncGenerator(result)) return wrapSyncGenerator(result, span, ctx, frame);
  okSpan(span, frame, result);
  return result;
}

function wrap<F extends (...args: any[]) => any>(spec: KindSpec, fn: F): F {
  if (typeof fn !== 'function') return fn;
  const wrapped = function (this: unknown, ...args: unknown[]) {
    let span: Span | undefined;
    let ctx: Context | undefined;
    let frame: Frame | undefined;
    try {
      const parent = otelContext.active();
      const { spanName, attrs } = startAttributes(spec);
      const client = spec.kind === 'llm' || spec.kind === 'embedding' || spec.kind === 'retrieval';
      span = tracer().startSpan(
        spanName,
        { kind: client ? SpanKind.CLIENT : SpanKind.INTERNAL, attributes: { ...sessionAttributes(parent), ...attrs } },
        parent,
      );
      setInput(span, spec, args);
      frame = { span, kind: spec.kind, outputSet: false };
      ctx = traceApi.setSpan(parent, span).setValue(FRAME_KEY, frame);
    } catch {
      // If instrumentation fails, call the original function directly.
      span = undefined;
    }
    if (!span || !ctx) return fn.apply(this, args);
    const s = span;
    const c = ctx;
    let result: unknown;
    try {
      result = otelContext.with(c, () => fn.apply(this, args));
    } catch (e) {
      failSpan(s, e);
      throw e;
    }
    try {
      return settle(result, s, c, frame);
    } catch {
      return result;
    }
  };
  try {
    Object.defineProperty(wrapped, 'name', { value: fn.name, configurable: true });
    Object.defineProperty(wrapped, 'length', { value: fn.length, configurable: true });
  } catch {
    // Ignore instrumentation errors.
  }
  return wrapped as unknown as F;
}

/** Mark an agent entry point with an invoke_agent span. The wrapper preserves the signature and this. */
export function agent<F extends (...args: any[]) => any>(name: string, fn: F, opts?: AgentOptions): F {
  return wrap({ kind: 'agent', name, description: opts?.description }, fn);
}

/** Mark a tool function with an execute_tool span. */
export function tool<F extends (...args: any[]) => any>(name: string, fn: F, opts?: ToolOptions): F {
  return wrap({ kind: 'tool', name, description: opts?.description }, fn);
}

/** Mark a workflow with an invoke_workflow span. */
export function workflow<F extends (...args: any[]) => any>(name: string, fn: F): F {
  return wrap({ kind: 'workflow', name }, fn);
}

/**
 * Mark an uninstrumented model call, such as a custom gateway or private SDK, with a chat {model} span. Use annotate for usage, response model, and messages.
 * Wrapping an instrumented model call counts it twice (contract §4.1).
 */
export function llm<F extends (...args: any[]) => any>(model: string, fn: F, opts?: ModelOptions): F {
  return wrap({ kind: 'llm', name: model, provider: opts?.provider }, fn);
}

/** Mark an uninstrumented embedding call with an embeddings {model} span. Input content is omitted. */
export function embedding<F extends (...args: any[]) => any>(model: string, fn: F, opts?: ModelOptions): F {
  return wrap({ kind: 'embedding', name: model, provider: opts?.provider }, fn);
}

/** Mark a vector database or search engine lookup with a retrieval {name} span. Use its return value as the retrieved documents. */
export function retrieval<F extends (...args: any[]) => any>(name: string, fn: F): F {
  return wrap({ kind: 'retrieval', name }, fn);
}

const USAGE_ATTRS: Array<[keyof Usage, string]> = [
  ['inputTokens', 'gen_ai.usage.input_tokens'],
  ['outputTokens', 'gen_ai.usage.output_tokens'],
  ['cacheReadInputTokens', 'gen_ai.usage.cache_read.input_tokens'],
  ['cacheWriteInputTokens', 'gen_ai.usage.cache_write.input_tokens'],
];

/**
 * Add data to the innermost semantic API span (agent / tool / workflow / llm / embedding / retrieval) in the current context (contract §4.2).
 * With no semantic span, do nothing. Input and output honor the content capture switch. Annotated output takes precedence over the return value.
 */
export function annotate(opts: AnnotateOptions): void {
  try {
    const frame = otelContext.active().getValue(FRAME_KEY) as Frame | undefined;
    if (!frame || !frame.span || !opts) return;
    const { span, kind } = frame;
    if (opts.input !== undefined && contentOn()) writeInput(span, kind, opts.input, true);
    if (opts.output !== undefined && kind !== 'embedding') {
      frame.outputSet = true;
      if (contentOn()) writeOutput(span, kind, opts.output, true);
    }
    const model = kind === 'llm' || kind === 'embedding';
    if (opts.model != null && model) span.setAttribute('gen_ai.response.model', String(opts.model));
    if (opts.usage && model) {
      for (const [k, attr] of kind === 'embedding' ? USAGE_ATTRS.slice(0, 1) : USAGE_ATTRS) {
        const v = opts.usage[k];
        if (typeof v === 'number' && Number.isInteger(v) && v >= 0) span.setAttribute(attr, v);
      }
    }
    if (Array.isArray(opts.finishReasons) && kind === 'llm') {
      span.setAttribute('gen_ai.response.finish_reasons', opts.finishReasons.map(String));
    }
    if (opts.tags) for (const [k, v] of Object.entries(opts.tags)) span.setAttribute(`flashcat.tag.${k}`, String(v));
  } catch {
    // Keep SDK failures from affecting the application.
  }
}

/** Run fn(span) immediately with a non-GenAI span, typically the root span of a script or job. */
export function trace<T>(name: string, fn: (span: Span) => T, attributes?: Attributes): T {
  let span: Span | undefined;
  let ctx: Context | undefined;
  try {
    const parent = otelContext.active();
    span = tracer().startSpan(
      name,
      { kind: SpanKind.INTERNAL, attributes: { ...sessionAttributes(parent), ...(attributes ?? {}) } },
      parent,
    );
    ctx = traceApi.setSpan(parent, span);
  } catch {
    span = undefined;
  }
  if (!span || !ctx) return fn(traceApi.wrapSpanContext({ traceId: '0'.repeat(32), spanId: '0'.repeat(16), traceFlags: 0 }));
  const s = span;
  const c = ctx;
  let result: T;
  try {
    result = otelContext.with(c, () => fn(s));
  } catch (e) {
    failSpan(s, e);
    throw e;
  }
  try {
    return settle(result, s, c, undefined) as T;
  } catch {
    return result;
  }
}

/**
 * Run fn immediately. New spans within its session scope carry gen_ai.conversation.id, user.id, and flashcat.tag.*.
 * Baggage is off by default because HTTP headers carry it downstream. Set propagate: true for cross-service correlation.
 */
export function session<T>(opts: SessionOptions, fn: () => T): T {
  let ctx: Context;
  try {
    ctx = otelContext.active();
    const o = opts ?? {};
    if (o.conversationId) ctx = ctx.setValue(CONV_KEY, String(o.conversationId));
    if (o.userId) ctx = ctx.setValue(USER_KEY, String(o.userId));
    if (o.tags && Object.keys(o.tags).length) {
      const merged = { ...((ctx.getValue(TAGS_KEY) as Record<string, string>) ?? {}) };
      for (const [k, v] of Object.entries(o.tags)) merged[String(k)] = String(v);
      ctx = ctx.setValue(TAGS_KEY, merged);
    }
    if (o.propagate) {
      let bag = propagation.getBaggage(ctx) ?? propagation.createBaggage();
      if (o.conversationId) bag = bag.setEntry('gen_ai.conversation.id', { value: String(o.conversationId) });
      if (o.userId) bag = bag.setEntry('user.id', { value: String(o.userId) });
      ctx = propagation.setBaggage(ctx, bag);
    }
  } catch {
    return fn();
  }
  return otelContext.with(ctx, fn);
}
