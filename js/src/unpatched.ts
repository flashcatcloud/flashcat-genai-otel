/**
 * Detect target libraries that are in use without a patch at runtime (not_patched in contract §8).
 *
 * When Next.js, webpack, or esbuild bundles a model SDK, runtime require and import hooks cannot intercept it.
 * Diagnostics say installed and the app runs normally, but no model spans appear. Subscribe to Node's diagnostics_channel:
 * global fetch uses undici:request:create and node:http uses http.client.request.start.
 * Identify model SDK requests by their User-Agent (OpenAI/JS x.y.z or Anthropic/JS x.y.z)
 * and a path covered by instrumentation. Require both of the following before reporting not_patched:
 *     1. The instrumentation's module hook has never fired, so it has patched no copy of this library.
 *     2. The active span was not created by this instrumentation, which rules out ESM manuallyInstrument patches that bypass the hook.
 * Requiring both checks avoids false reports for projects that correctly externalize the SDK.
 *
 * Read only the User-Agent and path per outbound request. Unsubscribe when all targets are resolved and ignore subscription failures.
 */

import { trace } from '@opentelemetry/api';
import * as dc from 'node:diagnostics_channel';
import { CATALOG } from './catalog';
import { log } from './log';
import { state } from './state';

interface Watch {
  lib: string;
  /** User-Agent prefix. */
  ua: string;
  /** Instrumented endpoint suffixes, allowing baseURL path prefixes. */
  paths: string[];
}

// Monitor bundleable model SDKs identifiable by request headers. Bundling does not affect native instrumentation.
const WATCHES: Watch[] = [
  { lib: 'openai', ua: 'OpenAI/JS ', paths: ['/chat/completions', '/completions', '/embeddings', '/responses'] },
  { lib: 'anthropic', ua: 'Anthropic/JS ', paths: ['/messages'] },
];

const WATCHED_STATUS = new Set(['installed', 'loaded_before_init']);
const CHANNELS = ['undici:request:create', 'http.client.request.start'];

/** When a module hook fires, InstrumentationBase stores the exports in the module definition's moduleExports. */
export function hookFired(inst: any): boolean {
  // getModuleDefinitions() would call init() again and return new definitions.
  const mods = inst?._modules;
  if (!Array.isArray(mods)) return true; // Assume the hook fired if definitions are unavailable to avoid false reports.
  return mods.some(
    (m: any) => m?.moduleExports !== undefined || (Array.isArray(m?.files) && m.files.some((f: any) => f?.moduleExports !== undefined)),
  );
}

/** Check whether the current span came from this instrumentation. OpenInference wraps the underlying SDK span in .span. */
function activeSpanFrom(scope: string | undefined): boolean {
  try {
    const active: any = trace.getActiveSpan();
    const real = active?.span ?? active;
    const name = real?.instrumentationScope?.name ?? real?.instrumentationLibrary?.name;
    return !!scope && name === scope;
  } catch {
    return false;
  }
}

function headerOf(headers: unknown, name: string): string | undefined {
  // undici uses ['k', 'v', 'k', 'v', ...]. Older versions pass a raw header string.
  if (Array.isArray(headers)) {
    for (let i = 0; i + 1 < headers.length; i += 2) {
      if (String(headers[i]).toLowerCase() === name) return String(headers[i + 1]);
    }
    return undefined;
  }
  if (typeof headers === 'string') {
    for (const line of headers.split('\r\n')) {
      const i = line.indexOf(':');
      if (i > 0 && line.slice(0, i).trim().toLowerCase() === name) return line.slice(i + 1).trim();
    }
  }
  return undefined;
}

/** Get the User-Agent and path from either channel. Return undefined if unavailable. */
function requestInfo(channel: string, msg: any): { ua: string; path: string } | undefined {
  const req = msg?.request;
  if (!req) return undefined;
  let ua: unknown;
  if (channel === 'http.client.request.start') ua = typeof req.getHeader === 'function' ? req.getHeader('user-agent') : undefined;
  else ua = headerOf(req.headers, 'user-agent');
  // node-fetch (the Node.js default in openai 4.x) exposes headers via Headers.raw(), with array values.
  if (Array.isArray(ua)) ua = ua.join(', ');
  if (typeof ua !== 'string' || typeof req.path !== 'string') return undefined;
  return { ua, path: req.path.split('?')[0] };
}

/** Chinese diagnostics message that explains common causes and how to externalize the package in Next.js, webpack, or esbuild. */
export function notPatchedProblem(lib: string, status: string, ua: string, path: string): string {
  const pkg = CATALOG.find((e) => e.lib === lib)?.target ?? lib;
  const list = `['@flashcat/otel', '${pkg}']`;
  const head = `${pkg} 正在发模型请求（${ua.trim()} → ${path}），但插桩补丁没有生效，这些调用不会产生 span（状态 not_patched）。`;
  const loadOrder =
    status === 'loaded_before_init'
      ? `${pkg} 在 init 之前已被加载：请用 node --import @flashcat/otel/register 启动，或把 init 放在导入模型 SDK 之前。另一个常见原因是`
      : '常见原因是';
  return (
    head +
    `${loadOrder}打包工具把 ${pkg} 打进了 bundle，运行时的 require / import 钩子拦不到。` +
    `Next.js 13～14：在 next.config.js 里加 experimental: { serverComponentsExternalPackages: ${list} }；` +
    `Next.js 15 起：顶层 serverExternalPackages: ${list}；` +
    `webpack：用 externals 把 ${pkg} 排除在 bundle 之外（例如 externals: { '${pkg}': 'commonjs ${pkg}' }）；` +
    `esbuild：--external:${pkg}（或 packages: 'external'）。改完重新构建`
  );
}

/**
 * After loading instrumentation, monitor OpenAI and Anthropic entries marked installed or loaded_before_init
 * from upstream or OpenInference. On detection, set not_patched, add a problems entry, and log one WARN.
 */
export function watchUnpatched(): void {
  const st = state();
  const items: any[] = st.diag?.instrumentations ?? [];
  const pending = new Map<string, { watch: Watch; inst: any; scope: string | undefined }>();
  for (const w of WATCHES) {
    const item = items.find((i) => i.lib === w.lib);
    const rec = st.instances[w.lib];
    if (!item || !rec || !WATCHED_STATUS.has(item.status)) continue;
    if (rec.source !== 'upstream' && rec.source !== 'openinference') continue;
    pending.set(w.lib, { watch: w, inst: rec.inst, scope: rec.inst?.instrumentationName });
  }
  if (pending.size === 0) return;

  const handlers: Array<[string, (msg: unknown) => void]> = [];
  const unsubscribe = () => {
    for (const [ch, fn] of handlers) {
      try {
        dc.unsubscribe(ch, fn);
      } catch {
        // Ignore subscription cleanup errors.
      }
    }
  };
  const onRequest = (channel: string, msg: unknown) => {
    try {
      const info = requestInfo(channel, msg);
      if (!info) return;
      for (const [lib, p] of pending) {
        if (!info.ua.startsWith(p.watch.ua) || !p.watch.paths.some((x) => info.path.endsWith(x))) continue;
        if (hookFired(p.inst)) {
          // If the hook fired, the patch is in place and monitoring can stop for this target.
          pending.delete(lib);
        } else if (!activeSpanFrom(p.scope)) {
          pending.delete(lib);
          markNotPatched(lib, info.ua, info.path);
        }
      }
      if (pending.size === 0) unsubscribe();
    } catch {
      // Keep monitoring failures from affecting the application.
    }
  };
  for (const ch of CHANNELS) {
    const fn = (msg: unknown) => onRequest(ch, msg);
    try {
      dc.subscribe(ch, fn);
      handlers.push([ch, fn]);
    } catch {
      // Older Node versions may lack this channel. Skip it silently.
    }
  }
}

function markNotPatched(lib: string, ua: string, path: string): void {
  const st = state();
  const diag: any = st.diag;
  const item = diag?.instrumentations?.find((i: any) => i.lib === lib);
  if (!item) return;
  const problem = notPatchedProblem(lib, item.status, ua, path);
  const pkg = CATALOG.find((e) => e.lib === lib)?.target ?? lib;
  item.status = 'not_patched';
  item.hint = `把 ${pkg} 排除在打包之外（Next.js：serverExternalPackages / experimental.serverComponentsExternalPackages），或确认在导入它之前完成 init`;
  if (!diag.problems.includes(problem)) diag.problems.push(problem);
  log.warnOnce(`not_patched:${lib}`, problem);
}
