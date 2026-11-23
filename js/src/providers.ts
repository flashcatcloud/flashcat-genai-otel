/**
 * TracerProvider and MeterProvider scenarios (contract §2.4).
 *
 * In OTel JS's @opentelemetry/sdk-trace 3.1.1 (verified Nov 12, 2026),
 * NodeTracerProvider and its register() method are absent. sdk-trace-node is still at 2.12.0
 * and requires @opentelemetry/api <1.10. Scenario A uses TracerProvider from sdk-trace
 * and registers AsyncLocalStorageContextManager and the W3C propagator directly.
 */

import { context, metrics, propagation, trace } from '@opentelemetry/api';
import { AsyncLocalStorageContextManager } from '@opentelemetry/context-async-hooks';
import { CompositePropagator, W3CBaggagePropagator, W3CTraceContextPropagator } from '@opentelemetry/core';
import { defaultResource, detectResources, resourceFromAttributes, resourceAttributesEnvDetector, serviceNameEnvDetector, type Resource } from '@opentelemetry/resources';
import { BatchSpanProcessor, TracerProvider } from '@opentelemetry/sdk-trace';
import type { Settings } from './config';
import { urlHost } from './config';
import { basicAuth, metricExporter, spanExporter } from './export';
import { FlashcatSpanProcessor, MaskingProcessor, SessionProcessor, type CompiledMasking } from './processors';
import { localRequire } from './req';
import { state } from './state';
import { VERSION } from './version';

export const SITUATION_B_PROBLEM =
  "进程里已有 TracerProvider，OTel JS 不支持事后追加处理器。请把 `otel.spanProcessor()` 加进 NodeSDK 的 `spanProcessors`，并用 `init({ exporter: 'reuse' })`";

export interface ProviderResult {
  situation: 'A' | 'B' | 'C' | 'unsupported';
  exporterMode: 'add' | 'reuse' | 'none';
  metricsMode: string;
  serviceName: string | undefined;
  notes: string[];
  problems: string[];
}

export function authHeader(s: Pick<Settings, 'apiKey' | 'authHeader'>): string | undefined {
  return s.apiKey ? basicAuth(s.apiKey) : s.authHeader;
}

function existingEndpointHost(): string | undefined {
  const ep = process.env.OTEL_EXPORTER_OTLP_TRACES_ENDPOINT || process.env.OTEL_EXPORTER_OTLP_ENDPOINT;
  return ep ? urlHost(ep) || undefined : undefined;
}

/** Scenario A resource precedence, lowest first: OTel defaults (including telemetry.sdk.language=nodejs), OTEL_RESOURCE_ATTRIBUTES / OTEL_SERVICE_NAME, SDK attributes. */
export function buildResource(s: Settings): Resource {
  const attrs: Record<string, string> = {
    'service.name': s.serviceName || 'unknown_service',
    'deployment.environment.name': s.environment,
    'telemetry.distro.name': 'flashcat-genai-otel',
    'telemetry.distro.version': VERSION,
    ...s.resourceAttributes,
  };
  const fromEnv = detectResources({ detectors: [resourceAttributesEnvDetector, serviceNameEnvDetector] });
  return defaultResource().merge(fromEnv).merge(resourceFromAttributes(attrs));
}

/** Get the underlying global TracerProvider via ProxyTracerProvider.getDelegate(). */
function globalDelegate(): any {
  const p: any = trace.getTracerProvider();
  return typeof p?.getDelegate === 'function' ? p.getDelegate() : p;
}

function isNoop(p: any): boolean {
  return !p || p.constructor?.name === 'NoopTracerProvider';
}

/** Build the Flashcat processor. If the endpoint or credentials are missing, only process sessions and record the reason in notes. */
export function buildProcessor(
  s: Settings,
  masking: CompiledMasking,
  notes: string[],
): { processor: FlashcatSpanProcessor; exporting: boolean } {
  const st = state();
  const session = new SessionProcessor();
  let chain: MaskingProcessor | undefined;
  if (!s.endpoint) {
    notes.push('缺少 endpoint（FLASHCAT_ENDPOINT，完整 .../v1/traces URL），未启用导出');
  } else if (!authHeader(s)) {
    notes.push('缺少 api_key（FLASHCAT_API_KEY=keyID:secret），未启用导出');
  } else {
    const exporter = spanExporter(s.endpoint, authHeader(s), s.exportTimeoutMs, st.stats);
    chain = new MaskingProcessor(new BatchSpanProcessor({ exporter }), masking, session.ancestry);
  }
  const processor = new FlashcatSpanProcessor(session, chain);
  st.processors.push(processor);
  return { processor, exporting: !!chain };
}

export function setupTracing(settings: Settings, masking: CompiledMasking): ProviderResult {
  let s = settings;
  const notes: string[] = [];
  const problems: string[] = [];
  const st = state();
  const current = globalDelegate();
  let situation: ProviderResult['situation'];
  if (isNoop(current)) {
    situation = 'A';
  } else {
    const host = existingEndpointHost();
    situation = host && s.knownHosts.includes(host) ? 'C' : 'B';
  }

  const serviceInvalid = !s.serviceName || s.serviceName.startsWith('unknown_service');
  if (situation === 'A' && serviceInvalid && (s.exporter === 'auto' || s.exporter === 'add')) {
    // As in Python, a missing application name disables export. init supplies the diagnostics message.
    s = { ...s, exporter: 'none' };
  }
  let mode: ProviderResult['exporterMode'];
  if (s.exporter === 'auto') {
    if (situation === 'A') mode = 'add';
    else if (situation === 'C') mode = 'reuse';
    else if (st.userProcessor) {
      // If the user's SDK already includes spanProcessor(), treat it as reuse and skip the warning.
      mode = 'reuse';
      notes.push('检测到 otel.spanProcessor() 已在使用，数据经它导出，按 reuse 处理');
    } else {
      mode = 'none';
      problems.push(SITUATION_B_PROBLEM);
    }
  } else if (s.exporter === 'add' && situation !== 'A') {
    // OTel JS cannot append processors to an existing provider (contract §2.4).
    mode = 'none';
    problems.push(SITUATION_B_PROBLEM);
  } else {
    mode = s.exporter as ProviderResult['exporterMode'];
  }
  if (situation === 'C' && s.exporter === 'add') {
    notes.push("进程已在往 Flashcat 发数据，显式 exporter='add' 可能产生重复上报");
  }
  if (mode === 'reuse') {
    notes.push('未新增 exporter，数据经进程里已有的 exporter 发出（GenAI 数据由平台侧识别）');
  }
  if (situation === 'C' && !st.userProcessor) {
    notes.push(
      '已有 provider 时 OTel JS 不能追加会话处理器：自动插桩产生的 span 不带会话属性（语义 API 的 span 照常带）；需要时把 otel.spanProcessor() 放进 SDK 配置',
    );
  }

  let serviceName = s.serviceName;
  if (situation === 'A') {
    if (mode === 'add' || mode === 'reuse' || mode === 'none') {
      // Scenario A always creates a provider for session processing. mode=none omits export.
      const resource = buildResource(s);
      const procNotes: string[] = [];
      let processor: FlashcatSpanProcessor;
      if (mode === 'add') {
        const built = buildProcessor(s, masking, procNotes);
        processor = built.processor;
        if (!built.exporting) mode = 'none';
      } else {
        processor = new FlashcatSpanProcessor(new SessionProcessor(), undefined);
      }
      notes.push(...procNotes);
      const provider = new TracerProvider({ resource, spanProcessors: [processor] });
      if (!trace.setGlobalTracerProvider(provider)) {
        // Another provider registered after the initial check.
        void provider.shutdown();
        situation = 'B';
        mode = 'none';
        problems.push(SITUATION_B_PROBLEM);
      } else {
        st.ownTracerProvider = provider;
        context.setGlobalContextManager(new AsyncLocalStorageContextManager().enable());
        // setGlobalPropagator returns false if the application has already registered a propagator and leaves it in place.
        propagation.setGlobalPropagator(
          new CompositePropagator({ propagators: [new W3CTraceContextPropagator(), new W3CBaggagePropagator()] }),
        );
      }
    }
  } else {
    try {
      // Read the existing provider's resource from its private field. Fall back to the supplied configuration for diagnostics only.
      const actual = current?._resource?.attributes?.['service.name'];
      if (typeof actual === 'string' && s.serviceName && actual !== s.serviceName) {
        notes.push(`已有 provider 的 service.name 是 '${actual}'，与参数 '${s.serviceName}' 不同；OTel 不支持修改 resource，以实际值为准`);
        serviceName = actual;
      } else if (typeof actual === 'string') {
        serviceName = actual;
      }
    } catch {
      // Ignore resource lookup failures.
    }
  }
  return { situation, exporterMode: mode, metricsMode: 'none', serviceName, notes, problems };
}

/** Configure MeterProvider as well so gen_ai.client.* metrics are not silently discarded. */
export function setupMetrics(s: Settings, res: ProviderResult): void {
  if (!s.metrics) {
    res.metricsMode = 'disabled';
    return;
  }
  const mp: any = metrics.getMeterProvider();
  // Without a registered provider, the API returns NoopMeterProvider. API 1.10 has no proxy, so check the class name.
  if (mp && mp.constructor?.name !== 'NoopMeterProvider') {
    res.metricsMode = 'reuse';
    return;
  }
  if (res.exporterMode !== 'add' || !s.metricsEndpoint) {
    res.metricsMode = 'none';
    if (res.exporterMode === 'add') res.notes.push('没有 metrics endpoint，gen_ai.client.* 指标不导出');
    return;
  }
  const { MeterProvider, PeriodicExportingMetricReader } = localRequire('@opentelemetry/sdk-metrics');
  const reader = new PeriodicExportingMetricReader({
    exporter: metricExporter(s.metricsEndpoint, authHeader(s), s.exportTimeoutMs),
    exportIntervalMillis: 60_000,
  });
  const provider = new MeterProvider({ resource: buildResource(s), readers: [reader] });
  if (metrics.setGlobalMeterProvider(provider)) {
    state().ownMeterProvider = provider;
    res.metricsMode = 'add';
  } else {
    void provider.shutdown();
    res.metricsMode = 'reuse';
  }
}
