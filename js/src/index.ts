/**
 * Flashcat GenAI OTel SDK for Node.js, with one-line setup for AI agent observability.
 *
 *     import * as otel from '@flashcat/genai-otel';
 *     otel.init({ serviceName: 'kefu-agent' });
 *
 * See spec/contract.md for the cross-language contract.
 */

import { trace as traceApi } from '@opentelemetry/api';
import type { SpanProcessor } from '@opentelemetry/sdk-trace';
import { resolve, type InitOptions, type Settings } from './config';
import { load, silentGapProblems } from './loader';
import { log } from './log';
import { openDeadline, type Deadline } from './deadline';
import { compileMasking } from './processors';
import { watchUnpatched } from './unpatched';
import { buildProcessor, setupMetrics, setupTracing } from './providers';
import { state } from './state';
import { VERSION } from './version';

export { agent, tool, workflow, llm, embedding, retrieval, annotate, trace, session } from './semantics';
export type { AgentOptions, ToolOptions, ModelOptions, Usage, AnnotateOptions, SessionOptions } from './semantics';
export { ConfigError } from './config';
export type { InitOptions, CaptureContent, ExporterMode } from './config';
export type { MaskingConfig, MaskingRule } from './processors';
export { VERSION as version };

/** Return structure of init() and diagnostics(). Field names match across all four languages (contract §8). */
export interface Diagnostics {
  sdk: { name: 'flashcat-genai-otel'; version: string; language: 'nodejs' };
  enabled: boolean;
  service_name: string | null;
  environment: string | null;
  endpoint: string | null;
  problems: string[];
  notes: string[];
  provider: { situation: 'A' | 'B' | 'C' | 'unsupported' | null; exporter_mode: string; metrics: string };
  content: Record<string, string>;
  instrumentations: Array<{
    lib: string;
    source: string;
    status: string;
    lib_version?: string;
    hint?: string;
    supported?: string;
    error?: string;
    foreign?: string[];
  }>;
  export: Record<string, number | string | null>;
  semantics: { orphan_inference_spans: number; nested_inference_spans: number; masked_spans: number; truncated_spans: number };
}

/**
 * Options for spanProcessor(), limited to the export pipeline settings from InitOptions.
 * init() or the environment sets content capture for all instrumentation sources. The processor reads the setting in onEnd.
 */
export type SpanProcessorOptions = Pick<InitOptions, 'endpoint' | 'apiKey' | 'masking' | 'exportTimeoutMs'>;

function baseDiag(s: Settings | undefined): Diagnostics {
  return {
    sdk: { name: 'flashcat-genai-otel', version: VERSION, language: 'nodejs' },
    enabled: false,
    service_name: s?.serviceName ?? null,
    environment: s?.environment ?? null,
    endpoint: s?.endpoint ?? null,
    problems: [...(s?.problems ?? [])],
    notes: [],
    provider: { situation: null, exporter_mode: 'none', metrics: 'none' },
    content: {},
    instrumentations: [],
    export: {},
    semantics: { orphan_inference_spans: 0, nested_inference_spans: 0, masked_spans: 0, truncated_spans: 0 },
  };
}

/** Apply the content capture switch to all sources (contract §7). Run before loading instrumentation because instrumentation and Anthropic clients read the environment when constructed. */
function applyContent(s: Settings, diag: Diagnostics): void {
  const on = s.captureContent === 'span';
  // Upstream JS instrumentation reads only true / false and gives the environment precedence over constructor options.
  process.env.OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT = on ? 'true' : 'false';
  const content: Record<string, string> = { upstream: on ? 'SPAN_ONLY' : 'NO_CONTENT' };
  if (process.env.ANTHROPIC_OPEN_TELEMETRY_TRACES_CONTENT_MODE === undefined) {
    process.env.ANTHROPIC_OPEN_TELEMETRY_TRACES_CONTENT_MODE = on ? 'content' : 'metadata_only';
  }
  content.anthropic = process.env.ANTHROPIC_OPEN_TELEMETRY_TRACES_CONTENT_MODE.toLowerCase() === 'content' ? 'content' : 'metadata_only';
  for (const k of ['OPENINFERENCE_HIDE_INPUTS', 'OPENINFERENCE_HIDE_OUTPUTS']) {
    if (process.env[k] === undefined) process.env[k] = on ? 'false' : 'true';
  }
  content.openinference = 'best_effort:hide=' + process.env.OPENINFERENCE_HIDE_INPUTS;
  content.vercel = on ? 'sdk_default' : 'recordInputs=false,recordOutputs=false';
  diag.content = content;
  state().contentEnabled = on;
}

/** Report the effective content capture setting for each source (contract §7). */
function contentNotes(diag: Diagnostics): void {
  const active = (lib: string, source: string) =>
    diag.instrumentations.some((i) => i.lib === lib && i.source === source && (i.status === 'installed' || i.status === 'loaded_before_init'));
  if (active('openai', 'upstream')) {
    // Upstream emits content as log events. bridgeContentEvents in openai-extras.ts copies it to span attributes.
    diag.content.openai = 'log_events_bridged';
  }
  if (active('anthropic', 'upstream')) {
    diag.content.anthropic_upstream = 'not_supported';
    diag.notes.push('@anthropic-ai/sdk <0.134 走上游 instrumentation-anthropic：它不采正文（token 用量由 Flashcat 补上）；需要正文时升级到 0.134 以上改用 SDK 自带埋点');
  }
}

function emit(diag: Diagnostics, debug: boolean): void {
  if (debug) {
    log.info('diagnostics: ' + JSON.stringify(diag));
    return;
  }
  const brief = {
    service: diag.service_name,
    enabled: diag.enabled,
    provider: diag.provider,
    installed: diag.instrumentations.filter((i) => i.status === 'installed').map((i) => i.lib),
    problems: diag.problems,
  };
  log.info(JSON.stringify(brief));
}

/**
 * Initialize the SDK synchronously and return diagnostics. On failure, record diagnostics and fall back.
 * Invalid explicit arguments throw ConfigError. Without --import, call init before importing model SDKs.
 */
export function init(options: InitOptions = {}): Diagnostics {
  const st = state();
  if (st.initialized) {
    log.warn('init 已调用过，本次忽略（OTel 不允许替换已设置的全局 TracerProvider）');
    return diagnostics();
  }
  // Throw ConfigError before marking the SDK initialized so callers can retry with corrected arguments.
  const s = resolve(options ?? {});
  const masking = compileMasking(options?.masking);

  const diag = baseDiag(s);
  st.initialized = true;
  st.diag = diag;
  st.shutdownTimeoutMs = s.shutdownTimeoutMs;
  try {
    if (s.disabled) {
      diag.notes.push('FLASHCAT_DISABLED / OTEL_SDK_DISABLED 已开启，未做任何事');
      emit(diag, s.debug);
      return diagnostics();
    }

    // Set the content capture switch before loading instrumentation.
    applyContent(s, diag);

    let res;
    try {
      res = setupTracing(s, masking);
      setupMetrics(s, res);
    } catch (e) {
      diag.problems.push(`初始化 provider 失败：${(e as Error)?.name}: ${(e as Error)?.message}`);
      emit(diag, s.debug);
      return diagnostics();
    }
    // Validate the application name after determining the provider scenario. In B and C, the existing provider supplies service.name.
    if (!res.serviceName || res.serviceName.startsWith('unknown_service')) {
      diag.problems.push('缺少 service_name（参数或 OTEL_SERVICE_NAME），且不能是 unknown_service*；未启用导出');
    }
    diag.problems.push(...res.problems);
    diag.notes.push(...res.notes);
    diag.enabled = res.exporterMode !== 'none' || res.situation === 'C';
    diag.service_name = res.serviceName ?? null;
    diag.provider = { situation: res.situation, exporter_mode: res.exporterMode, metrics: res.metricsMode };

    try {
      diag.instrumentations = load({
        only: s.instrumentations,
        disabled: s.disabledInstrumentations,
        prefer: s.prefer,
        contentEnabled: s.captureContent === 'span',
      });
    } catch (e) {
      diag.problems.push(`装载插桩失败：${(e as Error)?.name}: ${(e as Error)?.message}`);
      diag.instrumentations = [];
    }
    // Report missing instrumentation and unsupported versions in problems and log a WARN (contract §8).
    diag.problems.push(...silentGapProblems(diag.instrumentations));
    // Change the status to not_patched if runtime use confirms the patch failed, for example after bundling.
    try {
      watchUnpatched();
    } catch {
      // Monitoring failures should leave initialization unaffected.
    }
    if (s.captureContent === 'span') contentNotes(diag);

    installExitHooks();
  } catch (e) {
    diag.problems.push(`初始化失败：${(e as Error)?.name}: ${(e as Error)?.message}`);
  }
  emit(diag, s.debug);
  return diagnostics();
}

/** Return the same diagnostics as init with live export counters. FlashAI and coding agents rely on stable field names. */
export function diagnostics(): Diagnostics {
  const st = state();
  const d: Diagnostics = st.diag ? structuredClone(st.diag) as Diagnostics : baseDiag(undefined);
  if (!st.diag) d.problems.push('init 尚未调用');
  for (const p of st.pendingProblems) if (!d.problems.includes(p)) d.problems.push(p);
  d.export = st.stats.snapshot();
  d.semantics = {
    orphan_inference_spans: st.orphans.value,
    nested_inference_spans: st.nested.value,
    masked_spans: st.masked.value,
    truncated_spans: st.truncated.value,
  };
  return d;
}

/**
 * Return Flashcat's SpanProcessor for an existing NodeSDK, with session processing, masking, batching, and export.
 *
 *     new NodeSDK({ spanProcessors: [otel.spanProcessor()] }).start();
 *     otel.init({ exporter: 'reuse' });
 *
 * Use the same precedence as init (explicit arguments > FLASHCAT_* > OTEL_*). If the endpoint or credentials are missing,
 * run only session processing and report the reason in diagnostics.problems.
 */
export function spanProcessor(options: SpanProcessorOptions = {}): SpanProcessor {
  const st = state();
  const { endpoint, apiKey, masking: maskingOpt, exportTimeoutMs } = options ?? {};
  const s = resolve({ endpoint, apiKey, masking: maskingOpt, exportTimeoutMs });
  const masking = compileMasking(options?.masking);
  st.userProcessor = true;
  if (!st.initialized) st.contentEnabled = s.captureContent === 'span';
  const notes: string[] = [];
  const { processor } = buildProcessor(s, masking, notes);
  for (const p of [...s.problems, ...notes.map((n) => `spanProcessor(): ${n}`)]) {
    if (!st.pendingProblems.includes(p)) st.pendingProblems.push(p);
  }
  return processor;
}

/** Flush the Flashcat export pipeline (SDK-owned provider or spanProcessor()) and the global TracerProvider within the deadline. */
function flushWithin(dl: Deadline, withMeter: boolean): Promise<boolean> {
  const st = state();
  const tasks: Promise<unknown>[] = [];
  const seen = new Set<unknown>();
  const add = (p: any) => {
    if (!p || seen.has(p) || typeof p.forceFlush !== 'function') return;
    seen.add(p);
    try {
      // TracerProvider and MeterProvider accept timeoutMillis. Processors ignore it.
      tasks.push(Promise.resolve(p.forceFlush({ timeoutMillis: Math.max(1, dl.remaining()) })));
    } catch (e) {
      tasks.push(Promise.reject(e));
    }
  };
  if (st.ownTracerProvider) add(st.ownTracerProvider);
  else for (const p of st.processors) add(p);
  try {
    const g: any = traceApi.getTracerProvider();
    add(typeof g?.getDelegate === 'function' ? g.getDelegate() : g);
  } catch {
    // Ignore failures while looking up the global tracer provider.
  }
  if (withMeter && st.ownMeterProvider) add(st.ownMeterProvider);
  st.endedSinceExitFlush = 0;
  return dl.wait(Promise.all(tasks));
}

/**
 * Export ended spans and metrics. Call before a script or serverless function exits. Return false on timeout or failure.
 * timeoutMs defaults to shutdownTimeoutMs. Abort outstanding requests at the deadline (contract §9.4).
 */
export async function flush(timeoutMs?: number): Promise<boolean> {
  const dl = openDeadline(timeoutMs ?? state().shutdownTimeoutMs);
  try {
    return await flushWithin(dl, true);
  } finally {
    dl.close();
  }
}

/**
 * Flush, then close only the providers this SDK created (scenario A).
 * timeoutMs sets one deadline for flushing and shutting down traces and metrics (contract §9). It defaults to shutdownTimeoutMs.
 * Run once and return the same result on subsequent calls.
 */
export function shutdown(timeoutMs?: number): Promise<boolean> {
  const st = state();
  if (st.shutdownPromise) return st.shutdownPromise;
  st.shutdownPromise = (async () => {
    const dl = openDeadline(timeoutMs ?? st.shutdownTimeoutMs);
    try {
      // Yield one event loop turn so spans still wrapping up on a promise chain can end, such as an OpenInference
      // streaming span ending on the other tee branch. The provider drops spans that end after shutdown starts.
      await new Promise((r) => setImmediate(r));
      // Flush traces, then shut down the SDK-owned provider to export spans that ended during the flush. Only flush external providers.
      const traces = flushWithin(dl, false).then(async (ok) => {
        const tp: any = st.ownTracerProvider;
        if (tp) await tp.shutdown();
        if (!ok) throw new Error('flush 未在时限内完成');
      });
      // MetricReader.shutdown already exports the final batch. Calling forceFlush first would export twice
      // and double the delay when the endpoint is unreachable (the main cause of the 15.9-second exit measured Nov 16, 2026).
      const mp: any = st.ownMeterProvider;
      const meter = mp ? Promise.resolve().then(() => mp.shutdown({ timeoutMillis: Math.max(1, dl.remaining()) })) : undefined;
      st.endedSinceExitFlush = 0;
      return await dl.wait(Promise.all([traces, meter]));
    } finally {
      // Keep an expired deadline for SDK-owned providers so any export started later during shutdown is dropped immediately.
      // Release it in scenarios B and C, where the external provider can still use Flashcat's processor.
      dl.close(!!st.ownTracerProvider);
    }
  })();
  return st.shutdownPromise;
}

/**
 * Flush and shut down on process exit within shutdownTimeoutMs (contract §9).
 * - beforeExit: Shut down once the event loop empties. Shutdown runs the loop again, so ignore later beforeExit events.
 *     Skip shutdown if no new spans ended to avoid an extra turn in an otherwise idle process.
 * - SIGTERM: A listener overrides Node's default exit. If ours is the only listener, remove it and re-emit SIGTERM after shutdown.
 *     If the application has its own listener, only flush so the application can decide when to exit.
 */
function installExitHooks(): void {
  const st = state();
  if (st.exitHooksInstalled) return;
  st.exitHooksInstalled = true;
  process.on('beforeExit', () => {
    const cur = state();
    if (!cur.shutdownPromise && cur.endedSinceExitFlush > 0) void shutdown(cur.shutdownTimeoutMs);
  });
  const onTerm = () => {
    const cur = state();
    if (process.listenerCount('SIGTERM') === 1) {
      void shutdown(cur.shutdownTimeoutMs).finally(() => {
        process.removeListener('SIGTERM', onTerm);
        process.kill(process.pid, 'SIGTERM');
      });
    } else {
      void flush(cur.shutdownTimeoutMs);
    }
  };
  process.on('SIGTERM', onTerm);
}
