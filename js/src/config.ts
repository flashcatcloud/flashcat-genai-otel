/**
 * Configuration precedence (contract §1). Explicit arguments > FLASHCAT_* > standard OTEL_* > defaults.
 * Matches Python's `_config.py` except where documented below.
 */

import type { MaskingConfig } from './processors';

export type CaptureContent = false | 'span';
export type ExporterMode = 'auto' | 'add' | 'reuse' | 'none';

export interface InitOptions {
  /** Application name. Also configurable via FLASHCAT_SERVICE_NAME or OTEL_SERVICE_NAME. */
  serviceName?: string;
  /** Environment. Defaults to prod. */
  environment?: string;
  /** Full trace ingest endpoint, including /v1/traces. */
  endpoint?: string;
  /** Credentials in keyID:secret form. The SDK handles Basic encoding. */
  apiKey?: string;
  /** auto (default) / add / reuse / none */
  exporter?: ExporterMode;
  /** Content capture switch. false (default) disables capture. 'span' writes content to span attributes. Aliases: 'off' / 'no_content' / 'span_only'. */
  captureContent?: CaptureContent | 'off' | 'no_content' | 'span_only' | 'false';
  /** Load only these instrumentation sources (catalog lib names). Omit to load all. */
  instrumentations?: string[];
  /** Instrumentation sources to skip. */
  disabledInstrumentations?: string[];
  /** Whether to configure the MeterProvider as well. Defaults to true. */
  metrics?: boolean;
  /** Masking and truncation options. */
  masking?: MaskingConfig;
  /** Additional resource attributes for scenario A. */
  resourceAttributes?: Record<string, string>;
  /** Export timeout in milliseconds. Defaults to 10000. */
  exportTimeoutMs?: number;
  /** Total exit deadline in milliseconds for flushing and shutting down traces and metrics. Defaults to 5000. FLASHCAT_SHUTDOWN_TIMEOUT takes seconds. */
  shutdownTimeoutMs?: number;
  /** Log full diagnostics. */
  debug?: boolean;
  /** If another instrumentation has patched the library, 'foreign' (default) defers to it and 'flashcat' installs ours anyway. */
  prefer?: 'foreign' | 'flashcat';
}

export class ConfigError extends Error {
  /** init throws for invalid arguments so development catches the error (the sole exception in contract §2). */
  constructor(message: string) {
    super(message);
    this.name = 'ConfigError';
  }
}

// contract §1 allows only false / span and their aliases in the newer SDKs.
const CONTENT_OFF = new Set(['false', '0', 'no', 'off', 'no_content']);
const CONTENT_SPAN = new Set(['span', 'span_only']);
// Event modes record a problem and disable content capture.
const CONTENT_EVENT = new Set(['event', 'event_only', 'span_and_event']);

export interface Settings {
  serviceName: string | undefined;
  environment: string;
  endpoint: string | undefined;
  metricsEndpoint: string | undefined;
  apiKey: string | undefined;
  authHeader: string | undefined;
  exporter: ExporterMode;
  captureContent: CaptureContent;
  instrumentations: string[] | undefined;
  disabledInstrumentations: string[];
  metrics: boolean;
  resourceAttributes: Record<string, string>;
  exportTimeoutMs: number;
  shutdownTimeoutMs: number;
  debug: boolean;
  prefer: 'foreign' | 'flashcat';
  disabled: boolean;
  knownHosts: string[];
  problems: string[];
}

function env(name: string): string | undefined {
  const v = process.env[name];
  return v && v.trim() ? v.trim() : undefined;
}

/** Parse OTEL_RESOURCE_ATTRIBUTES / OTEL_EXPORTER_OTLP_HEADERS as k=v,k=v with URL-encoded values. */
export function parseKvList(raw: string | undefined): Record<string, string> {
  const out: Record<string, string> = {};
  if (!raw) return out;
  for (const part of raw.split(',')) {
    const i = part.indexOf('=');
    if (i < 0) continue;
    const k = part.slice(0, i).trim();
    let v = part.slice(i + 1).trim();
    try {
      v = decodeURIComponent(v);
    } catch {
      // Keep the original value.
    }
    out[k] = v;
  }
  return out;
}

function repr(v: unknown): string {
  return typeof v === 'string' ? `'${v}'` : String(v);
}

/** Explicit true and unknown values throw ConfigError. Event modes return 'event' so the caller can report a problem. */
export function parseContent(value: unknown): CaptureContent | 'event' {
  if (value === false || value === undefined || value === null) return false;
  if (value === true) {
    throw new ConfigError('captureContent=true 有歧义，请用 \'span\'（写进 span 属性）或 false');
  }
  if (typeof value === 'string') {
    const v = value.trim().toLowerCase();
    if (CONTENT_OFF.has(v)) return false;
    if (CONTENT_SPAN.has(v)) return 'span';
    if (CONTENT_EVENT.has(v)) return 'event';
  }
  throw new ConfigError(`captureContent 只能是 false / 'span'，收到 ${repr(value)}`);
}

function truthy(v: string | undefined): boolean {
  return ['1', 'true', 'yes', 'on'].includes((v ?? '').toLowerCase());
}

function urlHost(u: string): string {
  try {
    return new URL(u).hostname.toLowerCase();
  } catch {
    return '';
  }
}

export function resolve(opts: InitOptions): Settings {
  const problems: string[] = [];
  const exporter = opts.exporter ?? 'auto';
  if (!['auto', 'add', 'reuse', 'none'].includes(exporter)) {
    throw new ConfigError(`exporter 只能是 'auto' / 'add' / 'reuse' / 'none'，收到 ${repr(exporter)}`);
  }
  const prefer = opts.prefer ?? 'foreign';
  if (prefer !== 'foreign' && prefer !== 'flashcat') {
    throw new ConfigError(`prefer 只能是 'foreign' / 'flashcat'，收到 ${repr(prefer)}`);
  }
  for (const k of ['instrumentations', 'disabledInstrumentations'] as const) {
    if (opts[k] !== undefined && !Array.isArray(opts[k])) {
      throw new ConfigError(`${k} 必须是字符串数组`);
    }
  }
  for (const k of ['exportTimeoutMs', 'shutdownTimeoutMs'] as const) {
    const v = opts[k];
    if (v !== undefined && (typeof v !== 'number' || !Number.isFinite(v) || v <= 0)) {
      throw new ConfigError(`${k} 必须是正数（毫秒），收到 ${repr(v)}`);
    }
  }

  // An explicit content capture setting, including false, takes precedence over environment variables.
  let content: CaptureContent | 'event' | undefined;
  let contentRaw: string | undefined;
  if (opts.captureContent !== undefined) {
    content = parseContent(opts.captureContent);
    contentRaw = String(opts.captureContent);
  } else {
    const fc = env('FLASHCAT_CAPTURE_CONTENT');
    const oc = env('OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT');
    if (fc !== undefined) {
      contentRaw = fc;
      try {
        content = parseContent(fc);
      } catch {
        // Unlike Python, record invalid environment values as problems and disable capture. Only explicit arguments can throw (contract §2.1).
        problems.push(`FLASHCAT_CAPTURE_CONTENT 只能是 false / span，收到 ${repr(fc)}；已按不采正文处理`);
        content = false;
      }
    } else if (oc !== undefined) {
      contentRaw = oc;
      const v = oc.toLowerCase();
      // Upstream JS OpenAI and OpenAI Agents instrumentation reads this variable as a boolean, with true enabling capture.
      // Treat true as span here. Python has no such alias. As in Python, other unknown values disable capture.
      if (v === 'true' || CONTENT_SPAN.has(v)) content = 'span';
      else if (CONTENT_EVENT.has(v)) content = 'event';
      else content = false;
    }
  }
  if (content === 'event') {
    problems.push(
      `正文的 event 类取值（${contentRaw}）在 Node 版不支持：Flashcat 只关联 span 属性里的正文；已按不采处理，需要正文请用 'span'`,
    );
    content = false;
  }

  const resEnv = parseKvList(env('OTEL_RESOURCE_ATTRIBUTES'));
  const svc =
    opts.serviceName || env('FLASHCAT_SERVICE_NAME') || env('OTEL_SERVICE_NAME') || resEnv['service.name'] || undefined;
  const environment =
    opts.environment ||
    env('FLASHCAT_ENVIRONMENT') ||
    resEnv['deployment.environment.name'] ||
    resEnv['deployment.environment'] ||
    'prod';

  let ep: string | undefined = opts.endpoint || env('FLASHCAT_ENDPOINT') || env('OTEL_EXPORTER_OTLP_TRACES_ENDPOINT');
  if (ep === undefined) {
    const base = env('OTEL_EXPORTER_OTLP_ENDPOINT');
    if (base) ep = base.replace(/\/+$/, '') + '/v1/traces';
  }
  if (ep !== undefined) {
    let parsed: URL | undefined;
    try {
      parsed = new URL(ep);
    } catch {
      parsed = undefined;
    }
    if (!parsed || (parsed.protocol !== 'http:' && parsed.protocol !== 'https:') || !parsed.host) {
      problems.push(
        `endpoint 必须是完整 URL（含 http(s):// 与 /v1/traces），收到 ${repr(ep)}；不自动补端口，以免落到无鉴权入口`,
      );
      ep = undefined;
    } else if (!parsed.pathname || parsed.pathname === '/') {
      problems.push(`endpoint 缺少路径，应为 .../v1/traces，收到 ${repr(ep)}`);
      ep = undefined;
    }
  }

  let metricsEp = env('FLASHCAT_METRICS_ENDPOINT') || env('OTEL_EXPORTER_OTLP_METRICS_ENDPOINT');
  if (metricsEp === undefined && ep && ep.endsWith('/v1/traces')) {
    metricsEp = ep.slice(0, -'/v1/traces'.length) + '/v1/metrics';
  }

  let key: string | undefined = opts.apiKey || env('FLASHCAT_API_KEY');
  let authHeader: string | undefined;
  if (key === undefined) {
    const headers = parseKvList(env('OTEL_EXPORTER_OTLP_TRACES_HEADERS') || env('OTEL_EXPORTER_OTLP_HEADERS'));
    for (const [k, v] of Object.entries(headers)) {
      if (k.toLowerCase() === 'authorization') authHeader = v;
    }
  } else if (!key.includes(':')) {
    problems.push('api_key 应为 keyID:secret（不要传 base64，也不要传 URL 编码的 Authorization）');
    key = undefined;
  }

  const disabled = truthy(env('FLASHCAT_DISABLED')) || truthy(env('OTEL_SDK_DISABLED'));
  const disabledInsts = [...(opts.disabledInstrumentations ?? [])];
  // OTEL_NODE_DISABLED_INSTRUMENTATIONS is the JS equivalent of OTEL_PYTHON_DISABLED_INSTRUMENTATIONS.
  for (const name of (env('OTEL_NODE_DISABLED_INSTRUMENTATIONS') ?? '').split(',')) {
    if (name.trim()) disabledInsts.push(name.trim());
  }

  // Shutdown timeout precedence: explicit argument > FLASHCAT_SHUTDOWN_TIMEOUT (fractional seconds allowed) > 5 seconds. Invalid environment values record a problem and fall back to the default.
  let shutdownTimeoutMs = opts.shutdownTimeoutMs;
  const shutdownRaw = env('FLASHCAT_SHUTDOWN_TIMEOUT');
  if (shutdownTimeoutMs === undefined && shutdownRaw !== undefined) {
    const seconds = Number(shutdownRaw);
    if (Number.isFinite(seconds) && seconds > 0) shutdownTimeoutMs = seconds * 1000;
    else problems.push(`FLASHCAT_SHUTDOWN_TIMEOUT 必须是正数（秒，可带小数），收到 ${repr(shutdownRaw)}；已按默认 5 秒处理`);
  }

  const known = (env('FLASHCAT_KNOWN_HOSTS') ?? '')
    .split(',')
    .map((h) => h.trim().toLowerCase())
    .filter(Boolean);
  if (ep) known.push(urlHost(ep));

  return {
    serviceName: svc,
    environment,
    endpoint: ep,
    metricsEndpoint: metricsEp,
    apiKey: key,
    authHeader,
    exporter,
    captureContent: content ?? false,
    instrumentations: opts.instrumentations ? [...opts.instrumentations] : undefined,
    disabledInstrumentations: disabledInsts,
    metrics: opts.metrics ?? true,
    resourceAttributes: { ...(opts.resourceAttributes ?? {}) },
    exportTimeoutMs: opts.exportTimeoutMs ?? 10_000,
    shutdownTimeoutMs: shutdownTimeoutMs ?? 5_000,
    debug: !!opts.debug,
    prefer,
    disabled,
    knownHosts: known.filter(Boolean),
    problems,
  };
}

export { urlHost };
