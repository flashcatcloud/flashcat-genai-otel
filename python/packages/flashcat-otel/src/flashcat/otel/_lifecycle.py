"""Enforce monotonic deadlines for flush and shutdown independently of OTel's internal timeouts."""

from __future__ import annotations

import threading
import time
from collections.abc import Callable


class ExportControl:
    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._changed = threading.Condition(self._lock)
        self._deadlines: dict[object, float] = {}
        self._shutdown_deadline: float | None = None

    def begin(self, deadline: float, shutdown: bool = False) -> object:
        token = object()
        with self._lock:
            self._deadlines[token] = deadline
            if shutdown:
                self._shutdown_deadline = deadline if self._shutdown_deadline is None else min(deadline, self._shutdown_deadline)
            self._changed.notify_all()
        return token

    def end(self, token: object) -> None:
        with self._lock:
            self._deadlines.pop(token, None)

    def deadline(self) -> float | None:
        with self._lock:
            return self._deadline()

    def _deadline(self) -> float | None:
        deadlines = list(self._deadlines.values())
        if self._shutdown_deadline is not None:
            deadlines.append(self._shutdown_deadline)
        return min(deadlines) if deadlines else None

    def wait_for_retry(self, delay: float, deadline: float) -> float | None:
        """Leave time for a request after backoff. An earlier deadline wakes the wait and can only shorten it."""
        retry_at = time.monotonic() + delay
        with self._changed:
            while True:
                current = self._deadline()
                if current is not None:
                    deadline = min(deadline, current)
                now = time.monotonic()
                wait = max(0, retry_at - now)
                if deadline - now <= wait:
                    return None
                if wait == 0:
                    return deadline
                self._changed.wait(wait)


def run_bounded(actions: list[Callable[[float], object]], deadline: float, control: ExportControl, token: object) -> bool:
    """Run force_flush on a daemon thread so the caller meets the overall deadline even if OTel ignores the timeout."""
    results: list[bool] = []

    def run(action: Callable[[float], object]) -> None:
        try:
            results.append(action(max(0, deadline - time.monotonic()) * 1000) is not False)
        except Exception:  # noqa: BLE001
            results.append(False)

    def finish() -> None:
        threads = [threading.Thread(target=run, args=(action,), daemon=True) for action in actions]
        try:
            for thread in threads:
                thread.start()
            for thread in threads:
                thread.join()
        finally:
            # Keep the deadline for exports still running after timeout to prevent late retries.
            control.end(token)

    worker = threading.Thread(target=finish, name="FlashcatLifecycle", daemon=True)
    worker.start()
    worker.join(max(0, deadline - time.monotonic()))
    return not worker.is_alive() and len(results) == len(actions) and all(results)
