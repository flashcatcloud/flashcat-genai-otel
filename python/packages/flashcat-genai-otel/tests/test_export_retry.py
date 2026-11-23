from __future__ import annotations

import threading
import time
from datetime import datetime, timezone
from email.utils import format_datetime
from types import SimpleNamespace

import pytest
import requests


@pytest.fixture(params=["traces", "metrics"])
def export_case(request, monkeypatch):
    from opentelemetry.sdk.metrics import MeterProvider
    from opentelemetry.sdk.metrics.export import InMemoryMetricReader, MetricExportResult
    from opentelemetry.sdk.trace import TracerProvider
    from opentelemetry.sdk.trace.export import SpanExportResult
    from flashcat.genai import _export, _lifecycle

    cases = []

    def build(timeout_s=10):
        clock = SimpleNamespace(now=100.0)
        clock.monotonic = lambda: clock.now
        clock.time = lambda: 1700000000 + clock.now - 100
        monkeypatch.setattr(_export, "time", clock)
        monkeypatch.setattr(_lifecycle, "time", clock)
        control, stats = _lifecycle.ExportControl(), _export.ExportStats()
        case = SimpleNamespace(control=control, stats=stats, clock=clock, calls=[], waits=[],
                               outcomes=[], headers={}, on_request=lambda: None, request_duration=0)

        def wait(delay):
            case.waits.append(delay)
            clock.now += delay

        def send(self, *args, **kwargs):
            case.calls.append(dict(kwargs, at=clock.now))
            case.on_request()
            clock.now += case.request_duration
            outcome = case.outcomes.pop(0) if case.outcomes else 200
            if isinstance(outcome, Exception):
                raise outcome
            response = requests.Response()
            response.status_code, response._content = outcome, b""
            response.headers.update(case.headers)
            return response

        monkeypatch.setattr(control._changed, "wait", wait)
        monkeypatch.setattr(requests.Session, "request", send)
        if request.param == "traces":
            provider = TracerProvider(shutdown_on_exit=False)
            span = provider.get_tracer("test").start_span("one")
            span.end()
            exporter = _export.span_exporter("http://127.0.0.1:19119/v1/traces", None, timeout_s, stats, control)
            case.data, case.success, case.failure = [span], SpanExportResult.SUCCESS, SpanExportResult.FAILURE
            case.session = exporter._session
        else:
            reader = InMemoryMetricReader()
            provider = MeterProvider(metric_readers=[reader], shutdown_on_exit=False)
            provider.get_meter("test").create_counter("test").add(1)
            exporter = _export.metric_exporter("http://127.0.0.1:19119/v1/metrics", None, timeout_s, control, stats)
            case.data, case.success, case.failure = reader.get_metrics_data(), MetricExportResult.SUCCESS, MetricExportResult.FAILURE
            case.session = exporter._recording_session
        case.signal, case.provider, case.exporter = request.param, provider, exporter
        case.export = lambda: exporter.export(case.data)
        cases.append(case)
        return case

    yield build
    for case in cases:
        case.exporter.shutdown()
        case.provider.shutdown()


@pytest.mark.parametrize("shutdown", [False, True])
@pytest.mark.parametrize("failure", [requests.ConnectionError("reset"), requests.ReadTimeout("timeout"), 429, 502, 503, 504])
@pytest.mark.parametrize("failures", [1, 2, 3])
def test_bounded_retry_count_backoff_and_accounting(export_case, shutdown, failure, failures):
    case = export_case()
    case.control.begin(case.clock.now + 5, shutdown=shutdown)
    case.outcomes = [failure] * failures
    result = case.export()
    assert result is (case.success if failures < 3 else case.failure)
    assert len(case.calls) == min(failures + 1, 3)
    assert case.waits == pytest.approx([0.2, 0.4][:failures])
    assert [c["timeout"].total for c in case.calls] == pytest.approx([5, 4.8, 4.4][:len(case.calls)])
    assert all(c["data"] == case.calls[0]["data"] for c in case.calls)
    stats = case.stats.snapshot()
    if case.signal == "traces":
        if failures < 3:
            assert stats["ok"] == 1 and stats["network"] == 0
            assert stats["last_error"] is None
        else:
            bucket = "network" if isinstance(failure, Exception) else ("http_429" if failure == 429 else "http_5xx")
            assert stats[bucket] == 1 and stats["ok"] == 0
    else:
        assert stats.get("metrics_failed", 0) == (1 if failures == 3 else 0)


@pytest.mark.parametrize("failure", [400, 401, 403, 404, 408, 413, 500, 501,
                                    requests.exceptions.InvalidURL("bad URL"), requests.HTTPError("bad response")])
def test_non_retryable_errors_are_not_retried(export_case, failure):
    case = export_case()
    case.control.begin(case.clock.now + 5, shutdown=True)
    case.outcomes, case.headers = [failure], {"Retry-After": "60"}
    assert case.export() is case.failure
    assert len(case.calls) == 1 and case.waits == []


@pytest.mark.parametrize("budget,duration,calls,waits", [(0.1, 0, 1, []), (0.2, 0, 1, []),
                                                        (0.5, 0.35, 1, []), (0.61, 0.01, 2, [0.2])])
def test_insufficient_time_for_backoff_and_request(export_case, budget, duration, calls, waits):
    case = export_case()
    case.control.begin(case.clock.now + budget)
    case.request_duration, case.outcomes = duration, [503] * 3
    assert case.export() is case.failure
    assert len(case.calls) == calls and case.waits == pytest.approx(waits)


@pytest.mark.parametrize("export_timeout,budget", [(0.1, 5), (10, 0.8)])
def test_request_timeout_is_clamped_on_every_attempt(export_case, export_timeout, budget):
    case = export_case(export_timeout)
    case.control.begin(case.clock.now + budget)
    case.outcomes = [503, 503]
    assert case.export() is case.success
    for call in case.calls:
        expected = min(export_timeout, 100 + budget - call["at"])
        assert call["timeout"].total == pytest.approx(expected)
        assert 0 < call["timeout"].connect_timeout <= expected
        assert 0 < call["timeout"].read_timeout <= expected


@pytest.mark.parametrize("value,waits,success", [("60", [], False), ("5", [], False),
                                               ("0.3", [0.3], True), ("0.1", [0.2], True),
                                               ("bad", [0.2], True), ("nan", [0.2], True),
                                               ("inf", [0.2], True), ("-1", [0.2], True),
                                               ("date-long", [], False), ("date-short", [1], True)])
def test_retry_after_respects_remaining_budget(export_case, value, waits, success):
    case = export_case()
    case.control.begin(case.clock.now + 5, shutdown=True)
    if value.startswith("date-"):
        delay = 60 if value == "date-long" else 1
        value = format_datetime(datetime.fromtimestamp(case.clock.time() + delay, timezone.utc), usegmt=True)
    case.headers, case.outcomes = {"Retry-After": value}, [503]
    assert case.export() is (case.success if success else case.failure)
    assert len(case.calls) == (2 if success else 1)
    assert case.waits == pytest.approx(waits)


def test_retry_after_is_rechecked_after_each_failure(export_case):
    case = export_case()
    case.control.begin(case.clock.now + 5)
    case.outcomes = [429, 503]
    case.on_request = lambda: case.headers.update({"Retry-After": "0.3" if len(case.calls) == 1 else "0.5"})
    assert case.export() is case.success
    assert case.waits == pytest.approx([0.3, 0.5])


@pytest.mark.parametrize("failure", [503, requests.ConnectionError("reset")])
def test_running_request_switches_to_bounded_retries(export_case, failure):
    case = export_case()
    case.outcomes = [failure, failure]

    def close():
        if len(case.calls) == 1:
            case.control.begin(case.clock.now + 5, shutdown=True)

    case.on_request = close
    assert case.export() is case.success
    assert len(case.calls) == 3 and case.waits == pytest.approx([0.2, 0.4])
    assert isinstance(case.calls[0]["timeout"], float)
    assert case.calls[1]["timeout"].total == pytest.approx(4.8)


def test_expired_deadline_prevents_request(export_case):
    case = export_case()
    case.control.begin(case.clock.now - 1)
    assert case.export() is case.failure
    assert case.calls == [] and case.waits == []


def test_expired_deadline_after_backoff_prevents_request(export_case, monkeypatch):
    case = export_case()
    case.control.begin(case.clock.now + 1)
    case.outcomes = [503]

    def oversleep(delay):
        case.clock.now += 2

    monkeypatch.setattr(case.control._changed, "wait", oversleep)
    assert case.export() is case.failure
    assert len(case.calls) == 1


def test_flush_deadline_stays_with_inflight_export(export_case):
    case = export_case()
    token = case.control.begin(case.clock.now + 0.5)
    case.outcomes, case.request_duration = [503], 0.4
    case.on_request = lambda: case.control.end(token)
    assert case.export() is case.failure
    assert len(case.calls) == 1 and case.waits == []
    assert case.control.deadline() is None


def test_each_flush_batch_gets_its_own_retry_budget(export_case):
    case = export_case()
    first = case.control.begin(case.clock.now + 5)
    case.outcomes = [503, 503]
    assert case.export() is case.success
    case.control.end(first)
    case.control.begin(case.clock.now + 5)
    case.outcomes = [503]
    assert case.export() is case.success
    assert len(case.calls) == 5 and case.waits == pytest.approx([0.2, 0.4, 0.2])
    assert case.calls[-1]["timeout"].total == pytest.approx(4.8)


@pytest.mark.parametrize("failure", [503, requests.ConnectionError("reset")])
def test_unbounded_requests_leave_retries_to_upstream(export_case, failure):
    case = export_case()
    case.outcomes = [failure]
    case.session.begin()
    if isinstance(failure, Exception):
        with pytest.raises(type(failure)):
            case.session.request("POST", "http://127.0.0.1:19119/v1/traces", timeout=10)
    else:
        assert case.session.request("POST", "http://127.0.0.1:19119/v1/traces", timeout=10).status_code == failure
    assert len(case.calls) == 1 and case.waits == []
    assert case.calls[0]["timeout"] == 10


@pytest.mark.parametrize("operation", ["flush", "shutdown"])
def test_public_lifecycle_retries_both_signals(run_script, operation):
    result = run_script("""
        import json
        import requests
        from flashcat import genai
        from opentelemetry import metrics
        attempts, successes = {}, {}
        def send(self, *args, **kwargs):
            url = kwargs.get("url") or args[1]
            path = url.rsplit("/", 1)[-1]
            attempts[path] = attempts.get(path, 0) + 1
            response = requests.Response()
            response.status_code = 503 if attempts[path] < 3 else 200
            response._content = b""
            if response.status_code == 200:
                successes[path] = successes.get(path, 0) + 1
            assert 0 < kwargs["timeout"].total <= 2
            return response
        requests.Session.request = send
        genai.init(service_name="retry", endpoint="http://127.0.0.1:19119/v1/traces",
                  api_key="test:secret", instrumentations=[])
        with genai.trace("last"):
            pass
        metrics.get_meter("test").create_counter("last").add(1)
        genai.OPERATION(2)
        print("RESULT:" + json.dumps(dict(attempts=attempts, successes=successes,
                                         stats=genai.diagnostics()["export"])))
    """.replace("OPERATION", operation))
    assert result["attempts"] == {"traces": 3, "metrics": 3}
    assert result["successes"] == {"traces": 1, "metrics": 1}
    assert result["stats"]["ok"] == 1 and result["stats"]["network"] == 0


def test_shorter_deadline_interrupts_retry_wait(monkeypatch):
    from flashcat.genai._lifecycle import ExportControl

    control, waiting, results = ExportControl(), threading.Event(), []
    deadline = time.monotonic() + 2
    control.begin(deadline)
    original_wait = control._changed.wait

    def wait(delay):
        waiting.set()
        return original_wait(delay)

    monkeypatch.setattr(control._changed, "wait", wait)
    worker = threading.Thread(target=lambda: results.append(control.wait_for_retry(1, deadline)), daemon=True)
    worker.start()
    assert waiting.wait(1)
    control.begin(time.monotonic() + 0.01, shutdown=True)
    worker.join(0.5)
    assert not worker.is_alive() and results == [None]
