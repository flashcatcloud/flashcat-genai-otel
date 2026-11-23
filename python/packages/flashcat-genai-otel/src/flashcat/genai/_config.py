"""Resolve settings in order of precedence (contract §1).

Explicit arguments > FLASHCAT_* > standard OTEL_* > defaults.
"""

from __future__ import annotations

import math
import os
from dataclasses import dataclass, field
from typing import Iterable, Literal, Mapping
from urllib.parse import unquote, urlparse

ContentMode = Literal[False, "span", "event", "span_and_event"]
ExporterMode = Literal["auto", "add", "reuse", "none"]

_CONTENT_VALUES: dict[str, ContentMode] = {
    "false": False,
    "0": False,
    "no": False,
    "off": False,
    "no_content": False,
    "span": "span",
    "span_only": "span",
    "event": "event",
    "event_only": "event",
    "span_and_event": "span_and_event",
}

# Map capture_content to the four enum values util-genai accepts. Raw true/false values fall back to NO_CONTENT.
UPSTREAM_CONTENT_ENUM = {
    False: "NO_CONTENT",
    "span": "SPAN_ONLY",
    "event": "EVENT_ONLY",
    "span_and_event": "SPAN_AND_EVENT",
}


class ConfigError(ValueError):
    """Invalid argument types are the only errors init raises directly, so they surface during development (contract §2)."""


@dataclass
class Settings:
    service_name: str | None
    environment: str
    endpoint: str | None
    metrics_endpoint: str | None
    api_key: str | None
    auth_header: str | None
    exporter: ExporterMode
    capture_content: ContentMode
    instrumentations: list[str] | None
    disabled_instrumentations: list[str]
    metrics: bool
    resource_attributes: dict[str, str]
    export_timeout_s: float
    shutdown_timeout_s: float
    debug: bool
    disabled: bool
    known_hosts: list[str] = field(default_factory=list)
    problems: list[str] = field(default_factory=list)


def _env(name: str) -> str | None:
    v = os.environ.get(name)
    return v.strip() if v and v.strip() else None


def _parse_kv_list(raw: str | None) -> dict[str, str]:
    """Parse OTEL_RESOURCE_ATTRIBUTES and OTEL_EXPORTER_OTLP_HEADERS as k=v,k=v pairs with URL-encoded values."""
    out: dict[str, str] = {}
    if not raw:
        return out
    for part in raw.split(","):
        if "=" not in part:
            continue
        k, v = part.split("=", 1)
        out[k.strip()] = unquote(v.strip())
    return out


def parse_content(value: object) -> ContentMode:
    if value is False or value is None:
        return False
    if value is True:
        raise ConfigError('capture_content=True 有歧义，请用 "span"（写进 span 属性）或 "event"')
    if isinstance(value, str):
        mode = _CONTENT_VALUES.get(value.strip().lower())
        if mode is not None or value.strip().lower() in _CONTENT_VALUES:
            return mode  # type: ignore[return-value]
    raise ConfigError(f"capture_content 只能是 False / 'span' / 'event' / 'span_and_event'，收到 {value!r}")


def _content_from_env(problems: list[str]) -> ContentMode | None:
    """Report invalid environment values and disable capture. Raise only for invalid explicit arguments (contract §1)."""
    for name in ("FLASHCAT_CAPTURE_CONTENT", "OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT"):
        raw = _env(name)
        if raw is None:
            continue
        key = raw.lower()
        if key in _CONTENT_VALUES:
            return _CONTENT_VALUES[key]
        # Accept the boolean values used by upstream JS, Go, and Java instrumentation so shared deployment settings behave consistently.
        if key in ("true", "1", "yes", "on"):
            return "span"
        problems.append(f"{name}={raw!r} 不是合法取值（false / span / event / span_and_event），按不采集正文处理")
        return False
    return None


def _truthy(v: str | None) -> bool:
    return (v or "").lower() in ("1", "true", "yes", "on")


def resolve(
    service_name: str | None,
    environment: str | None,
    endpoint: str | None,
    api_key: str | None,
    exporter: str,
    capture_content: object,
    instrumentations: Iterable[str] | None,
    disabled_instrumentations: Iterable[str],
    metrics: bool,
    resource_attributes: Mapping[str, str] | None,
    export_timeout_s: float,
    shutdown_timeout_s: float | None,
    debug: bool,
) -> Settings:
    if exporter not in ("auto", "add", "reuse", "none"):
        raise ConfigError(f'exporter 只能是 "auto" / "add" / "reuse" / "none"，收到 {exporter!r}')
    # Check the environment only when the caller omits capture_content (None). Explicit False takes precedence.
    problems: list[str] = []
    if shutdown_timeout_s is None:
        raw = os.environ.get("FLASHCAT_SHUTDOWN_TIMEOUT", "5")
        try:
            shutdown_timeout_s = float(raw)
            if not math.isfinite(shutdown_timeout_s) or shutdown_timeout_s <= 0:
                raise ValueError
        except ValueError:
            problems.append("FLASHCAT_SHUTDOWN_TIMEOUT 必须是大于 0 的有限秒数，使用默认值 5 秒")
            shutdown_timeout_s = 5
    elif isinstance(shutdown_timeout_s, bool) or not isinstance(shutdown_timeout_s, (int, float)) or not math.isfinite(shutdown_timeout_s) or shutdown_timeout_s <= 0:
        raise ConfigError("shutdown_timeout_s 必须是大于 0 的有限秒数")
    content = parse_content(capture_content) if capture_content is not None else _content_from_env(problems)
    if content is None:
        content = False

    res_env = _parse_kv_list(_env("OTEL_RESOURCE_ATTRIBUTES"))

    svc = service_name or _env("FLASHCAT_SERVICE_NAME") or _env("OTEL_SERVICE_NAME") or res_env.get("service.name")
    env_name = (
        environment
        or _env("FLASHCAT_ENVIRONMENT")
        or res_env.get("deployment.environment.name")
        or res_env.get("deployment.environment")
        or "prod"
    )

    ep = endpoint or _env("FLASHCAT_ENDPOINT") or _env("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")
    if ep is None and (base := _env("OTEL_EXPORTER_OTLP_ENDPOINT")):
        ep = base.rstrip("/") + "/v1/traces"
    if ep is not None:
        parsed = urlparse(ep)
        if parsed.scheme not in ("http", "https") or not parsed.netloc:
            problems.append(f"endpoint 必须是完整 URL（含 http(s):// 与 /v1/traces），收到 {ep!r}；不自动补端口，以免落到无鉴权入口")
            ep = None
        elif not parsed.path or parsed.path == "/":
            problems.append(f"endpoint 缺少路径，应为 .../v1/traces，收到 {ep!r}")
            ep = None

    metrics_ep = _env("FLASHCAT_METRICS_ENDPOINT") or _env("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT")
    if metrics_ep is None and ep and ep.endswith("/v1/traces"):
        metrics_ep = ep[: -len("/v1/traces")] + "/v1/metrics"

    key = api_key or _env("FLASHCAT_API_KEY")
    auth_header = None
    if key is None:
        headers = _parse_kv_list(_env("OTEL_EXPORTER_OTLP_TRACES_HEADERS") or _env("OTEL_EXPORTER_OTLP_HEADERS"))
        for k, v in headers.items():
            if k.lower() == "authorization":
                auth_header = v
    elif ":" not in key:
        problems.append("api_key 应为 keyID:secret（不要传 base64，也不要传 URL 编码的 Authorization）")
        key = None

    disabled = _truthy(_env("FLASHCAT_DISABLED")) or _truthy(_env("OTEL_SDK_DISABLED"))
    disabled_insts = list(disabled_instrumentations)
    for name in (_env("OTEL_PYTHON_DISABLED_INSTRUMENTATIONS") or "").split(","):
        if name.strip():
            disabled_insts.append(name.strip())

    known = [h.strip().lower() for h in (_env("FLASHCAT_KNOWN_HOSTS") or "").split(",") if h.strip()]
    if ep:
        known.append((urlparse(ep).hostname or "").lower())

    attrs = dict(resource_attributes or {})
    return Settings(
        service_name=svc,
        environment=env_name,
        endpoint=ep,
        metrics_endpoint=metrics_ep,
        api_key=key,
        auth_header=auth_header,
        exporter=exporter,  # type: ignore[arg-type]
        capture_content=content,
        instrumentations=list(instrumentations) if instrumentations is not None else None,
        disabled_instrumentations=disabled_insts,
        metrics=metrics,
        resource_attributes=attrs,
        export_timeout_s=export_timeout_s,
        shutdown_timeout_s=shutdown_timeout_s,
        debug=debug,
        disabled=disabled,
        known_hosts=[h for h in known if h],
        problems=problems,
    )
