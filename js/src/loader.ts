/**
 * Load instrumentation and detect conflicts. Record failures in diagnostics and let the application continue (contract §2 and §8).
 */

import { metrics } from '@opentelemetry/api';
import { existsSync, readFileSync, realpathSync } from 'node:fs';
import { createRequire } from 'node:module';
import * as path from 'node:path';
import { ALIASES, CATALOG, type Choice, type Entry } from './catalog';
import { log } from './log';
import { localRequire } from './req';
import { state } from './state';

export interface InstrumentationItem {
  lib: string;
  source: string;
  status: string;
  lib_version?: string;
  hint?: string;
  supported?: string;
  error?: string;
  foreign?: string[];
}

const LOADED_BEFORE_HINT =
  '目标库在 init 之前已被加载，补丁可能打不上；请用 node --import @flashcat/genai-otel/register 启动，或把 init 放在导入模型 SDK 之前';

/** Resolve target libraries from the application entry point's directory, then the current working directory. */
export function appBases(): string[] {
  const out: string[] = [];
  try {
    const main = process.argv[1];
    if (main && existsSync(main)) out.push(path.dirname(realpathSync(main)));
  } catch {
    // Ignore resolution errors.
  }
  out.push(process.cwd());
  return [...new Set(out)];
}

function requireFrom(base: string): NodeJS.Require {
  return createRequire(path.join(base, '__flashcat_genai_otel__.js'));
}

/**
 * Find the package directory and read its version. require.resolve('<pkg>/package.json') can throw
 * ERR_PACKAGE_PATH_NOT_EXPORTED because packages such as openai omit package.json from exports.
 */
export function findPackage(name: string, bases: string[]): { dir: string; version: string } | undefined {
  for (const base of bases) {
    let dirs: string[] | null = null;
    try {
      dirs = requireFrom(base).resolve.paths(name);
    } catch {
      dirs = null;
    }
    for (const d of dirs ?? []) {
      const pj = path.join(d, name, 'package.json');
      if (!existsSync(pj)) continue;
      try {
        const version = JSON.parse(readFileSync(pj, 'utf8')).version;
        if (typeof version === 'string') return { dir: path.dirname(pj), version };
      } catch {
        // Keep searching.
      }
    }
  }
  return undefined;
}

function resolveFrom(specifier: string, bases: string[]): string | undefined {
  for (const base of bases) {
    try {
      return requireFrom(base).resolve(specifier);
    } catch {
      // Keep searching.
    }
  }
  return undefined;
}

/** Return exports for an already-loaded CommonJS module. Reading require.cache does not load it. */
function loadedExports(specifier: string, bases: string[]): any | undefined {
  const resolved = resolveFrom(specifier, bases);
  if (!resolved) return undefined;
  const m = localRequire.cache[resolved];
  return m && m.loaded ? m.exports : undefined;
}

/** Load from the application directory first for optional peers, then from this package. */
export function loadOptional(pkg: string, bases: string[], bundled: boolean): any {
  if (bundled) return localRequire(pkg);
  let lastErr: unknown;
  for (const base of bases) {
    try {
      return requireFrom(base)(pkg);
    } catch (e) {
      lastErr = e;
      if ((e as any)?.code !== 'MODULE_NOT_FOUND' && (e as any)?.code !== 'ERR_MODULE_NOT_FOUND') throw e;
    }
  }
  try {
    return localRequire(pkg);
  } catch (e) {
    throw lastErr ?? e;
  }
}

function isNotFound(e: unknown, pkg: string): boolean {
  const code = (e as any)?.code;
  return (code === 'MODULE_NOT_FOUND' || code === 'ERR_MODULE_NOT_FOUND') && String((e as any)?.message ?? '').includes(pkg);
}

function walk(obj: any, p: string[]): any {
  let cur = obj;
  for (const k of p) {
    if (cur == null) return undefined;
    cur = cur[k];
  }
  return cur;
}

/** Check loaded modules for third-party wrappers via shimmer's __wrapped flag or OpenInference's openInferencePatched. */
function detectConflicts(entry: Entry, bases: string[]): { foreign: string[]; loaded: boolean } {
  const foreign: string[] = [];
  let loaded = false;
  for (const probe of entry.probes) {
    let exp: any;
    try {
      exp = loadedExports(probe.specifier, bases);
    } catch {
      exp = undefined;
    }
    if (exp === undefined) continue;
    loaded = true;
    try {
      if (exp.openInferencePatched) foreign.push('openinference');
      const fn = walk(exp, probe.path);
      if (typeof fn === 'function' && (fn as any).__wrapped) foreign.push(`${probe.path.join('.')}.__wrapped`);
    } catch {
      // Ignore inspection errors.
    }
  }
  if (!loaded && entry.probes.length === 0) {
    try {
      loaded = loadedExports(entry.target, bases) !== undefined;
    } catch {
      loaded = false;
    }
  }
  return { foreign: [...new Set(foreign)], loaded };
}

function disabledMatch(entry: Entry, choice: Choice | undefined, off: Set<string>): boolean {
  return off.has(entry.lib) || (choice ? off.has(`${choice.source}:${entry.lib}`) : false) || off.has(entry.lib.replace(/-/g, '_'));
}

export interface LoadOptions {
  only: string[] | undefined;
  disabled: string[];
  prefer: 'foreign' | 'flashcat';
  contentEnabled: boolean;
}

export function load(opts: LoadOptions): InstrumentationItem[] {
  const want = opts.only ? new Set(opts.only.map((x) => ALIASES[x] ?? x)) : undefined;
  const off = new Set(opts.disabled.map((x) => ALIASES[x] ?? x));
  const bases = appBases();
  const results: InstrumentationItem[] = [];
  for (const entry of CATALOG) {
    if (want && !want.has(entry.lib)) continue;
    const item: InstrumentationItem = { lib: entry.lib, source: 'upstream', status: 'not_present' };
    results.push(item);
    try {
      const pkg = findPackage(entry.target, bases);
      if (!pkg) {
        // If the library is absent, report the preferred source from the catalog.
        item.source = entry.choose('0.0.0').kind === 'skip' ? entry.choose('1.0.0').source : entry.choose('0.0.0').source;
        continue;
      }
      item.lib_version = pkg.version;
      const choice = entry.choose(pkg.version);
      item.source = choice.source;
      if (disabledMatch(entry, choice, off)) {
        item.status = 'disabled';
        continue;
      }
      if (choice.kind === 'skip') {
        item.status = 'skipped_version';
        if (choice.supported) item.supported = choice.supported;
        continue;
      }
      const { foreign, loaded } = detectConflicts(entry, bases);
      if (foreign.length && opts.prefer !== 'flashcat') {
        item.status = 'skipped_conflict';
        item.foreign = foreign;
        log.warn(`${entry.lib} 已被其他插桩包装（${foreign.join(', ')}），跳过 Flashcat 这一层以免重复`);
        continue;
      }
      if (choice.kind === 'native') {
        // For native instrumentation, install nothing. init already applied the content capture switch to the environment.
        item.status = 'installed';
        continue;
      }
      if (choice.kind === 'vercel') {
        loadVercel(item, bases, opts.contentEnabled);
        continue;
      }
      let inst: any;
      if (choice.create) {
        // Constructing built-in instrumentation enables it with a tracer from the global provider.
        inst = choice.create(opts.contentEnabled, bases);
      } else {
        let mod: any;
        try {
          mod = loadOptional(choice.pkg!, bases, !!choice.bundled);
        } catch (e) {
          if (isNotFound(e, choice.pkg!)) {
            item.status = 'instrumentation_not_installed';
            item.hint = `npm install ${choice.pkg}`;
          } else {
            item.status = 'failed';
            item.error = `${(e as Error)?.name}: ${(e as Error)?.message}`;
          }
          continue;
        }
        const Cls = mod[choice.className!];
        if (typeof Cls !== 'function') {
          item.status = 'failed';
          item.error = `${choice.pkg} 没有导出 ${choice.className}`;
          continue;
        }
        // InstrumentationBase defaults to enabled=true, so construction enables it with a tracer from the global provider.
        inst = new Cls(choice.config ? choice.config(opts.contentEnabled) : {});
      }
      // Call setMeterProvider again. In instrumentation-openai 0.23.0, subclass fields such as
      // _genaiClientTokenUsage overwrite instruments created in super() with undefined.
      // Streaming responses with usage can then throw TypeError into application code. NodeSDK and registerInstrumentations
      // normally call setMeterProvider, so call it explicitly when constructing the instrumentation directly.
      if (typeof inst.setMeterProvider === 'function') inst.setMeterProvider(metrics.getMeterProvider());
      if (typeof inst.isEnabled === 'function' && !inst.isEnabled()) inst.enable();
      if (choice.augment) {
        try {
          choice.augment(inst);
        } catch {
          // Continue using the base instrumentation if augmentation fails.
        }
      }
      state().instances[entry.lib] = { source: choice.source, inst };
      if (loaded) {
        item.status = 'loaded_before_init';
        item.hint = LOADED_BEFORE_HINT;
      } else {
        item.status = 'installed';
      }
    } catch (e) {
      item.status = 'failed';
      item.error = `${(e as Error)?.name}: ${(e as Error)?.message}`;
    }
  }
  return results;
}

/** The third case in contract §8 (outside coverage) applies to model SDKs. Unsupported LangChain or Agents versions can still produce spans through the underlying model SDK. */
const MODEL_SDKS = new Set(['openai', 'anthropic', 'google-genai']);

/**
 * When a detected library will produce no spans, report why in problems even if the app and its diagnostics otherwise look healthy (contract §8).
 * Log one WARN per cause. Return the messages to merge into problems.
 */
export function silentGapProblems(items: InstrumentationItem[]): string[] {
  const out: string[] = [];
  for (const item of items) {
    const target = CATALOG.find((e) => e.lib === item.lib)?.target ?? item.lib;
    const lib = `${target} ${item.lib_version ?? ''}`.trim();
    let p: string | undefined;
    if (item.status === 'instrumentation_not_installed') {
      p = `检测到 ${lib}，但没装对应的插桩包，这部分调用不会产生 span；请执行 ${item.hint ?? '安装对应的插桩包'}`;
    } else if (item.status === 'skipped_version' && MODEL_SDKS.has(item.lib)) {
      p = `检测到 ${lib}，不在插桩支持的版本范围（${item.supported ?? '见 README'}）内，模型调用不会产生 span`;
    }
    if (!p) continue;
    out.push(p);
    log.warnOnce(`gap:${item.lib}:${item.status}`, p);
  }
  return out;
}

/**
 * Vercel AI SDK 7 uses `registerTelemetry(new OpenTelemetry())`.
 * OpenTelemetry integration has no constructor options for input or output recording. When content capture is off,
 * wrap each event and set recordInputs and recordOutputs to false so @ai-sdk/otel skips those attributes.
 */
function loadVercel(item: InstrumentationItem, bases: string[], contentEnabled: boolean): void {
  let otelMod: any;
  try {
    otelMod = loadOptional('@ai-sdk/otel', bases, false);
  } catch (e) {
    if (isNotFound(e, '@ai-sdk/otel')) {
      item.status = 'instrumentation_not_installed';
      item.hint = 'npm install @ai-sdk/otel';
    } else {
      item.status = 'failed';
      item.error = `${(e as Error)?.name}: ${(e as Error)?.message}`;
    }
    return;
  }
  const aiMod = loadOptional('ai', bases, false);
  const existing: any[] = (globalThis as any).AI_SDK_TELEMETRY_INTEGRATIONS ?? [];
  if (existing.some((x) => x && (x instanceof otelMod.OpenTelemetry || x.__flashcat || x.constructor?.name === 'OpenTelemetry'))) {
    item.status = 'already_instrumented';
    return;
  }
  const integration = new otelMod.OpenTelemetry();
  aiMod.registerTelemetry(contentEnabled ? integration : hideContent(integration));
  item.status = 'installed';
}

export function hideContent(integration: any): any {
  return new Proxy(integration, {
    get(target, prop) {
      if (prop === '__flashcat') return true;
      const v = Reflect.get(target, prop, target);
      if (typeof v === 'function' && typeof prop === 'string' && /^(experimental_)?on[A-Z]/.test(prop)) {
        return function (this: unknown, event: any, ...rest: unknown[]) {
          const e = event && typeof event === 'object' ? { ...event, recordInputs: false, recordOutputs: false } : event;
          return v.call(target, e, ...rest);
        };
      }
      return v;
    },
  });
}
