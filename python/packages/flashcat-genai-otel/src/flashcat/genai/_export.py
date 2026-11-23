"""Export OTLP/HTTP protobuf with gzip and Basic auth. CountingExporter only tracks outcomes (contract §3)."""

from __future__ import annotations

import base64
import math
import threading
import time
from datetime import timezone
from email.utils import parsedate_to_datetime
from typing import Sequence

import requests
from opentelemetry.exporter.otlp.proto.http import Compression
from opentelemetry.exporter.otlp.proto.http.metric_exporter import OTLPMetricExporter
from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
from opentelemetry.sdk.trace import ReadableSpan
from opentelemetry.sdk.trace.export import SpanExporter, SpanExportResult
from opentelemetry.sdk.metrics.export import MetricExportResult
from urllib3.util import Timeout

from ._lifecycle import ExportControl


def basic_auth(api_key: str) -> str:
    """Build the Basic auth header from raw keyID:secret credentials. The SDK handles encoding."""
    return "Basic " + base64.b64encode(api_key.encode()).decode()


class ExportStats:
    """Track export counts by outcome for the diagnostics export field (contract §8)."""

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self.counts: dict[str, int] = {"ok": 0, "network": 0, "dropped_queue_full": 0}
        self.last_error: str | None = None
        self.last_status: int | None = None
        self._pending: dict[object, tuple[str, int]] = {}

    def start_batch(self, size: int, signal: str = "spans") -> object:
        token = object()
        with self._lock:
            self._pending[token] = (signal, size)
        return token

    def finish_batch(self, token: object) -> bool:
        with self._lock:
            return self._pending.pop(token, None) is not None

    def drop_pending(self) -> None:
        with self._lock:
            for signal, size in self._pending.values():
                key = "dropped_timeout" if signal == "spans" else "metrics_dropped_timeout"
                self.counts[key] = self.counts.get(key, 0) + size
            if self._pending:
                self.last_error = "导出超过截止时间，已放弃等待"
                self._pending.clear()

    def inc(self, key: str, n: int = 1, error: str | None = None) -> None:
        with self._lock:
            self.counts[key] = self.counts.get(key, 0) + n
            if error:
                self.last_error = error

    def snapshot(self) -> dict:
        with self._lock:
            out = dict(self.counts)
            out["last_error"] = self.last_error
            return out


class _ExportStopped(Exception):
    """Stop upstream retries when bounded retries end by raising outside the RequestException hierarchy."""


def _retry_after(response: requests.Response) -> float | None:
    value = response.headers.get("Retry-After")
    if value is None:
        return None
    try:
        seconds = float(value)
    except ValueError:
        try:
            retry_at = parsedate_to_datetime(value)
        except (TypeError, ValueError, OverflowError):
            return None
        if retry_at.tzinfo is None:
            retry_at = retry_at.replace(tzinfo=timezone.utc)
        seconds = retry_at.timestamp() - time.time()
    return max(0, seconds) if math.isfinite(seconds) else None


class _RecordingSession(requests.Session):
    """Record each request's HTTP status code and partial_success because OTLPSpanExporter reports only SUCCESS or FAILURE."""

    def __init__(self, control: ExportControl, timeout_s: float) -> None:
        super().__init__()
        self._control = control
        self._timeout_s = timeout_s
        self.local = threading.local()

    def begin(self) -> None:
        self.local.deadline = None
        self.local.status = None
        self.local.error = None

    def _deadline(self) -> float | None:
        deadline = self._control.deadline()
        previous = getattr(self.local, "deadline", None)
        if previous is not None:
            deadline = previous if deadline is None else min(previous, deadline)
        self.local.deadline = deadline
        return deadline

    def request(self, *args, **kwargs):  # type: ignore[override]
        retries = 0
        while True:
            deadline = self._deadline()
            request_kwargs = dict(kwargs)
            if deadline is not None:
                remaining = min(self._timeout_s, deadline - time.monotonic())
                if remaining <= 0:
                    raise _ExportStopped("导出已超过截止时间")
                timeout = kwargs.get("timeout")
                if isinstance(timeout, tuple):
                    connect, read = timeout
                else:
                    connect = read = timeout
                request_kwargs["timeout"] = Timeout(
                    total=remaining,
                    connect=min(connect, remaining) if connect is not None else remaining,
                    read=min(read, remaining) if read is not None else remaining,
                )
            self.local.status = None
            retry_after = None
            try:
                resp = super().request(*args, **request_kwargs)
            except requests.RequestException as e:
                self.local.status = "network"
                self.local.error = f"{type(e).__name__}: {e}"
                if self._deadline() is None:
                    raise
                retryable = isinstance(e, (requests.ConnectionError, requests.Timeout))
            else:
                self.local.status = resp.status_code
                if resp.status_code < 300:
                    self.local.error = None
                    return resp
                body = resp.text[:300] if resp.content else ""
                self.local.error = f"HTTP {resp.status_code} {body}".strip()
                if self._deadline() is None:
                    return resp
                retryable = resp.status_code in (429, 502, 503, 504)
                if retryable:
                    retry_after = _retry_after(resp)
                resp.close()

            # Let the HTTP session handle bounded retries to avoid stacking upstream backoff and immediate retries on connection reset.
            deadline = self._deadline()
            if not retryable or retries == 2:
                raise _ExportStopped(self.local.error)
            delay = max((0.2, 0.4)[retries], retry_after or 0)
            next_deadline = self._control.wait_for_retry(delay, deadline)
            if next_deadline is None:
                raise _ExportStopped(self.local.error)
            self.local.deadline = next_deadline
            retries += 1


class CountingExporter(SpanExporter):
    def __init__(self, inner: OTLPSpanExporter, session: _RecordingSession, stats: ExportStats) -> None:
        self._inner = inner
        self._session = session
        self._stats = stats

    def export(self, spans: Sequence[ReadableSpan]) -> SpanExportResult:
        deadline = self._session._control.deadline()
        if deadline is not None and deadline <= time.monotonic():
            self._stats.inc("dropped_timeout", len(spans), error="导出超过截止时间")
            return SpanExportResult.FAILURE
        token = self._stats.start_batch(len(spans))
        self._session.begin()
        try:
            result = self._inner.export(spans)
        except Exception as e:  # Keep SDK failures from affecting application code.
            self._session.local.error = f"{type(e).__name__}: {e}"
            result = SpanExportResult.FAILURE
        if not self._stats.finish_batch(token):
            return SpanExportResult.FAILURE
        status = getattr(self._session.local, "status", None)
        if result is SpanExportResult.SUCCESS:
            self._stats.inc("ok", len(spans))
        elif status == "network" or status is None:
            self._stats.inc("network", error=getattr(self._session.local, "error", "export failed"))
        else:
            bucket = f"http_{status}" if status in (400, 401, 403, 413, 429) else ("http_5xx" if status >= 500 else f"http_{status}")
            self._stats.inc(bucket, error=getattr(self._session.local, "error", None))
        return result

    def shutdown(self) -> None:
        self._inner.shutdown()

    def force_flush(self, timeout_millis: int = 30000) -> bool:
        return self._inner.force_flush(timeout_millis)


class _DeadlineMetricExporter(OTLPMetricExporter):
    def __init__(self, *args, session: _RecordingSession, stats: ExportStats, **kwargs) -> None:
        super().__init__(*args, session=session, **kwargs)
        self._recording_session = session
        self._stats = stats

    def export(self, metrics_data, timeout_millis=10000, **kwargs):
        token = self._stats.start_batch(1, "metrics")
        self._recording_session.begin()
        try:
            result = super().export(metrics_data, timeout_millis=timeout_millis, **kwargs)
        except Exception as e:  # Keep SDK failures from affecting application code.
            self._recording_session.local.error = f"{type(e).__name__}: {e}"
            result = MetricExportResult.FAILURE
        if self._stats.finish_batch(token) and result is MetricExportResult.FAILURE:
            self._stats.inc("metrics_failed", error=getattr(self._recording_session.local, "error", None))
        return result


def span_exporter(endpoint: str, auth_header: str | None, timeout_s: float, stats: ExportStats,
                  control: ExportControl | None = None) -> CountingExporter:
    session = _RecordingSession(control or ExportControl(), timeout_s)
    headers = {"Authorization": auth_header} if auth_header else None
    inner = OTLPSpanExporter(endpoint=endpoint, headers=headers, timeout=timeout_s, compression=Compression.Gzip, session=session)
    return CountingExporter(inner, session, stats)


def metric_exporter(endpoint: str, auth_header: str | None, timeout_s: float,
                    control: ExportControl | None = None, stats: ExportStats | None = None) -> OTLPMetricExporter:
    headers = {"Authorization": auth_header} if auth_header else None
    return _DeadlineMetricExporter(endpoint=endpoint, headers=headers, timeout=timeout_s, compression=Compression.Gzip,
                                   session=_RecordingSession(control or ExportControl(), timeout_s), stats=stats or ExportStats())
