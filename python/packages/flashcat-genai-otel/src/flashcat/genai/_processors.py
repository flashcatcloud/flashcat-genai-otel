"""Session enrichment at span start (contract §5) and masking at span end (§6). See contract §5.1 for the AI root marker."""

from __future__ import annotations

import logging
import re
import threading
import time
from collections import OrderedDict
from dataclasses import dataclass, field
from typing import Optional

from opentelemetry import context as otel_context
from opentelemetry import trace as trace_api
from opentelemetry.context import Context
from opentelemetry.sdk.trace import ReadableSpan, Span, SpanProcessor

# ---- Private Context keys for session data. Baggage is opt-in. ----
CONV_KEY = otel_context.create_key("flashcat.conversation_id")
USER_KEY = otel_context.create_key("flashcat.user_id")
TAGS_KEY = otel_context.create_key("flashcat.tags")

TRUNCATED_ATTR = "flashcat.content_truncated"
ROOT_ATTR = "flashcat.genai.root"
TAG_PREFIX = "flashcat.tag."

SEMANTIC_SCOPE = "flashcat.genai"  # Matches _semantics.SCOPE. Keep a local constant to avoid a circular import.
INFERENCE_OPS = ("chat", "text_completion", "generate_content", "embeddings")
NESTED_INFERENCE_PROBLEM = ("手工 llm / embedding span 里又有被插桩的模型调用，同一次调用会重复计数："
                            "只用 llm / embedding 标没有被插桩的模型客户端，或去掉外层的手工 span")


class Counter:
    def __init__(self) -> None:
        self._lock = threading.Lock()
        self.value = 0

    def inc(self) -> None:
        with self._lock:
            self.value += 1


# ---- AI root marker (contract §5.1) ----

# Use the same GenAI classification rule the Flashcat backend uses to decide which spans enter the GenAI span table.
GENAI_KEYS = (
    "gen_ai.operation.name",
    "gen_ai.request.model",
    "gen_ai.provider.name",
    "gen_ai.system",
    "openinference.span.kind",
    "llm.model_name",
    "traceloop.span.kind",
    "langfuse.observation.type",
    "flashcat.span.kind",
    "gen_ai.tool.name",
    "gen_ai.tool.call.id",
    "ai.operationId",
    "llm.request.type",
)
GENAI_PREFIXES = ("gen_ai.", "llm.", "openinference.", "traceloop.", "langfuse.observation.")
# Identify GenAI spans by instrumentation scope when attributes may arrive at span end, as with OpenInference LangChain.
GENAI_SCOPE_PREFIXES = ("openinference.instrumentation.",)


def is_genai(attrs) -> bool:
    if not attrs:
        return False
    for k in GENAI_KEYS:
        if k in attrs:
            return True
    vercel = "ai.operationId" in attrs or "operation.name" in attrs
    for k in attrs:
        # Session processing and baggage can add this key to any span, so exclude it from GenAI classification.
        if k == "gen_ai.conversation.id":
            continue
        if k.startswith(GENAI_PREFIXES) or (vercel and k.startswith("ai.")):
            return True
    return False


def _genai_scope(span: object) -> bool:
    name = getattr(getattr(span, "instrumentation_scope", None), "name", "") or ""
    return name.startswith(GENAI_SCOPE_PREFIXES)


class GenAIAncestry:
    """Track in-process GenAI ancestry and mark GenAI spans with flashcat.genai.root.

    A GenAI span is an AI root if it has no GenAI ancestor. Classify the parent using its attributes when the child starts,
    so attributes added after the parent starts still count. Use GENAI_SCOPE_PREFIXES for instrumentation that sets attributes at span end.
    Keep only the most recent cap spans with GenAI ancestry, so APM-only processes store no entries.
    Treat remote parents as absent because ancestry is currently tracked within a process.
    """

    def __init__(self, cap: int = 65536) -> None:
        self._lock = threading.Lock()
        self._ids: OrderedDict = OrderedDict()
        self._cap = cap

    def has(self, span_id: Optional[int]) -> bool:
        with self._lock:
            return span_id in self._ids

    def start(self, span: Span, parent_span: object, attrs) -> None:
        parent = span.parent
        anc = False
        if parent is not None and not parent.is_remote:
            sc = parent_span.get_span_context() if hasattr(parent_span, "get_span_context") else None
            if sc is not None and sc.span_id == parent.span_id:
                anc = is_genai(getattr(parent_span, "attributes", None)) or _genai_scope(parent_span)
            anc = anc or self.has(parent.span_id)
        if anc:
            with self._lock:
                self._ids[span.context.span_id] = None
                if len(self._ids) > self._cap:
                    self._ids.popitem(last=False)
        if is_genai(attrs) or _genai_scope(span):
            span.set_attribute(ROOT_ATTR, not anc)


class ContextEnricher(SpanProcessor):
    """Write the conversation ID, user ID, and tags to every new span in the session (contract §5).

    Enrich spans at start even if gen_ai.operation.name is absent, as can happen with model SDK built-in instrumentation.
    Children also inherit session attributes from their in-process parents.
    Count orphan inference spans (orphan_inference_spans) to flag where decorators may be needed.
    Count inference spans under semantic API llm or embedding spans (nested_inference_spans, contract §4.1).
    Set the AI root marker for spans that qualify as GenAI at start (contract §5.1).
    MaskingWrapper adds the marker at export time for spans that qualify based on their final attributes.
    Use constant-time reads and writes without serialization.
    """

    def __init__(self) -> None:
        self.orphans = Counter()
        self.nested = Counter()
        self._nested_warned = False
        self.ancestry = GenAIAncestry()

    def on_start(self, span: Span, parent_context: Optional[Context] = None) -> None:
        try:
            attrs = span.attributes or {}
            ctx = parent_context if parent_context is not None else otel_context.get_current()
            conv = otel_context.get_value(CONV_KEY, ctx)
            user = otel_context.get_value(USER_KEY, ctx)
            tags = dict(otel_context.get_value(TAGS_KEY, ctx) or {})
            # Inherit session attributes from the local parent. Context values take precedence.
            parent_attrs = getattr(trace_api.get_current_span(ctx), "attributes", None)
            if parent_attrs:
                conv = conv or parent_attrs.get("gen_ai.conversation.id")
                user = user or parent_attrs.get("user.id")
                for k, v in parent_attrs.items():
                    if k.startswith(TAG_PREFIX):
                        tags.setdefault(k[len(TAG_PREFIX) :], v)
            if conv and "gen_ai.conversation.id" not in attrs:
                span.set_attribute("gen_ai.conversation.id", str(conv))
            if user:
                span.set_attribute("user.id", str(user))
            for k, v in tags.items():
                span.set_attribute(f"{TAG_PREFIX}{k}", str(v))
            # Add GenAI tool-call attributes to built-in MCP 2.x client spans (MCP send tools/call {name}) that only set mcp.method.name.
            if attrs.get("mcp.method.name") == "tools/call" and "gen_ai.operation.name" not in attrs:
                extra = {"gen_ai.operation.name": "execute_tool"}
                name = getattr(span, "name", "") or ""
                i = name.find("tools/call ")
                if i >= 0 and name[i + len("tools/call "):]:
                    extra["gen_ai.tool.name"] = name[i + len("tools/call "):]
                span.set_attributes(extra)
                attrs = span.attributes or {}
            op = attrs.get("gen_ai.operation.name")
            inference = op in INFERENCE_OPS or attrs.get("openinference.span.kind") in ("LLM", "EMBEDDING")
            if span.parent is None and op in INFERENCE_OPS:
                self.orphans.inc()
            if inference and parent_attrs and parent_attrs.get("gen_ai.operation.name") in ("chat", "embeddings"):
                scope = getattr(trace_api.get_current_span(ctx), "instrumentation_scope", None)
                if scope is not None and scope.name == SEMANTIC_SCOPE:
                    self.nested.inc()
                    if not self._nested_warned:
                        self._nested_warned = True
                        logging.getLogger("flashcat.genai").warning("flashcat-genai-otel: %s", NESTED_INFERENCE_PROBLEM)
        except Exception:  # Keep SDK failures from affecting application code.
            pass
        try:
            # Classify the span using the attributes available after MCP enrichment.
            self.ancestry.start(span, trace_api.get_current_span(ctx), span.attributes)
        except Exception:
            pass


# ---- Masking and truncation ----

# L1 masks by an exact match on the final key segment to preserve counters such as gen_ai.usage.input_tokens.
_SENSITIVE_KEY = re.compile(
    r"(^|[._-])(authorization|api[-_]?key|apikey|password|passwd|secret|client[-_]?secret|"
    r"token|access[-_]?token|refresh[-_]?token|auth[-_]?token|cookie|set[-_]?cookie|private[-_]?key)$",
    re.IGNORECASE,
)

CONTENT_KEYS = (
    "gen_ai.input.messages",
    "gen_ai.output.messages",
    "gen_ai.system_instructions",
    "gen_ai.tool.call.arguments",
    "gen_ai.tool.call.result",
    "gen_ai.retrieval.query.text",
    "gen_ai.retrieval.documents",
)

# The default L2 rules match those of the Flashcat ingest gateway. The email regex is O(n²) on long strings without @, such as base64 tool results.
# replace_emails produces identical output. Keep the regex as the rule definition and cross-language reference.
# Use digit boundaries (?<!\d) and (?!\d) because \b fails next to JSON Unicode escapes such as \uXXXX.
DEFAULT_PATTERNS: list[tuple[str, str, str]] = [
    ("email", r"[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}", "[REDACTED:email]"),
    ("cn_id_card", r"(?<!\d)[1-9]\d{5}(?:19|20)\d{2}(?:0[1-9]|1[0-2])(?:0[1-9]|[12]\d|3[01])\d{3}[\dXx](?![\dXx])", "[REDACTED:id_card]"),
    ("bank_card", r"(?<!\d)(?:\d{4}[ \-]?){3}\d{4,7}(?!\d)", "[REDACTED:bank_card]"),
    ("cn_mobile", r"(?<!\d)1[3-9]\d{9}(?!\d)", "[REDACTED:phone]"),
]


_EMAIL_PATTERN = DEFAULT_PATTERNS[0][1]
_LOCAL = frozenset("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789._%+-")
_DOMAIN = frozenset("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789.-")
_ALPHA = frozenset("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz")


def replace_emails(s: str, repl: str) -> str:
    """Apply the default email rule in linear time by visiting each @ with the regex's leftmost-match and greedy-backtracking semantics.

    The regex `[A-Za-z0-9._%+\\-]+@…` consumes the entire local-part run from each starting position before backtracking.
    Without an @, it takes about 0.8 seconds on 32 KiB and can block the application thread for tens of seconds at the 256 KiB scan limit.
    This implementation examines candidate characters only around @ signs.
    """
    out: list[str] = []
    pos = 0  # End of the last replacement, where re.sub resumes searching.
    n = len(s)
    at = s.find("@", pos)
    while at != -1:
        # Find the local part immediately to the left of @, stopping at pos.
        st = at
        while st > pos and s[st - 1] in _LOCAL:
            st -= 1
        if st < at:
            # In the domain to the right of @, greedy backtracking picks the rightmost dot followed by at least two letters.
            e = at + 1
            while e < n and s[e] in _DOMAIN:
                e += 1
            end = -1
            dot = e - 1
            while dot >= at + 2:
                if s[dot] == "." and dot + 2 < e and s[dot + 1] in _ALPHA and s[dot + 2] in _ALPHA:
                    end = dot + 1
                    while end < n and s[end] in _ALPHA:
                        end += 1
                    break
                dot -= 1
            if end != -1:
                out.append(s[pos:st])
                out.append(repl)
                pos = end
                at = s.find("@", pos)
                continue
        at = s.find("@", at + 1)
    out.append(s[pos:])
    return "".join(out)


@dataclass
class MaskingConfig:
    """Configure the three masking and truncation layers (contract §6). L2 runs only when content capture is enabled."""

    key_masking: bool = True
    content_patterns: list[tuple[str, str, str]] = field(default_factory=lambda: list(DEFAULT_PATTERNS))
    extra_patterns: list[tuple[str, str, str]] = field(default_factory=list)
    scan_limit_bytes: int = 256 * 1024  # Default L2 scan limit per value in bytes.
    truncate_bytes: int = 512 * 1024  # Default L3 limit per value, below the server's 1 MB limit.


def _truncate_utf8(s: str, limit: int) -> tuple[str, bool]:
    b = s.encode("utf-8")
    if len(b) <= limit:
        return s, False
    return b[:limit].decode("utf-8", errors="ignore"), True


class MaskingWrapper(SpanProcessor):
    """Wrap BatchSpanProcessor and pass it a modified span copy from on_end, following Logfire's MainSpanProcessorWrapper.

    This protects only the Flashcat export pipeline. Content capture defaults to off, and the backend also applies masking.
    """

    def __init__(self, inner: SpanProcessor, cfg: MaskingConfig, content_enabled: bool, ancestry: Optional[GenAIAncestry] = None) -> None:
        self._inner = inner
        self._cfg = cfg
        self._content = content_enabled
        self._ancestry = ancestry
        self._rules = []  # (name, apply) pairs, with apply taking and returning a string.
        for name, pattern, repl in list(cfg.content_patterns) + list(cfg.extra_patterns):
            if pattern == _EMAIL_PATTERN:
                self._rules.append((name, lambda s, _r=repl: replace_emails(s, _r)))
                continue
            compiled = re.compile(pattern)
            _probe_timeout(compiled)
            self._rules.append((name, lambda s, _rx=compiled, _r=repl: _rx.sub(_r, s)))
        self.masked = Counter()
        self.truncated = Counter()

    def on_start(self, span: Span, parent_context: Optional[Context] = None) -> None:
        self._inner.on_start(span, parent_context)

    def on_end(self, span: ReadableSpan) -> None:
        try:
            span = self._mask(span)
        except Exception:
            pass
        self._inner.on_end(span)

    def _mask(self, span: ReadableSpan) -> ReadableSpan:
        attrs = span.attributes
        if not attrs:
            return span
        extra: dict = {}
        # Use final attributes to set the AI root marker for spans that qualify as GenAI only after start.
        if self._ancestry is not None and ROOT_ATTR not in attrs and is_genai(attrs):
            sc = span.context
            extra[ROOT_ATTR] = not self._ancestry.has(sc.span_id if sc is not None else None)
        changed: dict = {}
        truncated = False
        for k, v in attrs.items():
            if not isinstance(v, str):
                continue
            nv = v
            if self._cfg.key_masking and _SENSITIVE_KEY.search(k):
                nv = "[REDACTED]"
            elif self._content and k in CONTENT_KEYS:
                head, tail = nv[: self._cfg.scan_limit_bytes], nv[self._cfg.scan_limit_bytes :]
                for _, apply in self._rules:
                    head = apply(head)
                nv = head + tail
            nv, cut = _truncate_utf8(nv, self._cfg.truncate_bytes)
            truncated = truncated or cut
            if nv != v:
                changed[k] = nv
        if not changed and not truncated and not extra:
            return span
        if changed:
            self.masked.inc()
        new_attrs = dict(attrs)
        new_attrs.update(changed)
        new_attrs.update(extra)
        if truncated:
            self.truncated.inc()
            new_attrs[TRUNCATED_ATTR] = True
        return ReadableSpan(
            name=span.name,
            context=span.context,
            parent=span.parent,
            resource=span.resource,
            attributes=new_attrs,
            events=span.events,
            links=span.links,
            kind=span.kind,
            status=span.status,
            start_time=span.start_time,
            end_time=span.end_time,
            instrumentation_scope=span.instrumentation_scope,
        )

    def shutdown(self) -> None:
        self._inner.shutdown()

    def force_flush(self, timeout_millis: int = 30000) -> bool:
        return self._inner.force_flush(timeout_millis)


def _probe_timeout(rx: re.Pattern, budget_s: float = 0.05) -> None:
    """Time custom regexes once at init and reject patterns that show excessive backtracking."""
    sample = "a" * 2000 + "@" + "1" * 30 + "!"
    t0 = time.perf_counter()
    rx.search(sample)
    if time.perf_counter() - t0 > budget_s:
        raise ValueError(f"脱敏正则 {rx.pattern!r} 在探测样本上耗时过长，可能存在回溯，请改成线性写法")
