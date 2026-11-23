from __future__ import annotations

import json
import logging
import os
import subprocess
import sys
import threading
import time
from pathlib import Path

import pytest
import requests


def settings(timeout=None):
    from flashcat.genai._config import resolve

    return resolve("test", None, None, None, "none", False, [], [], True, None, 10, timeout, False)


@pytest.mark.parametrize("value", ["2", "0.25", " 1.5 "])
def test_shutdown_timeout_env(monkeypatch, value):
    monkeypatch.setenv("FLASHCAT_SHUTDOWN_TIMEOUT", value)
    assert settings().shutdown_timeout_s == float(value)
    assert settings(3).shutdown_timeout_s == 3


@pytest.mark.parametrize("value", ["", "bad", "0", "-1", "nan", "inf"])
def test_invalid_shutdown_timeout_env(monkeypatch, value):
    monkeypatch.setenv("FLASHCAT_SHUTDOWN_TIMEOUT", value)
    s = settings()
    assert s.shutdown_timeout_s == 5
    assert len(s.problems) == 1 and "FLASHCAT_SHUTDOWN_TIMEOUT" in s.problems[0]
    assert settings(2).problems == []


@pytest.mark.parametrize("value", [True, "2", 0, -1, float("nan"), float("inf")])
def test_invalid_explicit_shutdown_timeout(value):
    from flashcat.genai._config import ConfigError

    with pytest.raises(ConfigError):
        settings(value)


@pytest.mark.parametrize("disabled", [None, "openai", "*"])
def test_zero_code_reports_instrumentation_and_delivers_spans(collector, disabled):
    env = {k: v for k, v in os.environ.items() if not k.startswith(("OTEL_", "FLASHCAT_")) and "proxy" not in k.lower()}
    env.update(OTEL_PYTHON_DISTRO="flashcat", OTEL_SERVICE_NAME="zero-code",
               FLASHCAT_ENDPOINT=collector.url + "/v1/traces", FLASHCAT_API_KEY="test:secret")
    if disabled is not None:
        env["OTEL_PYTHON_DISABLED_INSTRUMENTATIONS"] = disabled
    code = """
import json
from flashcat import genai
from opentelemetry.instrumentation.genai.openai import OpenAIInstrumentor
with genai.trace("zero-code"):
    pass
print("RESULT:" + json.dumps(dict(diag=genai.diagnostics(), patched=OpenAIInstrumentor().is_instrumented_by_opentelemetry)))
"""
    p = subprocess.run([str(Path(sys.executable).with_name("opentelemetry-instrument")), sys.executable, "-c", code],
                       env=env, capture_output=True, text=True, timeout=10)
    assert p.returncode == 0, p.stderr
    result = json.loads(next(line[7:] for line in p.stdout.splitlines() if line.startswith("RESULT:")))
    items = [i for i in result["diag"]["instrumentations"] if i["lib"] == "openai"]
    assert len(items) == 1
    assert items[0]["status"] == ("installed" if disabled is None else "disabled")
    assert result["patched"] is (disabled is None)
    assert [s["name"] for s in collector.spans()] == ["zero-code"]


def test_missing_instrumentation_warns_once(monkeypatch, caplog):
    from flashcat import genai
    from flashcat.genai import _loader
    from flashcat.genai._catalog import Entry

    monkeypatch.setattr(_loader, "CATALOG", (Entry("agents-test", "upstream", "missing_instrumentation:Instrumentor", "json", "json"),))
    items = _loader.load(None, None, None, [])
    assert items[0]["status"] == "instrumentation_not_installed"
    diag = dict(problems=[], notes=[], instrumentations=items)
    monkeypatch.setattr(genai, "_state", {})
    with caplog.at_level(logging.WARNING, logger="flashcat.genai"):
        for _ in range(2):
            genai._instrumentation_diagnostics(diag)
            genai._emit(diag, False)
    assert len(diag["problems"]) == 1
    assert "pip install 'flashcat-genai-otel[agents-test]'" in diag["problems"][0]
    assert len(caplog.records) == 1


def test_init_publishes_instrumentation_problems_and_notes(run_script):
    out = run_script("""
        import json
        from flashcat import genai
        genai._loader.load = lambda *args: [
            dict(lib="openai-agents", status="installed", source="upstream"),
            dict(lib="anthropic", status="instrumentation_not_installed", hint="pip install 'flashcat-genai-otel[anthropic]'"),
        ]
        d = genai.init(service_name="test", exporter="none", metrics=False)
        print("RESULT:" + json.dumps(d))
    """, {"OPENAI_AGENTS_DISABLE_TRACING": "TRUE"})
    assert any("anthropic" in p for p in out["problems"])
    assert any("模型 span 不受影响" in n for n in out["notes"])


def test_namespace_is_not_a_target_library(monkeypatch):
    from flashcat.genai import _loader
    from flashcat.genai._catalog import Entry

    monkeypatch.setattr(_loader, "CATALOG", (Entry("namespace-child", "upstream", "missing:Inst", "json.no_such_sdk", "missing"),))
    assert _loader.load(None, None, None, [])[0]["status"] == "not_present"


@pytest.mark.parametrize("value", ["1", "TRUE", "Yes"])
@pytest.mark.parametrize("status", ["installed", "already_instrumented"])
def test_agents_disabled_tracing_note(monkeypatch, value, status):
    from flashcat import genai

    monkeypatch.setenv("OPENAI_AGENTS_DISABLE_TRACING", value)
    diag = dict(problems=[], notes=[], instrumentations=[dict(lib="openai-agents", status=status)])
    genai._instrumentation_diagnostics(diag)
    genai._instrumentation_diagnostics(diag)
    assert len(diag["notes"]) == 1
    assert all(text in diag["notes"][0] for text in ("invoke_agent", "模型 span 不受影响", "设为 0", "重启"))


@pytest.mark.parametrize("value,status", [("0", "installed"), ("false", "installed"), ("1", "not_present"), ("yes", "disabled")])
def test_agents_tracing_note_only_when_loaded_and_disabled(monkeypatch, value, status):
    from flashcat import genai

    monkeypatch.setenv("OPENAI_AGENTS_DISABLE_TRACING", value)
    diag = dict(problems=[], notes=[], instrumentations=[dict(lib="openai-agents", status=status)])
    genai._instrumentation_diagnostics(diag)
    assert diag["notes"] == []


@pytest.mark.parametrize("failure", ["network", 429, 503])
@pytest.mark.parametrize("already_running", [False, True])
def test_closing_export_without_retry_budget(monkeypatch, failure, already_running):
    from opentelemetry.sdk.trace import TracerProvider
    from opentelemetry.sdk.trace.export import SpanExportResult
    from flashcat.genai._export import ExportStats, span_exporter
    from flashcat.genai._lifecycle import ExportControl

    calls = []

    def request(self, *args, **kwargs):
        calls.append(kwargs["timeout"])
        if already_running:
            control.begin(time.monotonic() + 0.2, shutdown=True)
        if failure == "network":
            raise requests.ConnectionError("test connection refused")
        response = requests.Response()
        response.status_code, response._content = failure, b"retry"
        return response

    monkeypatch.setattr(requests.Session, "request", request)
    control, stats = ExportControl(), ExportStats()
    if not already_running:
        control.begin(time.monotonic() + 0.2, shutdown=True)
    exporter = span_exporter("http://127.0.0.1:19119/v1/traces", None, 10, stats, control)
    provider = TracerProvider(shutdown_on_exit=False)
    span = provider.get_tracer("test").start_span("one")
    span.end()
    assert exporter.export([span]) is SpanExportResult.FAILURE
    assert len(calls) == 1
    if not already_running:
        assert 0 < calls[0].total <= 0.2
    bucket = "network" if failure == "network" else ("http_5xx" if failure == 503 else "http_429")
    assert stats.snapshot()[bucket] == 1
    exporter.shutdown()


def test_expired_export_is_counted_without_request(monkeypatch):
    from opentelemetry.sdk.trace.export import SpanExportResult
    from flashcat.genai._export import ExportStats, span_exporter
    from flashcat.genai._lifecycle import ExportControl

    def request(*args, **kwargs):
        pytest.fail("expired exports must not send requests")

    monkeypatch.setattr(requests.Session, "request", request)
    control, stats = ExportControl(), ExportStats()
    control.begin(time.monotonic() - 1, shutdown=True)
    exporter = span_exporter("http://127.0.0.1:19119/v1/traces", None, 10, stats, control)
    assert exporter.export([None, None]) is SpanExportResult.FAILURE
    assert stats.snapshot()["dropped_timeout"] == 2
    exporter.shutdown()


def test_closing_metrics_export_without_retry_budget(monkeypatch):
    from opentelemetry.sdk.metrics import MeterProvider
    from opentelemetry.sdk.metrics.export import InMemoryMetricReader, MetricExportResult
    from flashcat.genai._export import ExportStats, metric_exporter
    from flashcat.genai._lifecycle import ExportControl

    calls = []

    def request(self, *args, **kwargs):
        calls.append(kwargs["timeout"])
        response = requests.Response()
        response.status_code, response._content = 503, b"retry"
        return response

    monkeypatch.setattr(requests.Session, "request", request)
    reader = InMemoryMetricReader()
    provider = MeterProvider(metric_readers=[reader], shutdown_on_exit=False)
    provider.get_meter("test").create_counter("test").add(1)
    control, stats = ExportControl(), ExportStats()
    control.begin(time.monotonic() + 0.2, shutdown=True)
    exporter = metric_exporter("http://127.0.0.1:19119/v1/metrics", None, 10, control, stats)
    assert exporter.export(reader.get_metrics_data()) is MetricExportResult.FAILURE
    assert len(calls) == 1 and 0 < calls[0].total <= 0.2
    assert stats.snapshot()["metrics_failed"] == 1
    exporter.shutdown()
    provider.shutdown()


@pytest.mark.parametrize("operation", ["flush", "shutdown"])
def test_total_budget_and_no_duplicate_shutdown(monkeypatch, operation):
    from flashcat import genai
    from flashcat.genai._export import ExportStats
    from flashcat.genai._lifecycle import ExportControl
    from flashcat.genai._providers import ProviderResult

    release = threading.Event()
    calls = []

    class Provider:
        def force_flush(self, timeout_millis):
            calls.append(("flush", timeout_millis))
            release.wait(1)
            return True

        def shutdown(self, timeout_millis=None):
            calls.append(("shutdown", timeout_millis))
            release.wait(1)

    tp, mp, stats = Provider(), Provider(), ExportStats()
    stats.start_batch(7)
    res = ProviderResult("A", "add", tp, mp, "test", [], owns_tracer_provider=True,
                         owns_meter_provider=True, control=ExportControl())
    monkeypatch.setattr(genai, "_state", dict(providers=res, stats=stats))
    monkeypatch.setattr(genai._trace, "get_tracer_provider", lambda: tp)
    monkeypatch.setattr(genai._metrics, "get_meter_provider", lambda: mp)
    try:
        t0 = time.monotonic()
        result = getattr(genai, operation)(0.05)
        assert 0.04 <= time.monotonic() - t0 < 0.3
        assert result is (False if operation == "flush" else None)
        assert len(calls) == 2
        assert stats.snapshot()["dropped_timeout"] == 7
        if operation == "shutdown":
            t0 = time.monotonic()
            genai.shutdown(1)
            assert time.monotonic() - t0 < 0.05 and len(calls) == 2
    finally:
        release.set()


def test_foreign_providers_are_not_closed(monkeypatch):
    from flashcat import genai
    from flashcat.genai._export import ExportStats
    from flashcat.genai._lifecycle import ExportControl
    from flashcat.genai._providers import ProviderResult

    calls = []

    class Foreign:
        def shutdown(self, **kwargs):
            pytest.fail("foreign provider must not be shut down")

        def force_flush(self, timeout_millis):
            calls.append(timeout_millis)
            return True

    res = ProviderResult("B", "reuse", Foreign(), Foreign(), "test", [], control=ExportControl())
    monkeypatch.setattr(genai, "_state", dict(providers=res, stats=ExportStats()))
    genai.shutdown(0.2)
    assert len(calls) == 2


def test_sdk_providers_have_no_automatic_exit_hooks(collector, run_script):
    out = run_script("""
        import json
        from flashcat import genai
        genai.init(service_name="hooks", instrumentations=[])
        res = genai._state["providers"]
        print("RESULT:" + json.dumps(dict(traces=res.tracer_provider._atexit_handler,
                                         metrics=res.meter_provider._atexit_handler)))
    """, {"FLASHCAT_ENDPOINT": collector.url + "/v1/traces", "FLASHCAT_API_KEY": "test:secret"})
    assert out == dict(traces=None, metrics=None)
