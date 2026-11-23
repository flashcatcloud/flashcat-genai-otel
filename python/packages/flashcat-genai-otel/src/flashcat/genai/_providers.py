"""Handle the three TracerProvider and MeterProvider scenarios using public SDK APIs (contract §2.4)."""

from __future__ import annotations

import os
from dataclasses import dataclass
from urllib.parse import urlparse

from opentelemetry import metrics, trace
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.metrics.export import PeriodicExportingMetricReader
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor

from ._config import Settings
from ._export import ExportStats, basic_auth, metric_exporter, span_exporter
from ._lifecycle import ExportControl
from ._processors import ContextEnricher, MaskingConfig, MaskingWrapper
from .version import __version__


@dataclass
class ProviderResult:
    situation: str  # A / B / C / unsupported
    exporter_mode: str  # add / reuse / none
    tracer_provider: object
    meter_provider: object | None
    service_name: str | None
    notes: list[str]
    masking: MaskingWrapper | None = None
    metrics_mode: str = "none"
    owns_tracer_provider: bool = False
    owns_meter_provider: bool = False
    control: ExportControl | None = None


def _existing_endpoint_host() -> str | None:
    ep = os.environ.get("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") or os.environ.get("OTEL_EXPORTER_OTLP_ENDPOINT")
    if not ep:
        return None
    return (urlparse(ep).hostname or "").lower() or None


def _auth(s: Settings) -> str | None:
    if s.api_key:
        return basic_auth(s.api_key)
    return s.auth_header


def build_resource(s: Settings) -> Resource:
    attrs = {
        "service.name": s.service_name or "unknown_service",
        "deployment.environment.name": s.environment,
        "telemetry.distro.name": "flashcat-genai-otel",
        "telemetry.distro.version": __version__,
    }
    attrs.update(s.resource_attributes)
    return Resource.create(attrs)


def setup_tracing(s: Settings, enricher: ContextEnricher, masking: MaskingConfig, stats: ExportStats) -> ProviderResult:
    notes: list[str] = []
    control = ExportControl()
    current = trace.get_tracer_provider()
    is_proxy = isinstance(current, trace.ProxyTracerProvider)
    is_sdk = isinstance(current, TracerProvider)

    if is_proxy:
        situation = "A"
    elif is_sdk:
        host = _existing_endpoint_host()
        situation = "C" if host and host in s.known_hosts else "B"
    else:
        situation = "unsupported"
        notes.append(f"进程里已有非 SDK 的 TracerProvider（{type(current).__name__}），无法追加处理器，只装插桩")

    mode = s.exporter
    if mode == "auto":
        mode = {"A": "add", "B": "add", "C": "reuse", "unsupported": "none"}[situation]
    if situation == "C" and s.exporter == "add":
        notes.append("进程已在往 Flashcat 发数据，显式 exporter='add' 可能产生重复上报")
    if mode == "reuse":
        notes.append("未新增 exporter，数据经进程里已有的 exporter 发出（GenAI 数据由平台侧识别）")

    if situation == "A":
        provider = TracerProvider(resource=build_resource(s), shutdown_on_exit=False)
        trace.set_tracer_provider(provider)
    else:
        provider = current
        if is_sdk:
            actual = provider.resource.attributes.get("service.name")  # type: ignore[attr-defined]
            if actual and s.service_name and actual != s.service_name:
                notes.append(f"已有 provider 的 service.name 是 {actual!r}，与参数 {s.service_name!r} 不同；OTel 不支持修改 resource，以实际值为准")

    wrapper = None
    if isinstance(provider, TracerProvider):
        provider.add_span_processor(enricher)
        if mode == "add":
            if not s.endpoint:
                notes.append("缺少 endpoint（FLASHCAT_ENDPOINT，完整 .../v1/traces URL），未启用导出")
                mode = "none"
            elif not _auth(s):
                notes.append("缺少 api_key（FLASHCAT_API_KEY=keyID:secret），未启用导出")
                mode = "none"
            else:
                exporter = span_exporter(s.endpoint, _auth(s), s.export_timeout_s, stats, control)
                wrapper = MaskingWrapper(BatchSpanProcessor(exporter), masking, s.capture_content is not False, enricher.ancestry)
                provider.add_span_processor(wrapper)
    return ProviderResult(situation, mode, provider, None, _service_name(provider, s), notes, wrapper,
                          owns_tracer_provider=situation == "A", control=control)


def _service_name(provider: object, s: Settings) -> str | None:
    if isinstance(provider, TracerProvider):
        return provider.resource.attributes.get("service.name") or s.service_name  # type: ignore[return-value]
    return s.service_name


def setup_metrics(s: Settings, res: ProviderResult, stats: ExportStats | None = None) -> None:
    """Set up the MeterProvider too, since util-genai silently drops gen_ai.client.* metrics with only a TracerProvider."""
    if not s.metrics:
        res.metrics_mode = "disabled"
        return
    current = metrics.get_meter_provider()
    if isinstance(current, MeterProvider):
        res.meter_provider = current
        res.metrics_mode = "reuse"
        return
    if res.exporter_mode != "add" or not s.metrics_endpoint:
        res.metrics_mode = "none"
        if res.exporter_mode == "add":
            res.notes.append("没有 metrics endpoint，gen_ai.client.* 指标不导出")
        return
    reader = PeriodicExportingMetricReader(
        metric_exporter(s.metrics_endpoint, _auth(s), s.export_timeout_s, res.control, stats), export_interval_millis=60_000
    )
    resource = res.tracer_provider.resource if isinstance(res.tracer_provider, TracerProvider) else build_resource(s)
    mp = MeterProvider(resource=resource, metric_readers=[reader], shutdown_on_exit=False)
    metrics.set_meter_provider(mp)
    res.meter_provider = mp
    res.metrics_mode = "add"
    res.owns_meter_provider = True
