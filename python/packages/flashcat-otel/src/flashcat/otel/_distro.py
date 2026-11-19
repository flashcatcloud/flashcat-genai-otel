"""Zero-code entry point. Set OTEL_PYTHON_DISTRO=flashcat with opentelemetry-instrument.

The SDK loads instrumentation from its catalog and records diagnostics. OTel contrib handles the other entry points.
"""

from __future__ import annotations

from opentelemetry.instrumentation.distro import BaseDistro
from opentelemetry.sdk._configuration import _BaseConfigurator


class FlashcatConfigurator(_BaseConfigurator):
    def _configure(self, **kwargs):  # noqa: D401
        from flashcat import otel

        otel.init()


class FlashcatDistro(BaseDistro):
    def _configure(self, **kwargs):
        pass

    def load_instrumentor(self, entry_point, **kwargs):
        from flashcat import otel
        from ._catalog import CATALOG

        # The SDK already loaded catalog entries. Loading them again through contrib would duplicate instrumentation and bypass conflict and disable checks.
        if entry_point.value in {entry.instrumentor for entry in CATALOG}:
            return
        diag = otel._state.get("diag")
        if diag is None or otel._state["settings"].disabled:
            return
        item = {"lib": entry_point.name, "source": "upstream"}
        diag.setdefault("instrumentations", []).append(item)
        try:
            inst = entry_point.load()()
            already = inst.is_instrumented_by_opentelemetry
            inst.instrument(**kwargs)
            item["status"] = "already_instrumented" if already else (
                "installed" if inst.is_instrumented_by_opentelemetry else "failed")
        except Exception as e:  # noqa: BLE001
            item.update(status="failed", error=f"{type(e).__name__}: {e}")
