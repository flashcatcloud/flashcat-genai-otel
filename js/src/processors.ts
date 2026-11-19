/**
 * SessionProcessor (onStart) and MaskingProcessor (onEnd), see contract §5 and §6.
 */

import { context as otelContext, createContextKey, trace, type Attributes, type Context, type SpanContext } from '@opentelemetry/api';
import type { ReadableSpan, Span, SpanProcessor } from '@opentelemetry/sdk-trace';
import { ConfigError } from './config';
import { log } from './log';
import { state } from './state';

// ---- Session Context keys (private to Flashcat, with baggage off by default) ----
// createContextKey uses Symbol.for, so the ESM and CJS builds share these keys.
export const CONV_KEY = createContextKey('flashcat.conversation_id');
export const USER_KEY = createContextKey('flashcat.user_id');
export const TAGS_KEY = createContextKey('flashcat.tags');

export const TRUNCATED_ATTR = 'flashcat.content_truncated';
const ROOT_ATTR = 'flashcat.genai.root';
const TAG_PREFIX = 'flashcat.tag.';
const INFERENCE_OPS = new Set(['chat', 'text_completion', 'generate_content', 'embeddings']);

/** Track active spans created by the semantic API for annotate and nested inference detection. Upstream spans are excluded. */
export const FRAME_KEY = createContextKey('flashcat-otel-semantic-frame');
export interface SemanticFrame {
  span: unknown;
  kind: string;
  outputSet: boolean;
}
export const NESTED_INFERENCE_PROBLEM =
  '手工 llm / embedding span 里又有被插桩的模型调用，同一次调用会重复计数：只用 llm / embedding 标没有被插桩的模型客户端，或去掉外层的手工 span';

// ---- AI root marker (contract §5.1) ----

// Use the same rule as the Flashcat backend to classify GenAI spans.
const GENAI_KEYS = new Set([
  'gen_ai.operation.name',
  'gen_ai.request.model',
  'gen_ai.provider.name',
  'gen_ai.system',
  'openinference.span.kind',
  'llm.model_name',
  'traceloop.span.kind',
  'langfuse.observation.type',
  'flashcat.span.kind',
  'gen_ai.tool.name',
  'gen_ai.tool.call.id',
  'ai.operationId',
  'llm.request.type',
]);
const GENAI_PREFIXES = ['gen_ai.', 'llm.', 'openinference.', 'traceloop.', 'langfuse.observation.'];
const GENAI_SCOPE_PREFIX = '@arizeai/openinference-instrumentation-';
const MAX_ANCESTRY = 65_536;

function isGenAI(attrs: Attributes | undefined): boolean {
  if (!attrs) return false;
  const vercel = Object.hasOwn(attrs, 'operation.name');
  for (const key of Object.keys(attrs)) {
    // The session processor also sets this attribute on ordinary spans.
    if (key === 'gen_ai.conversation.id') continue;
    if (GENAI_KEYS.has(key) || GENAI_PREFIXES.some((prefix) => key.startsWith(prefix)) || (vercel && key.startsWith('ai.'))) {
      return true;
    }
  }
  return false;
}

function hasGenAIScope(span: Partial<ReadableSpan> | undefined): boolean {
  return span?.instrumentationScope?.name.startsWith(GENAI_SCOPE_PREFIX) ?? false;
}

class GenAIAncestry {
  // Track only spans with a GenAI ancestor and evict them in Set insertion order.
  private ids?: Set<string>;
  private oldest?: SetIterator<string>;
  // LangChain reconstructs local parents from their original SpanContext. Use that context to look up the scope when the original span is unavailable.
  private readonly scopedContexts = new WeakSet<SpanContext>();

  has(spanId: string): boolean {
    return this.ids?.has(spanId) ?? false;
  }

  start(span: Span, parent: Partial<ReadableSpan> | undefined): void {
    const parentCtx = span.parentSpanContext;
    const hasAncestor = !!parentCtx && !parentCtx.isRemote && (
      this.has(parentCtx.spanId) || this.scopedContexts.has(parentCtx) ||
      hasGenAIScope(parent) || isGenAI(parent?.attributes)
    );
    const scoped = hasGenAIScope(span);
    if (scoped) this.scopedContexts.add(span.spanContext());
    if (hasAncestor) {
      const ids = (this.ids ??= new Set());
      ids.add(span.spanContext().spanId);
      if (ids.size > MAX_ANCESTRY) {
        // Reuse the iterator to avoid rescanning deleted positions from the start.
        const oldest = (this.oldest ??= ids.values());
        ids.delete(oldest.next().value!);
      }
    }
    if (scoped || isGenAI(span.attributes)) span.setAttribute(ROOT_ATTR, !hasAncestor);
  }
}

/**
 * At span start, set the conversation ID, user ID, and tags on each new span in the session scope (contract §5).
 *
 * Unlike Python 0.1.0, this does not require gen_ai.operation.name at span start. Built-in Anthropic
 * and Vercel AI instrumentation may set it later. Child spans also inherit session attributes from local parents.
 * Track in-process GenAI ancestry and set the AI root marker on spans that qualify at start (contract §5.1).
 */
export class SessionProcessor implements SpanProcessor {
  readonly ancestry = new GenAIAncestry();

  onStart(span: Span, parentContext: Context): void {
    try {
      const attrs = span.attributes;
      const ctx = parentContext ?? otelContext.active();
      let conv = ctx.getValue(CONV_KEY) as string | undefined;
      let user = ctx.getValue(USER_KEY) as string | undefined;
      const tags: Record<string, string> = { ...((ctx.getValue(TAGS_KEY) as Record<string, string>) ?? {}) };

      // Inherit session attributes when the parent is a local SDK span with attributes.
      const parent = trace.getSpan(ctx);
      const readableParent = parent as Partial<ReadableSpan> | undefined;
      if (parent && !parent.spanContext().isRemote) {
        const pa = readableParent?.attributes;
        if (pa && typeof pa === 'object') {
          if (!conv && typeof pa['gen_ai.conversation.id'] === 'string') conv = pa['gen_ai.conversation.id'] as string;
          if (!user && typeof pa['user.id'] === 'string') user = pa['user.id'] as string;
          for (const [k, v] of Object.entries(pa)) {
            if (k.startsWith(TAG_PREFIX) && typeof v === 'string') {
              const name = k.slice(TAG_PREFIX.length);
              if (!(name in tags)) tags[name] = v;
            }
          }
        }
      }

      if (conv && !('gen_ai.conversation.id' in attrs)) span.setAttribute('gen_ai.conversation.id', String(conv));
      if (user) span.setAttribute('user.id', String(user));
      for (const [k, v] of Object.entries(tags)) span.setAttribute(TAG_PREFIX + k, String(v));

      this.ancestry.start(span, readableParent);

      const op = attrs['gen_ai.operation.name'];
      if (!span.parentSpanContext && typeof op === 'string' && INFERENCE_OPS.has(op)) {
        state().orphans.inc();
      }
      // A model call is counted twice when its parent is a semantic API llm or embedding span (contract §4.1).
      const oi = attrs['openinference.span.kind'];
      if ((typeof op === 'string' && INFERENCE_OPS.has(op)) || oi === 'LLM' || oi === 'EMBEDDING') {
        const frame = ctx.getValue(FRAME_KEY) as SemanticFrame | undefined;
        if (frame && (frame.kind === 'llm' || frame.kind === 'embedding') && parent && parent === frame.span) {
          const st = state();
          st.nested.inc();
          if (!st.pendingProblems.includes(NESTED_INFERENCE_PROBLEM)) st.pendingProblems.push(NESTED_INFERENCE_PROBLEM);
          log.warnOnce('nested_inference', `flashcat-otel: ${NESTED_INFERENCE_PROBLEM}`);
        }
      }
    } catch {
      // Keep SDK failures from affecting the application.
    }
  }

  onEnd(_span: ReadableSpan): void {}

  forceFlush(): Promise<void> {
    return Promise.resolve();
  }

  shutdown(): Promise<void> {
    return Promise.resolve();
  }
}

// ---- Masking and truncation ----

// L1 key masking matches the final segment exactly, as in Python, to preserve counters such as gen_ai.usage.input_tokens.
export const SENSITIVE_KEY_SOURCE =
  '(^|[._-])(authorization|api[-_]?key|apikey|password|passwd|secret|client[-_]?secret|' +
  'token|access[-_]?token|refresh[-_]?token|auth[-_]?token|cookie|set[-_]?cookie|private[-_]?key)$';
const SENSITIVE_KEY = new RegExp(SENSITIVE_KEY_SOURCE, 'i');

export const CONTENT_KEYS = new Set([
  'gen_ai.input.messages',
  'gen_ai.output.messages',
  'gen_ai.system_instructions',
  'gen_ai.tool.call.arguments',
  'gen_ai.tool.call.result',
  'gen_ai.retrieval.query.text',
  'gen_ai.retrieval.documents',
]);

export interface MaskingRule {
  name: string;
  /** Compile strings as JS regular expressions with the g flag. A RegExp can also be supplied directly. */
  pattern: string | RegExp;
  replacement: string;
}

export interface MaskingConfig {
  /** L1 key masking, enabled by default. */
  keyMasking?: boolean;
  /** L2 uses DEFAULT_PATTERNS by default. Supplied rules replace the entire set. */
  contentPatterns?: MaskingRule[];
  /** Custom rules to run after the defaults. */
  extraPatterns?: MaskingRule[];
  /** Maximum L2 scan length per value. Defaults to 256 KiB. */
  scanLimitBytes?: number;
  /** L3 limit in UTF-8 bytes per string attribute. Defaults to 512 KiB. */
  truncateBytes?: number;
}

// The L2 default regex text and order are identical to DEFAULT_PATTERNS in Python's _processors.py.
// Use (?<!\d)/(?!\d) for digit boundaries. A JSON \uXXXX escape next to a digit can defeat \b.
export const DEFAULT_PATTERNS: ReadonlyArray<{ name: string; regex: string; replacement: string }> = [
  { name: 'email', regex: '[A-Za-z0-9._%+\\-]+@[A-Za-z0-9.\\-]+\\.[A-Za-z]{2,}', replacement: '[REDACTED:email]' },
  {
    name: 'cn_id_card',
    regex: '(?<!\\d)[1-9]\\d{5}(?:19|20)\\d{2}(?:0[1-9]|1[0-2])(?:0[1-9]|[12]\\d|3[01])\\d{3}[\\dXx](?![\\dXx])',
    replacement: '[REDACTED:id_card]',
  },
  { name: 'bank_card', regex: '(?<!\\d)(?:\\d{4}[ \\-]?){3}\\d{4,7}(?!\\d)', replacement: '[REDACTED:bank_card]' },
  { name: 'cn_mobile', regex: '(?<!\\d)1[3-9]\\d{9}(?!\\d)', replacement: '[REDACTED:phone]' },
];

/**
 * Compile the default Python patterns as JS regexes. In Python 3 string patterns, \d matches every Unicode decimal digit (Nd).
 * In JS, \d matches ASCII only. Use \p{Nd} with the u flag to match Python.
 */
export function compilePythonPattern(regex: string): RegExp {
  return new RegExp(regex.replace(/\\d/g, '\\p{Nd}'), 'gu');
}

function compileUserRule(rule: MaskingRule): RegExp {
  try {
    if (rule.pattern instanceof RegExp) {
      const flags = rule.pattern.flags.includes('g') ? rule.pattern.flags : rule.pattern.flags + 'g';
      return new RegExp(rule.pattern.source, flags);
    }
    return new RegExp(String(rule.pattern), 'g');
  } catch (e) {
    throw new ConfigError(`脱敏规则 ${rule.name} 的正则无法编译：${(e as Error).message}`);
  }
}

/** Time custom regexes once at init and reject obvious backtracking, as in Python's _probe_timeout. */
function probeTimeout(rx: RegExp, name: string, budgetMs = 50): void {
  const sample = 'a'.repeat(2000) + '@' + '1'.repeat(30) + '!';
  const t0 = performance.now();
  rx.lastIndex = 0;
  rx.test(sample);
  rx.lastIndex = 0;
  if (performance.now() - t0 > budgetMs) {
    throw new ConfigError(`脱敏正则 ${name}（${rx.source}）在探测样本上耗时过长，可能存在回溯，请改成线性写法`);
  }
}

/** Compiled L2 rule that replaces all matches in a string. */
export interface CompiledRule {
  name: string;
  apply: (s: string) => string;
}

function regexRule(name: string, rx: RegExp, repl: string): CompiledRule {
  return {
    name,
    apply: (s) => {
      rx.lastIndex = 0;
      return s.replace(rx, () => repl);
    },
  };
}

// ---- Linear implementation of the default email rule ----
// The pattern [A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,} is O(n²) on long strings without an @, such as Base64 tool results.
// Each starting position consumes the rest of the run and backtracks. Measured times were about 0.5 s at 32 KiB and 1.7 s at 64 KiB, also with Python re.
// At the 256 KiB scan limit, this can block the event loop for tens of seconds in onEnd.
// Scanning each @ preserves the regex's leftmost match and greedy backtracking in linear time. Randomized tests compare the output byte for byte.
const isLocal = (c: number) =>
  (c >= 65 && c <= 90) || (c >= 97 && c <= 122) || (c >= 48 && c <= 57) || c === 46 || c === 95 || c === 37 || c === 43 || c === 45;
const isDomain = (c: number) => (c >= 65 && c <= 90) || (c >= 97 && c <= 122) || (c >= 48 && c <= 57) || c === 46 || c === 45;
const isAlpha = (c: number) => (c >= 65 && c <= 90) || (c >= 97 && c <= 122);

export function replaceEmails(s: string, repl: string): string {
  let out = '';
  let pos = 0; // End of the previous replacement, where re.sub would resume searching.
  let at = s.indexOf('@', pos);
  while (at !== -1) {
    // Local part: the run of L characters immediately left of @, bounded by pos. L excludes @, so the runs do not overlap.
    let st = at;
    while (st > pos && isLocal(s.charCodeAt(st - 1))) st--;
    if (st < at) {
      // Domain: the run of D characters after @. Greedy D+ backtracking selects the rightmost dot immediately followed by at least two letters.
      let e = at + 1;
      while (e < s.length && isDomain(s.charCodeAt(e))) e++;
      let end = -1;
      for (let dot = e - 1; dot >= at + 2; dot--) {
        if (s.charCodeAt(dot) === 46 && dot + 2 < e && isAlpha(s.charCodeAt(dot + 1)) && isAlpha(s.charCodeAt(dot + 2))) {
          let le = dot + 1;
          while (le < s.length && isAlpha(s.charCodeAt(le))) le++;
          end = le;
          break;
        }
      }
      if (end !== -1) {
        out += s.slice(pos, st) + repl;
        pos = end;
        at = s.indexOf('@', pos);
        continue;
      }
    }
    at = s.indexOf('@', at + 1);
  }
  return out + s.slice(pos);
}

/** Compile masking options. Invalid explicit regexes throw ConfigError. */
export function compileMasking(cfg: MaskingConfig | undefined): CompiledMasking {
  const c = cfg ?? {};
  const rules: CompiledRule[] = [];
  if (c.contentPatterns) {
    for (const r of c.contentPatterns) {
      const rx = compileUserRule(r);
      probeTimeout(rx, r.name);
      rules.push(regexRule(r.name, rx, r.replacement));
    }
  } else {
    for (const p of DEFAULT_PATTERNS) {
      if (p.name === 'email') rules.push({ name: p.name, apply: (s) => replaceEmails(s, p.replacement) });
      else rules.push(regexRule(p.name, compilePythonPattern(p.regex), p.replacement));
    }
  }
  for (const r of c.extraPatterns ?? []) {
    const rx = compileUserRule(r);
    probeTimeout(rx, r.name);
    rules.push(regexRule(r.name, rx, r.replacement));
  }
  return {
    keyMasking: c.keyMasking ?? true,
    rules,
    scanLimit: c.scanLimitBytes ?? 256 * 1024,
    truncateBytes: c.truncateBytes ?? 512 * 1024,
  };
}

export interface CompiledMasking {
  keyMasking: boolean;
  rules: CompiledRule[];
  scanLimit: number;
  truncateBytes: number;
}

/** Truncate at a valid UTF-8 boundary within limit bytes. Drop the entire partial character, as with Python errors="ignore". */
export function truncateUtf8(s: string, limit: number): [string, boolean] {
  // Fast path: each UTF-16 code unit contributes at most 3 UTF-8 bytes.
  if (s.length * 3 <= limit) return [s, false];
  const b = Buffer.from(s, 'utf8');
  if (b.length <= limit) return [s, false];
  let end = limit;
  // Back up to the start of the character. Continuation bytes have the form 10xxxxxx.
  while (end > 0 && (b[end] & 0xc0) === 0x80) end--;
  return [b.subarray(0, end).toString('utf8'), true];
}

/** Apply L1, L2, and L3 to one attribute value. Return the new value and whether it was truncated. */
export function maskValue(key: string, value: string, m: CompiledMasking, contentEnabled: boolean): [string, boolean] {
  let nv = value;
  if (m.keyMasking && SENSITIVE_KEY.test(key)) {
    nv = '[REDACTED]';
  } else if (contentEnabled && CONTENT_KEYS.has(key)) {
    // As in Python, scan the first scanLimit characters (UTF-16 code units here) and append the rest unchanged.
    let head = nv.slice(0, m.scanLimit);
    const tail = nv.slice(m.scanLimit);
    for (const rule of m.rules) head = rule.apply(head);
    nv = head + tail;
  }
  return truncateUtf8(nv, m.truncateBytes);
}

/**
 * Wrap the downstream BatchSpanProcessor. In onEnd, pass it a copy of the ReadableSpan with rewritten attributes.
 * Protects only the Flashcat export pipeline. Content capture defaults to off, and server-side masking provides a fallback.
 */
export class MaskingProcessor implements SpanProcessor {
  constructor(
    private readonly inner: SpanProcessor,
    private readonly masking: CompiledMasking,
    private readonly ancestry: GenAIAncestry,
  ) {}

  onStart(span: Span, parentContext: Context): void {
    this.inner.onStart(span, parentContext);
  }

  onEnd(span: ReadableSpan): void {
    let out = span;
    try {
      out = this.mask(span);
    } catch {
      // If masking fails, pass the original span downstream.
    }
    try {
      // BatchSpanProcessor's own dropped_queue_full counter is private and resets, so count before handing off
      // the span using the same condition (_finishedSpans.length >= _maxQueueSize, verified in @opentelemetry/sdk-trace 3.1.1).
      const b: any = this.inner;
      if (Array.isArray(b._finishedSpans) && typeof b._maxQueueSize === 'number' && b._finishedSpans.length >= b._maxQueueSize) {
        state().stats.inc('dropped_queue_full');
      }
    } catch {
      // Ignore failures while updating the queue drop counter.
    }
    this.inner.onEnd(out);
  }

  private mask(span: ReadableSpan): ReadableSpan {
    const attrs = span.attributes;
    if (!attrs) return span;
    // For spans that gain GenAI attributes after start, set the marker from the final attributes on the export copy only.
    const markRoot = !Object.hasOwn(attrs, ROOT_ATTR) && isGenAI(attrs);
    const st = state();
    const changed: Record<string, string> = {};
    let truncated = false;
    let any = false;
    for (const [k, v] of Object.entries(attrs)) {
      if (typeof v !== 'string') continue;
      const [nv, cut] = maskValue(k, v, this.masking, st.contentEnabled);
      truncated = truncated || cut;
      if (nv !== v) {
        changed[k] = nv;
        any = true;
      }
    }
    if (!any && !truncated && !markRoot) return span;
    if (any) st.masked.inc();
    const newAttrs: Attributes = { ...attrs, ...changed };
    if (markRoot) newAttrs[ROOT_ATTR] = !this.ancestry.has(span.spanContext().spanId);
    if (truncated) {
      st.truncated.inc();
      newAttrs[TRUNCATED_ATTR] = true;
    }
    return copySpan(span, newAttrs);
  }

  forceFlush(): Promise<void> {
    return this.inner.forceFlush();
  }

  shutdown(): Promise<void> {
    return this.inner.shutdown();
  }
}

/** Shallow copy each field. Object spread would lose ReadableSpan's prototype method spanContext(). */
function copySpan(span: ReadableSpan, attributes: Attributes): ReadableSpan {
  const ctx = span.spanContext();
  return {
    name: span.name,
    kind: span.kind,
    spanContext: () => ctx,
    parentSpanContext: span.parentSpanContext,
    startTime: span.startTime,
    endTime: span.endTime,
    status: span.status,
    attributes,
    links: span.links,
    events: span.events,
    duration: span.duration,
    ended: span.ended,
    resource: span.resource,
    instrumentationScope: span.instrumentationScope,
    droppedAttributesCount: span.droppedAttributesCount,
    droppedEventsCount: span.droppedEventsCount,
    droppedLinksCount: span.droppedLinksCount,
  };
}

/** Public Flashcat SpanProcessor. Applies session processing and optional masking, then batches and exports with counters. */
export class FlashcatSpanProcessor implements SpanProcessor {
  constructor(
    private readonly session: SessionProcessor,
    private readonly exportChain: MaskingProcessor | undefined,
  ) {}

  onStart(span: Span, parentContext: Context): void {
    this.session.onStart(span, parentContext);
    if (this.exportChain) {
      try {
        this.exportChain.onStart(span, parentContext);
      } catch {
        // Ignore processor errors.
      }
    }
  }

  onEnd(span: ReadableSpan): void {
    state().endedSinceExitFlush++;
    if (this.exportChain) {
      try {
        this.exportChain.onEnd(span);
      } catch {
        // Ignore processor errors.
      }
    }
  }

  forceFlush(): Promise<void> {
    return this.exportChain ? this.exportChain.forceFlush() : Promise.resolve();
  }

  shutdown(): Promise<void> {
    return this.exportChain ? this.exportChain.shutdown() : Promise.resolve();
  }
}
