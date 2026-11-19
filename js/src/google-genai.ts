/**
 * Instrument `@google/genai`, the JS SDK for the Gemini API and Vertex AI. Neither upstream OTel nor OpenInference
 * provides JS instrumentation (npm checked Nov 18, 2026), and SDK 3.6 has none built in. Follow the GenAI semantic conventions:
 *
 * - `generate_content {model}` (CLIENT): gen_ai.operation.name=generate_content, gen_ai.provider.name
 *     (gcp.gemini / gcp.vertex_ai), and gen_ai.request.model, plus response modelVersion, responseId, and finishReason.
 *     For usage, input_tokens is promptTokenCount (which includes cached tokens), and output_tokens sums candidates and thoughts
 *     to match backend accounting for reasoning tokens. Also record cache_read.input_tokens and reasoning.output_tokens.
 *     For streaming, also set gen_ai.request.stream and gen_ai.response.time_to_first_chunk in seconds.
 * - `embeddings {model}` (CLIENT): gen_ai.operation.name=embeddings. Gemini embedding responses omit token counts.
 * - When content capture is on, record gen_ai.input.messages (including systemInstruction) and gen_ai.output.messages. Mask these as usual.
 *
 * The public generateContent, generateContentStream, and embedContent methods are instance arrow functions assigned in the constructor.
 * Wrap the prototype methods that send requests: generateContentInternal, generateContentStreamInternal, and embedContentInternal.
 * This creates one span per request, including each round of automatic function calling (AFC).
 */

import { SpanKind, SpanStatusCode, context as otelContext, trace, type Context, type Span } from '@opentelemetry/api';
import { InstrumentationBase, InstrumentationNodeModuleDefinition, isWrapped } from '@opentelemetry/instrumentation';
import { serialize } from './semantics';
import { state } from './state';
import { VERSION } from './version';

export const GOOGLE_GENAI_SCOPE = 'flashcat.instrumentation.google-genai';

type Part = Record<string, unknown>;
interface Message {
  role: string;
  parts: Part[];
  finish_reason?: string;
}

function partOf(p: any): Part {
  if (typeof p === 'string') return { type: 'text', content: p };
  if (typeof p?.text === 'string') return { type: 'text', content: p.text };
  if (p?.functionCall) return { type: 'tool_call', id: p.functionCall.id, name: p.functionCall.name, arguments: p.functionCall.args };
  if (p?.functionResponse) {
    return { type: 'tool_call_response', id: p.functionResponse.id, response: p.functionResponse.response };
  }
  return { type: 'text', content: serialize(p) };
}

function contentOf(c: any, fallbackRole: string): Message {
  if (c && typeof c === 'object' && Array.isArray(c.parts)) {
    const role = c.role === 'model' ? 'assistant' : c.role || fallbackRole;
    return { role, parts: c.parts.map(partOf) };
  }
  return { role: fallbackRole, parts: [partOf(c)] };
}

/** contents accepts a string, Part, Content, or an array of these. */
function inputMessages(params: any): Message[] {
  const out: Message[] = [];
  const sys = params?.config?.systemInstruction;
  if (sys !== undefined) out.push(contentOf(sys, 'system'));
  if (out[0]) out[0].role = 'system';
  const contents = params?.contents;
  if (contents === undefined) return out;
  const list = Array.isArray(contents) ? contents : [contents];
  // Combine the entire list into one user message if it contains only strings or individual parts.
  if (list.every((c) => typeof c === 'string' || (c && typeof c === 'object' && !Array.isArray(c.parts)))) {
    out.push({ role: 'user', parts: list.map(partOf) });
  } else {
    for (const c of list) out.push(contentOf(c, 'user'));
  }
  return out;
}

function provider(models: any): string {
  try {
    return models?.apiClient?.isVertexAI?.() ? 'gcp.vertex_ai' : 'gcp.gemini';
  } catch {
    return 'gcp.gemini';
  }
}

function startSpan(tracer: any, op: string, models: any, params: any): Span {
  const model = typeof params?.model === 'string' ? params.model.replace(/^models\//, '') : undefined;
  const span: Span = tracer.startSpan(model ? `${op} ${model}` : op, {
    kind: SpanKind.CLIENT,
    attributes: {
      'gen_ai.operation.name': op,
      'gen_ai.provider.name': provider(models),
      ...(model ? { 'gen_ai.request.model': model } : {}),
    },
  });
  if (op === 'generate_content' && state().contentEnabled) {
    try {
      span.setAttribute('gen_ai.input.messages', JSON.stringify(inputMessages(params)));
    } catch {
      // Ignore instrumentation errors.
    }
  }
  return span;
}

function fail(span: Span, err: unknown): void {
  try {
    const status = (err as any)?.status;
    span.setAttribute('error.type', typeof status === 'number' ? String(status) : (err as any)?.constructor?.name || 'Error');
    if (err instanceof Error) span.recordException(err);
    span.setStatus({ code: SpanStatusCode.ERROR, message: err instanceof Error ? err.message : String(err) });
    span.end();
  } catch {
    // Ignore instrumentation errors.
  }
}

const num = (v: unknown) => (typeof v === 'number' && Number.isFinite(v) && v >= 0 ? v : undefined);

/** Record the response on the span. Streaming calls supply the assembled response. */
interface Collected {
  usage?: any;
  modelVersion?: string;
  responseId?: string;
  finishReasons: string[];
  outputs: Message[];
}

function collect(c: Collected, resp: any, append: boolean): void {
  if (!resp || typeof resp !== 'object') return;
  if (resp.usageMetadata) c.usage = resp.usageMetadata;
  if (typeof resp.modelVersion === 'string') c.modelVersion = resp.modelVersion;
  if (typeof resp.responseId === 'string') c.responseId = resp.responseId;
  const cands = Array.isArray(resp.candidates) ? resp.candidates : [];
  cands.forEach((cand: any, i: number) => {
    const idx = typeof cand?.index === 'number' ? cand.index : i;
    if (typeof cand?.finishReason === 'string') c.finishReasons[idx] = cand.finishReason;
    if (!state().contentEnabled) return;
    const parts: Part[] = Array.isArray(cand?.content?.parts) ? cand.content.parts.map(partOf) : [];
    const prev = c.outputs[idx];
    if (append && prev) {
      // Join adjacent text chunks and append other chunks as received.
      for (const p of parts) {
        const last = prev.parts[prev.parts.length - 1];
        if (p.type === 'text' && last?.type === 'text') last.content = String(last.content ?? '') + String(p.content ?? '');
        else prev.parts.push(p);
      }
    } else {
      c.outputs[idx] = { role: 'assistant', parts };
    }
  });
}

function finish(span: Span, c: Collected): void {
  try {
    const u = c.usage ?? {};
    const prompt = num(u.promptTokenCount);
    const candidates = num(u.candidatesTokenCount);
    const thoughts = num(u.thoughtsTokenCount);
    const cached = num(u.cachedContentTokenCount);
    if (prompt !== undefined) span.setAttribute('gen_ai.usage.input_tokens', prompt);
    if (candidates !== undefined || thoughts !== undefined) {
      span.setAttribute('gen_ai.usage.output_tokens', (candidates ?? 0) + (thoughts ?? 0));
    }
    if (thoughts) span.setAttribute('gen_ai.usage.reasoning.output_tokens', thoughts);
    if (cached) span.setAttribute('gen_ai.usage.cache_read.input_tokens', cached);
    if (c.modelVersion) span.setAttribute('gen_ai.response.model', c.modelVersion);
    if (c.responseId) span.setAttribute('gen_ai.response.id', c.responseId);
    const reasons = c.finishReasons.filter(Boolean);
    if (reasons.length) span.setAttribute('gen_ai.response.finish_reasons', reasons);
    if (state().contentEnabled) {
      const outputs = c.outputs
        .map((m, i) => (m ? { ...m, ...(c.finishReasons[i] ? { finish_reason: c.finishReasons[i] } : {}) } : undefined))
        .filter(Boolean);
      if (outputs.length) span.setAttribute('gen_ai.output.messages', JSON.stringify(outputs));
    }
    span.end();
  } catch {
    // Ignore instrumentation errors.
  }
}

function hasOutput(chunk: any): boolean {
  const cands = Array.isArray(chunk?.candidates) ? chunk.candidates : [];
  return cands.some((cand: any) => Array.isArray(cand?.content?.parts) && cand.content.parts.length > 0);
}

/** Observe the stream and yield chunks unchanged. End the span on exhaustion, early return, or error. */
async function* observeStream(gen: AsyncIterable<any>, span: Span, ctx: Context, startedAt: number): AsyncGenerator<any> {
  const c: Collected = { finishReasons: [], outputs: [] };
  let first = false;
  let ended = false;
  try {
    for await (const chunk of gen) {
      if (!first && hasOutput(chunk)) {
        first = true;
        span.setAttribute('gen_ai.response.time_to_first_chunk', (performance.now() - startedAt) / 1000);
      }
      try {
        collect(c, chunk, true);
      } catch {
        // Ignore instrumentation errors.
      }
      yield chunk;
    }
  } catch (e) {
    ended = true;
    fail(span, e);
    throw e;
  } finally {
    if (!ended) finish(span, c);
  }
  void ctx;
}

export class GoogleGenAIInstrumentation extends InstrumentationBase {
  constructor(config: Record<string, unknown> = {}) {
    super(GOOGLE_GENAI_SCOPE, VERSION, config);
  }

  protected init() {
    return [
      new InstrumentationNodeModuleDefinition(
        '@google/genai',
        ['>=1.0.0'],
        (exports: any) => {
          this.patch(exports);
          return exports;
        },
        (exports: any) => {
          this.unpatch(exports);
          return exports;
        },
      ),
    ];
  }

  /** Idempotent ESM fallback used by register.ts. */
  manuallyInstrument(mod: any): void {
    this.patch(mod);
  }

  private patch(exports: any): void {
    const proto = exports?.Models?.prototype;
    if (!proto) return;
    const self = this;
    if (typeof proto.generateContentInternal === 'function' && !isWrapped(proto.generateContentInternal)) {
      this._wrap(proto, 'generateContentInternal', (original: (...a: any[]) => any) =>
        function (this: any, params: any, ...rest: unknown[]) {
          return self.traceUnary(this, original, 'generate_content', params, rest);
        },
      );
    }
    if (typeof proto.embedContentInternal === 'function' && !isWrapped(proto.embedContentInternal)) {
      this._wrap(proto, 'embedContentInternal', (original: (...a: any[]) => any) =>
        function (this: any, params: any, ...rest: unknown[]) {
          return self.traceUnary(this, original, 'embeddings', params, rest);
        },
      );
    }
    if (typeof proto.generateContentStreamInternal === 'function' && !isWrapped(proto.generateContentStreamInternal)) {
      this._wrap(proto, 'generateContentStreamInternal', (original: (...a: any[]) => any) =>
        function (this: any, params: any, ...rest: unknown[]) {
          return self.traceStream(this, original, params, rest);
        },
      );
    }
  }

  private unpatch(exports: any): void {
    const proto = exports?.Models?.prototype;
    if (!proto) return;
    for (const m of ['generateContentInternal', 'embedContentInternal', 'generateContentStreamInternal']) {
      if (isWrapped(proto[m])) this._unwrap(proto, m);
    }
  }

  private traceUnary(models: any, original: (...a: any[]) => any, op: string, params: any, rest: unknown[]): unknown {
    let span: Span;
    let ctx: Context;
    try {
      span = startSpan(this.tracer, op, models, params);
      ctx = trace.setSpan(otelContext.active(), span);
    } catch {
      return original.call(models, params, ...rest);
    }
    let p: unknown;
    try {
      p = otelContext.with(ctx, () => original.call(models, params, ...rest));
    } catch (e) {
      fail(span, e);
      throw e;
    }
    return Promise.resolve(p).then(
      (resp) => {
        const c: Collected = { finishReasons: [], outputs: [] };
        try {
          collect(c, resp, false);
        } catch {
          // Ignore instrumentation errors.
        }
        finish(span, c);
        return resp;
      },
      (e) => {
        fail(span, e);
        throw e;
      },
    );
  }

  private traceStream(models: any, original: (...a: any[]) => any, params: any, rest: unknown[]): unknown {
    let span: Span;
    let ctx: Context;
    const startedAt = performance.now();
    try {
      span = startSpan(this.tracer, 'generate_content', models, params);
      span.setAttribute('gen_ai.request.stream', true);
      ctx = trace.setSpan(otelContext.active(), span);
    } catch {
      return original.call(models, params, ...rest);
    }
    let p: unknown;
    try {
      p = otelContext.with(ctx, () => original.call(models, params, ...rest));
    } catch (e) {
      fail(span, e);
      throw e;
    }
    return Promise.resolve(p).then(
      (gen: any) => (gen && typeof gen[Symbol.asyncIterator] === 'function' ? observeStream(gen, span, ctx, startedAt) : (finish(span, { finishReasons: [], outputs: [] }), gen)),
      (e) => {
        fail(span, e);
        throw e;
      },
    );
  }
}
