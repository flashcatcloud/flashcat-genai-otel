/**
 * MCP client instrumentation (contract §4.3). In `@modelcontextprotocol/sdk` 1.x, `Client.prototype.callTool` creates
 * a `tools/call {tool name}` CLIENT span with mcp.method.name, gen_ai.operation.name=execute_tool, and gen_ai.tool.name.
 * When content capture is on, record arguments and results for masking. If the result has isError, set error.type=tool_error and status ERROR.
 * Inject W3C traceparent into `_meta` via the Trace Context propagator without baggage so the server can join the trace.
 *
 * SDK 1.32 lacks built-in OTel instrumentation, and OpenInference MCP instrumentation only propagates context, so this implementation creates the spans.
 * Wrap only callTool. Leave the transport alone and copy the caller's params before adding _meta.
 *
 * The root export points to a missing dist/cjs/index.js, so require-in-the-middle cannot resolve the package entry
 * and skips all its files (verified with 1.32.2 on Nov 18, 2026). Require and patch client/index.js directly from the application directory during init.
 * The ESM build is a separate module instance. Import and patch its corresponding dist/esm file too. register provides the same fallback.
 */

import { existsSync } from 'node:fs';
import { createRequire } from 'node:module';
import { sep } from 'node:path';
import { pathToFileURL } from 'node:url';
import { SpanKind, SpanStatusCode, context as otelContext, defaultTextMapSetter, trace, type Span } from '@opentelemetry/api';
import { W3CTraceContextPropagator } from '@opentelemetry/core';
import { InstrumentationBase, isWrapped } from '@opentelemetry/instrumentation';
import { serialize } from './semantics';
import { state } from './state';
import { VERSION } from './version';

export const MCP_SCOPE = 'flashcat.instrumentation.mcp';
export const MCP_CLIENT_SPECIFIER = '@modelcontextprotocol/sdk/client/index.js';
const propagator = new W3CTraceContextPropagator();
// tsup rewrites import() to require() in CJS. Preserve native dynamic import to load the ESM build.
const dynamicImport = new Function('s', 'return import(s)') as (s: string) => Promise<any>;

function resultText(r: any): string {
  if (r?.structuredContent !== undefined) return serialize(r.structuredContent);
  const blocks = r?.content;
  if (Array.isArray(blocks) && blocks.length && blocks.every((b: any) => b?.type === 'text')) {
    return serialize(blocks.map((b: any) => b.text ?? '').join('\n'));
  }
  return serialize(r);
}

function fail(span: Span, err: unknown): void {
  try {
    const code = (err as any)?.code;
    span.setAttribute('error.type', typeof code === 'number' || typeof code === 'string' ? String(code) : (err as any)?.constructor?.name || 'Error');
    if (err instanceof Error) span.recordException(err);
    span.setStatus({ code: SpanStatusCode.ERROR, message: err instanceof Error ? err.message : String(err) });
    span.end();
  } catch {
    // Ignore instrumentation errors.
  }
}

function finish(span: Span, result: any): void {
  try {
    if (state().contentEnabled && result !== undefined) span.setAttribute('gen_ai.tool.call.result', resultText(result));
    if (result?.isError === true) {
      span.setAttribute('error.type', 'tool_error');
      span.setStatus({ code: SpanStatusCode.ERROR });
    }
    span.end();
  } catch {
    // Ignore instrumentation errors.
  }
}

export class McpClientInstrumentation extends InstrumentationBase {
  private readonly patched = new Set<any>();

  constructor(config: Record<string, unknown> = {}) {
    super(MCP_SCOPE, VERSION, config);
  }

  /** instrumentBuilds() and the register fallback patch directly without a require hook. See the file header. */
  protected init() {
    return [];
  }

  /** Patch a loaded client/index.js module (CJS or ESM). Safe to call repeatedly. */
  manuallyInstrument(mod: any): void {
    const proto = mod?.Client?.prototype;
    if (!proto || typeof proto.callTool !== 'function' || isWrapped(proto.callTool)) return;
    const self = this;
    this._wrap(proto, 'callTool', (original: (...a: any[]) => any) =>
      function patchedCallTool(this: unknown, params: any, ...rest: unknown[]) {
        return self.traceCall(this, original, params, rest);
      },
    );
    this.patched.add(proto);
  }

  override disable(): void {
    super.disable();
    for (const proto of this.patched) if (isWrapped(proto.callTool)) this._unwrap(proto, 'callTool');
    this.patched.clear();
  }

  /**
   * Load and patch both builds from the application directory: require CJS synchronously and import the corresponding dist/esm file asynchronously.
   * Patching the prototype also covers clients created before the import completes. Skip missing builds and ignore patch errors.
   */
  instrumentBuilds(bases: string[]): void {
    for (const base of bases) {
      let cjsPath: string;
      try {
        const req = createRequire(base.endsWith(sep) ? base : base + sep);
        cjsPath = req.resolve(MCP_CLIENT_SPECIFIER);
        this.manuallyInstrument(req(MCP_CLIENT_SPECIFIER));
      } catch {
        continue;
      }
      const esmPath = cjsPath.replace(`${sep}dist${sep}cjs${sep}`, `${sep}dist${sep}esm${sep}`);
      if (esmPath !== cjsPath && existsSync(esmPath)) {
        dynamicImport(pathToFileURL(esmPath).href).then(
          (m) => this.manuallyInstrument(m),
          () => undefined,
        );
      }
      return;
    }
  }

  private traceCall(client: unknown, original: (...a: any[]) => any, params: any, rest: unknown[]): unknown {
    let span: Span;
    let ctx;
    let callParams = params;
    try {
      const name = typeof params?.name === 'string' ? params.name : undefined;
      span = this.tracer.startSpan(name ? `tools/call ${name}` : 'tools/call', {
        kind: SpanKind.CLIENT,
        attributes: {
          'mcp.method.name': 'tools/call',
          'gen_ai.operation.name': 'execute_tool',
          ...(name ? { 'gen_ai.tool.name': name } : {}),
        },
      });
      if (state().contentEnabled && params?.arguments !== undefined) {
        span.setAttribute('gen_ai.tool.call.arguments', serialize(params.arguments));
      }
      ctx = trace.setSpan(otelContext.active(), span);
      if (params && typeof params === 'object') {
        const meta: Record<string, unknown> = { ...(params._meta ?? {}) };
        propagator.inject(ctx, meta, defaultTextMapSetter);
        callParams = { ...params, _meta: meta };
      }
    } catch {
      return original.call(client, params, ...rest);
    }
    let p: unknown;
    try {
      p = otelContext.with(ctx, () => original.call(client, callParams, ...rest));
    } catch (e) {
      fail(span, e);
      throw e;
    }
    return Promise.resolve(p).then(
      (r) => {
        finish(span, r);
        return r;
      },
      (e) => {
        fail(span, e);
        throw e;
      },
    );
  }
}
