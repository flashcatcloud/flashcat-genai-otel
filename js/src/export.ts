/**
 * Export (contract §3) uses OTLP/HTTP protobuf, gzip, and Basic authentication. CountingExporter records results without retrying.
 *
 * The OTLPTraceExporter in `@opentelemetry/exporter-trace-otlp-proto` (verified with 0.301.1)
 * retries 429, 502, 503, and 504 until timeout. Its RetryingTransport then returns a generic
 * "Export failed with retryable status" error, leaving the counter unable to distinguish 429 from 5xx.
 * Reuse the public components (OTLPExporterBase, createOtlpNetworkExportDelegate, and
 * ProtobufTraceSerializer) with a custom transport that returns an OTLPExporterError for every non-2xx response.
 * The error carries the status in `code` for the export counters (contract §3).
 * The transport retries at most twice during bounded flush or shutdown. The counter sees only the final result.
 *
 * Metrics use the same transport. The standard OTLPMetricExporter treats ECONNREFUSED as retryable,
 * backing off for 1 / 1.5 / 2.25 / 3.4 seconds until the 10-second export timeout, with referenced timers.
 * MeterProvider.forceFlush and shutdown each export once, which delayed exit by roughly 17 seconds when the endpoint refused connections (Nov 17, 2026).
 * Metrics default to cumulative temporality, so after a failed export, the next export still contains the total.
 * Both export pipelines share bounded retries (200 / 400 ms backoff) and the flush or shutdown deadline (contract §3 and §9, see deadline.ts).
 */

import * as http from 'node:http';
import * as https from 'node:https';
import * as zlib from 'node:zlib';
import { performance } from 'node:perf_hooks';
import { ExportResultCode, type ExportResult } from '@opentelemetry/core';
import {
  OTLPExporterBase,
  OTLPExporterError,
  createOtlpNetworkExportDelegate,
  getSharedConfigurationDefaults,
  mergeOtlpSharedConfigurationWithDefaults,
  type ExportResponse,
  type IExporterTransport,
} from '@opentelemetry/otlp-exporter-base';
import { createOtlpHttpExporterMetrics } from '@opentelemetry/otlp-exporter-base/node-http';
import {
  MetricsExporterMetricsHelper,
  ProtobufMetricsSerializer,
  ProtobufTraceSerializer,
  TraceExporterMetricsHelper,
} from '@opentelemetry/otlp-transformer';
import type { ReadableSpan, SpanExporter } from '@opentelemetry/sdk-trace';
import { DeadlineError, isDeadlineError, requestBudget, trackInflight, waitForRetry } from './deadline';
import type { ExportStats } from './state';
import { localRequire } from './req';
import { VERSION } from './version';

/** Build the Basic header from keyID:secret. The SDK handles Base64 encoding and expects raw credentials. */
export function basicAuth(apiKey: string): string {
  return 'Basic ' + Buffer.from(apiKey, 'utf8').toString('base64');
}

type AttemptResponse = ExportResponse & { retryable?: boolean; retryAfterMs?: number };

/** Retry-After accepts seconds or an HTTP date. Ignore invalid values and honor the server's minimum wait. */
function retryAfterMillis(value: string | undefined): number {
  value = value?.trim();
  if (!value) return 0;
  // Keep very large valid delays as Infinity so the budget check skips the retry instead of falling back to 200 ms.
  if (/^\d+$/.test(value)) return Number(value) * 1000;
  const date = Date.parse(value);
  return Number.isNaN(date) ? 0 : Math.max(0, date - Date.now());
}

/** POST with gzip and retry network errors or 429 / 502 / 503 / 504 at most twice during bounded flush or shutdown. */
export class GzipHttpTransport implements IExporterTransport {
  private readonly url: URL;
  private readonly agent: http.Agent;

  constructor(
    url: string,
    private readonly headers: Record<string, string>,
  ) {
    this.url = new URL(url);
    this.agent = this.url.protocol === 'https:' ? new https.Agent({ keepAlive: true }) : new http.Agent({ keepAlive: true });
  }

  async send(data: Uint8Array, timeoutMillis: number): Promise<ExportResponse> {
    for (let retries = 0; ; retries++) {
      const started = performance.now();
      const result = await this.sendOnce(data, timeoutMillis);
      if (!result.retryable || retries >= 2) return result;
      const delay = Math.max(200 * 2 ** retries, result.retryAfterMs ?? 0);
      if (!(await waitForRetry(delay, performance.now() - started))) {
        return requestBudget(timeoutMillis) <= 0 ? { status: 'failure', error: new DeadlineError() } : result;
      }
    }
  }

  private sendOnce(data: Uint8Array, timeoutMillis: number): Promise<AttemptResponse> {
    return new Promise((resolve) => {
      let settled = false;
      let cleanup = () => {};
      const done = (r: AttemptResponse) => {
        if (!settled) {
          settled = true;
          cleanup();
          resolve(r);
        }
      };
      const networkError = (error: Error) => done({ status: 'failure', error, retryable: !isDeadlineError(error) });
      zlib.gzip(data, (zerr, body) => {
        if (zerr) return done({ status: 'failure', error: zerr });
        // During flush or shutdown, cap the export timeout at the remaining budget. Skip requests once the deadline passes (contract §3).
        const budget = requestBudget(timeoutMillis);
        if (budget <= 0) return done({ status: 'failure', error: new DeadlineError() });
        const mod = this.url.protocol === 'https:' ? https : http;
        const req = mod.request(
          this.url,
          {
            method: 'POST',
            agent: this.agent,
            headers: {
              ...this.headers,
              'Content-Type': 'application/x-protobuf',
              'Content-Encoding': 'gzip',
              'Content-Length': String(body.length),
              'User-Agent': `flashcat-genai-otel-js/${VERSION}`,
            },
          },
          (res) => {
            const chunks: Buffer[] = [];
            let size = 0;
            res.on('data', (c: Buffer) => {
              size += c.length;
              if (size <= 64 * 1024) chunks.push(c);
            });
            res.on('end', () => {
              const code = res.statusCode ?? 0;
              const text = Buffer.concat(chunks);
              if (code >= 200 && code < 300) done({ status: 'success', data: text });
              else
                done({
                  status: 'failure',
                  error: new OTLPExporterError(res.statusMessage ?? `HTTP ${code}`, code, text.toString('utf8')),
                  retryable: [429, 502, 503, 504].includes(code),
                  retryAfterMs: retryAfterMillis(res.headers['retry-after']),
                });
            });
            res.on('error', networkError);
          },
        );
        // Use a total request timeout so blackholed connections end on time. A timeout shortened by the deadline counts as a deadline expiry.
        const clipped = budget < timeoutMillis;
        const timer = setTimeout(
          () => req.destroy(clipped ? new DeadlineError('flush / shutdown 时限已到，导出请求已中止') : new Error('Request timed out')),
          budget,
        );
        timer.unref?.();
        // abortInflight() destroys the request at the deadline. Stop tracking the request when it finishes.
        const untrack = trackInflight(() => req.destroy(new DeadlineError('flush / shutdown 时限已到，导出请求已中止')));
        cleanup = () => {
          clearTimeout(timer);
          untrack();
        };
        req.on('error', networkError);
        req.end(body);
      });
    });
  }

  shutdown(): void {
    this.agent.destroy();
  }
}

function errorText(e: unknown): string {
  if (e instanceof Error) return `${e.name || e.constructor?.name || 'Error'}: ${e.message}`;
  return String(e);
}

/** Count export results by outcome (contract §3): ok / http_400/401/403/413/429 / http_5xx / http_<code> / network / dropped_timeout. */
export class CountingExporter implements SpanExporter {
  constructor(
    private readonly inner: SpanExporter,
    private readonly stats: ExportStats,
  ) {}

  export(spans: ReadableSpan[], resultCallback: (result: ExportResult) => void): void {
    try {
      this.inner.export(spans, (result) => {
        try {
          this.count(spans.length, result);
        } catch {
          // A counter failure should leave export unaffected.
        }
        resultCallback(result);
      });
    } catch (e) {
      this.stats.inc('network', 1, errorText(e));
      resultCallback({ code: ExportResultCode.FAILED, error: e as Error });
    }
  }

  private count(n: number, result: ExportResult): void {
    if (result.code === ExportResultCode.SUCCESS) {
      this.stats.inc('ok', n);
      return;
    }
    const err: any = result.error;
    if (isDeadlineError(err)) {
      // Count deadline drops by span as specified in contract §3.
      this.stats.inc('dropped_timeout', n, errorText(err));
      return;
    }
    const code = err && typeof err.code === 'number' ? (err.code as number) : undefined;
    if (code === undefined) {
      this.stats.inc('network', 1, err ? errorText(err) : 'export failed');
      return;
    }
    const bucket = [400, 401, 403, 413, 429].includes(code) ? `http_${code}` : code >= 500 ? 'http_5xx' : `http_${code}`;
    const body = typeof err.data === 'string' ? err.data.slice(0, 300) : '';
    this.stats.inc(bucket, 1, `HTTP ${code} ${body}`.trim());
  }

  shutdown(): Promise<void> {
    return this.inner.shutdown();
  }

  forceFlush(): Promise<void> {
    return this.inner.forceFlush ? this.inner.forceFlush() : Promise.resolve();
  }
}

export function spanExporter(endpoint: string, authHeader: string | undefined, timeoutMs: number, stats: ExportStats): CountingExporter {
  const headers: Record<string, string> = authHeader ? { Authorization: authHeader } : {};
  const config = mergeOtlpSharedConfigurationWithDefaults({ timeoutMillis: timeoutMs }, {}, getSharedConfigurationDefaults());
  const delegate = createOtlpNetworkExportDelegate(
    config,
    ProtobufTraceSerializer,
    createOtlpHttpExporterMetrics('otlp_http_span_exporter', TraceExporterMetricsHelper, endpoint, undefined),
    new GzipHttpTransport(endpoint, headers),
  );
  const inner = new OTLPExporterBase<ReadableSpan[]>(delegate) as unknown as SpanExporter;
  return new CountingExporter(inner, stats);
}

/**
 * Metrics use OTLPMetricExporterBase to retain options such as OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE,
 * with protobuf and the same gzip transport, bounded retries, and deadline as traces. The caller's reader sets the 60-second interval.
 */
export function metricExporter(endpoint: string, authHeader: string | undefined, timeoutMs: number): any {
  // Load this package only when metrics are enabled and a metrics endpoint is configured.
  const { OTLPMetricExporterBase } = localRequire('@opentelemetry/exporter-metrics-otlp-http');
  const headers: Record<string, string> = authHeader ? { Authorization: authHeader } : {};
  const config = mergeOtlpSharedConfigurationWithDefaults({ timeoutMillis: timeoutMs }, {}, getSharedConfigurationDefaults());
  const delegate = createOtlpNetworkExportDelegate(
    config,
    ProtobufMetricsSerializer,
    createOtlpHttpExporterMetrics('otlp_http_metric_exporter', MetricsExporterMetricsHelper, endpoint, undefined),
    new GzipHttpTransport(endpoint, headers),
  );
  return new OTLPMetricExporterBase(delegate);
}
