"""Load instrumentation and detect conflicts."""

from __future__ import annotations

import importlib
import importlib.util
import logging
import threading
from importlib import metadata
from typing import Any

from ._catalog import ALIASES, CATALOG, Entry

logger = logging.getLogger("flashcat.genai")


def foreign_wrappers(owner: type, attr: str, prefixes: tuple[str, ...]) -> list[str]:
    """Follow the __wrapped__ chain to find third-party wrappers.

    Read from __dict__ to avoid rebinding proxies through getattr.
    An ObjectProxy type check would miss FunctionWrapper in wrapt 2.x.
    """
    fn = owner.__dict__.get(attr)
    seen: list[str] = []
    depth = 0
    while fn is not None and depth < 16:
        depth += 1
        wrapper = getattr(fn, "_self_wrapper", None)
        mod = getattr(wrapper, "__module__", "") or ""
        if not mod:
            mod = getattr(fn, "__module__", "") or ""
        if mod.startswith(prefixes):
            seen.append(mod)
        if not hasattr(fn, "__wrapped__"):
            break
        fn = fn.__wrapped__
    return seen


def _detect_conflicts(entry: Entry) -> list[str]:
    found: list[str] = []
    for t in entry.patch_targets:
        try:
            mod = importlib.import_module(t.module)
            owner = getattr(mod, t.owner)
        except Exception:
            continue
        found.extend(foreign_wrappers(owner, t.attr, entry.foreign_prefixes))
    return sorted(set(found))


def _version(dist: str) -> str | None:
    try:
        return metadata.version(dist)
    except metadata.PackageNotFoundError:
        return None


def is_native(entry: Entry, version: str | None) -> bool:
    """Check whether this version of the target library has built-in OTel instrumentation (Entry.native_since)."""
    if not entry.native_since or not version:
        return False
    try:
        from packaging.version import Version

        return Version(version) >= Version(entry.native_since)
    except Exception:  # noqa: BLE001
        return False


def _disabled(entry: Entry, disabled: set[str]) -> bool:
    return "*" in disabled or entry.lib in disabled or f"{entry.source}:{entry.lib}" in disabled or entry.lib.replace("-", "_") in disabled


def load(
    tracer_provider: Any,
    meter_provider: Any,
    only: list[str] | None,
    disabled: list[str],
    prefer: str = "foreign",
) -> list[dict]:
    """Load instrumentation from the catalog and return diagnostics for each entry. Record exceptions in diagnostics and let the application continue."""
    want = {ALIASES.get(x, x) for x in only} if only is not None else None
    off = {ALIASES.get(x, x) for x in disabled}
    results: list[dict] = []
    for entry in CATALOG:
        if want is not None and entry.lib not in want:
            continue
        item: dict = {"lib": entry.lib, "source": entry.source}
        results.append(item)
        try:
            present = importlib.util.find_spec(entry.target_module) is not None
        except (ImportError, ValueError):
            present = False
        if not present:
            item["status"] = "not_present"
            continue
        item["lib_version"] = _version(entry.dist)
        if _disabled(entry, off):
            item["status"] = "disabled"
            continue
        if is_native(entry, item["lib_version"]):
            item["source"] = "native"
            item["status"] = "installed"
            continue
        foreign = _detect_conflicts(entry)
        if foreign and prefer != "flashcat":
            item["status"] = "skipped_conflict"
            item["foreign"] = foreign
            logger.warning("flashcat-genai-otel: %s 已被其他插桩包装（%s），跳过 Flashcat 这一层以免重复", entry.lib, ", ".join(foreign))
            continue
        mod_name, cls_name = entry.instrumentor.split(":")
        try:
            cls = getattr(importlib.import_module(mod_name), cls_name)
        except ImportError:
            item["status"] = "instrumentation_not_installed"
            item["hint"] = f"pip install 'flashcat-genai-otel[{entry.lib}]'"
            continue
        except Exception as e:  # noqa: BLE001
            item["status"] = "failed"
            item["error"] = f"{type(e).__name__}: {e}"
            continue
        try:
            inst = cls()
            if inst.is_instrumented_by_opentelemetry:
                item["status"] = "already_instrumented"
                continue
            # instrument() also returns None on conflicts, so check versions first using the same path as contrib auto-loading.
            conflict = inst._check_dependency_conflicts()
            if conflict is not None:
                item["status"] = "skipped_version"
                item["supported"] = str(conflict.required) if getattr(conflict, "required", None) else str(conflict)
                continue
            inst.instrument(tracer_provider=tracer_provider, meter_provider=meter_provider, skip_dep_check=True)
            item["status"] = "installed"
        except Exception as e:  # noqa: BLE001
            item["status"] = "failed"
            item["error"] = f"{type(e).__name__}: {e}"
    return results


def schedule_recheck(results: list[dict], delay_s: float = 30.0) -> None:
    """Recheck once after a delay for third-party patches applied after init. Record any conflicts in diagnostics only."""

    def _recheck() -> None:
        by_lib = {e.lib: e for e in CATALOG}
        for item in results:
            if item.get("status") != "installed":
                continue
            entry = by_lib.get(item["lib"])
            if entry is None:
                continue
            foreign = _detect_conflicts(entry)
            if foreign:
                item["late_conflict"] = foreign
                logger.warning("flashcat-genai-otel: init 之后 %s 又被其他插桩包装（%s），可能产生重复 span", entry.lib, ", ".join(foreign))

    t = threading.Timer(delay_s, _recheck)
    t.daemon = True
    t.start()
