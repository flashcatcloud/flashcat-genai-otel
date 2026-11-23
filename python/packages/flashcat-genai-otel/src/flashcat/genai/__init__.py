"""Flashcat GenAI OTel SDK for one-line AI agent observability.

    from flashcat import genai
    diagnostics = genai.init(service_name="kefu-agent")

See spec/contract.md for the cross-language contract.
"""

from __future__ import annotations

import atexit
import json
import logging
import os
import threading
import time
from typing import Iterable, Literal, Mapping

from opentelemetry import metrics as _metrics
from opentelemetry import trace as _trace
from opentelemetry.util.genai.handler import TelemetryHandler

from . import _config, _loader, _providers, _semantics
from ._config import ConfigError, UPSTREAM_CONTENT_ENUM
from ._export import ExportStats
from ._lifecycle import run_bounded
from ._processors import NESTED_INFERENCE_PROBLEM, ContextEnricher, MaskingConfig
from ._semantics import agent, annotate, embedding, llm, retrieval, session, tool, trace, workflow
from .version import __version__

__all__ = [
    "init",
    "flush",
    "shutdown",
    "diagnostics",
    "agent",
    "tool",
    "workflow",
    "llm",
    "embedding",
    "retrieval",
    "annotate",
    "session",
    "trace",
    "MaskingConfig",
    "ConfigError",
    "__version__",
]

logger = logging.getLogger("flashcat.genai")

_lock = threading.Lock()
_shutdown_lock = threading.Lock()
_state: dict = {}


def init(
    service_name: str | None = None,
    *,
    environment: str | None = None,
    endpoint: str | None = None,
    api_key: str | None = None,
    exporter: Literal["auto", "add", "reuse", "none"] = "auto",
    capture_content: Literal[False, "span", "event", "span_and_event"] | None = None,
    instrumentations: Iterable[str] | None = None,
    disabled_instrumentations: Iterable[str] = (),
    metrics: bool = True,
    masking: MaskingConfig | None = None,
    resource_attributes: Mapping[str, str] | None = None,
    export_timeout_s: float = 10,
    shutdown_timeout_s: float | None = None,
    debug: bool = False,
    prefer: Literal["foreign", "flashcat"] = "foreign",
) -> dict:
    """Initialize the SDK and return diagnostics (contract §2).

    Initialization failures fall back and are recorded in diagnostics. Only invalid argument types raise ValueError.
    Call this before importing model SDKs or frameworks.
    """
    with _lock:
        if _state.get("initialized"):
            logger.warning("flashcat-genai-otel: init 已调用过，本次忽略（OTel 不允许替换已设置的全局 TracerProvider）")
            return diagnostics()

        s = _config.resolve(
            service_name, environment, endpoint, api_key, exporter, capture_content, instrumentations,
            disabled_instrumentations, metrics, resource_attributes, export_timeout_s, shutdown_timeout_s, debug,
        )
        diag: dict = {
            "sdk": {"name": "flashcat-genai-otel", "version": __version__, "language": "python"},
            "enabled": False,
            "service_name": s.service_name,
            "environment": s.environment,
            "endpoint": s.endpoint,
            "problems": list(s.problems),
            "notes": [],
        }
        _state.update(initialized=True, diag=diag, settings=s)

        if s.disabled:
            diag["notes"].append("FLASHCAT_DISABLED / OTEL_SDK_DISABLED 已开启，未做任何事")
            _emit(diag, debug)
            return diag
        if not s.service_name or s.service_name.startswith("unknown_service"):
            diag["problems"].append("缺少 service_name（参数或 OTEL_SERVICE_NAME），且不能是 unknown_service*；未启用导出")
            s.exporter = "none" if s.exporter in ("auto", "add") else s.exporter

        # Set the content capture switch before loading instrumentation. util-genai reads it once when creating a TelemetryHandler.
        _apply_content(s, diag)

        stats = ExportStats()
        enricher = ContextEnricher()
        _state.update(stats=stats, enricher=enricher)
        try:
            res = _providers.setup_tracing(s, enricher, masking or MaskingConfig(), stats)
            _state["providers"] = res
            atexit.register(_atexit, s.shutdown_timeout_s)
            _providers.setup_metrics(s, res, stats)
        except Exception as e:  # noqa: BLE001
            diag["problems"].append(f"初始化 provider 失败：{type(e).__name__}: {e}")
            _emit(diag, debug)
            return diag
        diag["enabled"] = res.exporter_mode != "none" or res.situation == "C"
        diag["service_name"] = res.service_name
        diag["provider"] = {"situation": res.situation, "exporter_mode": res.exporter_mode, "metrics": res.metrics_mode}
        diag["notes"].extend(res.notes)
        diag["distro_active"] = os.environ.get("OTEL_PYTHON_DISTRO") or "flashcat-manual"

        _semantics.set_handler(
            TelemetryHandler(
                tracer_provider=res.tracer_provider,
                meter_provider=res.meter_provider,
                instrumentation_scope_name=_semantics.SCOPE,
                instrumentation_scope_version=__version__,
            )
        )

        try:
            diag["instrumentations"] = _loader.load(
                res.tracer_provider, res.meter_provider or _metrics.get_meter_provider(),
                s.instrumentations, s.disabled_instrumentations, prefer,
            )
            _instrumentation_diagnostics(diag)
            _loader.schedule_recheck(diag["instrumentations"])
        except Exception as e:  # noqa: BLE001
            diag["problems"].append(f"装载插桩失败：{type(e).__name__}: {e}")
            diag["instrumentations"] = []

        _emit(diag, debug)
        return diag


def _apply_content(s: _config.Settings, diag: dict) -> None:
    """Apply one content capture switch across sources (contract §7).

    The switch takes effect for instrumentation the SDK loads. For other sources, it is applied on a best-effort basis.
    """
    os.environ["OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT"] = UPSTREAM_CONTENT_ENUM[s.capture_content]
    content = {"upstream": UPSTREAM_CONTENT_ENUM[s.capture_content]}
    on = s.capture_content is not False
    if "TRACELOOP_TRACE_CONTENT" not in os.environ:
        os.environ["TRACELOOP_TRACE_CONTENT"] = "true" if on else "false"
    content["openllmetry"] = "best_effort:" + os.environ["TRACELOOP_TRACE_CONTENT"]
    for k in ("OPENINFERENCE_HIDE_INPUTS", "OPENINFERENCE_HIDE_OUTPUTS"):
        if k not in os.environ:
            os.environ[k] = "false" if on else "true"
    content["openinference"] = "best_effort:hide=" + os.environ["OPENINFERENCE_HIDE_INPUTS"]
    if s.capture_content in ("event", "span_and_event"):
        diag["notes"].append("正文事件模式需要 LoggerProvider 与 logs exporter；一期 Flashcat 只关联 span 属性里的正文，建议用 'span'")
    diag["content"] = content


def _emit(diag: dict, debug: bool) -> None:
    warned = _state.setdefault("warned_problems", set())
    for problem in diag.get("problems", []):
        if problem not in warned:
            warned.add(problem)
            logger.warning("flashcat-genai-otel: %s", problem)
    if debug:
        logger.info("flashcat-genai-otel diagnostics: %s", json.dumps(diag, ensure_ascii=False, default=str))
        return
    brief = {
        "service": diag.get("service_name"),
        "enabled": diag.get("enabled"),
        "provider": diag.get("provider"),
        "installed": [i["lib"] for i in diag.get("instrumentations", []) if i.get("status") == "installed"],
        "problems": diag.get("problems"),
    }
    logger.info("flashcat-genai-otel: %s", json.dumps(brief, ensure_ascii=False))


def _instrumentation_diagnostics(diag: dict) -> None:
    for item in diag.get("instrumentations", []):
        if item.get("status") == "instrumentation_not_installed":
            problem = f"检测到 {item['lib']}，但未安装对应插桩包，无法采集该库的 span；请执行 {item['hint']}"
            if problem not in diag["problems"]:
                diag["problems"].append(problem)
        if (item["lib"] == "openai-agents" and item.get("status") in ("installed", "already_instrumented")
                and os.environ.get("OPENAI_AGENTS_DISABLE_TRACING", "").strip().lower() in ("1", "true", "yes")):
            note = ("OPENAI_AGENTS_DISABLE_TRACING 已开启，框架层 invoke_workflow / invoke_agent / execute_tool span 会缺失；"
                    "模型 span 不受影响。请移除该环境变量或设为 0，并重启进程以恢复框架 tracing。")
            if note not in diag["notes"]:
                diag["notes"].append(note)


def diagnostics() -> dict:
    """Return the same diagnostics structure as init, with live export counts and stable field names for FlashAI and coding agents."""
    diag = dict(_state.get("diag") or {"enabled": False, "problems": ["init 尚未调用"]})
    stats: ExportStats | None = _state.get("stats")
    if stats is not None:
        diag["export"] = stats.snapshot()
    enricher: ContextEnricher | None = _state.get("enricher")
    res = _state.get("providers")
    sem: dict = {}
    if enricher is not None:
        sem["orphan_inference_spans"] = enricher.orphans.value
        sem["nested_inference_spans"] = enricher.nested.value
        if enricher.nested.value and NESTED_INFERENCE_PROBLEM not in diag.get("problems", []):
            diag["problems"] = [*diag.get("problems", []), NESTED_INFERENCE_PROBLEM]
    if res is not None and res.masking is not None:
        sem["masked_spans"] = res.masking.masked.value
        sem["truncated_spans"] = res.masking.truncated.value
    if sem:
        diag["semantics"] = sem
    return diag


def flush(timeout_s: float = 5) -> bool:
    """Flush before a script or serverless function exits."""
    deadline = time.monotonic() + max(0, timeout_s)
    res = _state.get("providers")
    if res is None or res.control is None or _state.get("shutdown_started"):
        return False
    token = res.control.begin(deadline)
    actions = []
    for p in (_trace.get_tracer_provider(), _metrics.get_meter_provider()):
        fn = getattr(p, "force_flush", None)
        if fn is not None:
            actions.append(lambda ms, fn=fn: fn(timeout_millis=ms))
    ok = run_bounded(actions, deadline, res.control, token)
    if not ok:
        _state["stats"].drop_pending()
    return ok


def shutdown(timeout_s: float | None = None) -> None:
    """Shut down only SDK-created providers. Draining traces and collecting final metrics share a deadline."""
    started = time.monotonic()
    res = _state.get("providers")
    if res is None or res.control is None:
        return
    with _shutdown_lock:
        if _state.get("shutdown_started"):
            return
        _state["shutdown_started"] = True
    if timeout_s is None:
        timeout_s = _state["settings"].shutdown_timeout_s
    deadline = started + max(0, timeout_s)
    token = res.control.begin(deadline, shutdown=True)
    actions = []
    if res.owns_tracer_provider:
        actions.append(lambda ms: res.tracer_provider.shutdown())
    elif res.masking is not None:
        actions.append(lambda ms: res.masking.shutdown())
    else:
        fn = getattr(res.tracer_provider, "force_flush", None)
        if fn is not None:
            actions.append(lambda ms: fn(timeout_millis=ms))
    if res.meter_provider is not None:
        if res.owns_meter_provider:
            actions.append(lambda ms: res.meter_provider.shutdown(timeout_millis=ms))
        else:
            actions.append(lambda ms: res.meter_provider.force_flush(timeout_millis=ms))
    if not run_bounded(actions, deadline, res.control, token):
        _state["stats"].drop_pending()


def _atexit(timeout_s: float) -> None:
    shutdown(timeout_s)
