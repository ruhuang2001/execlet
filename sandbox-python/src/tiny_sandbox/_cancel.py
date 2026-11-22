"""Explicit, thread-safe caller cancellation, separate from server deadlines."""

from __future__ import annotations

import math
import threading
import time
from collections.abc import Callable


class _Cancellation(Exception):
    def __init__(self, *, dispatched: bool = False):
        super().__init__("caller canceled the operation")
        self.dispatched = dispatched


class CancellationToken:
    """Cancel from another thread, or after a monotonic timeout in seconds.

    Tokens are reusable for related operations but cannot be reset. A local
    deadline requests cancellation; it never creates a server timeout outcome.
    """

    def __init__(self, *, timeout: float | None = None):
        if timeout is not None and (not math.isfinite(timeout) or timeout < 0):
            raise ValueError("timeout must be finite and nonnegative")
        self._event = threading.Event()
        self._deadline = None if timeout is None else time.monotonic() + timeout
        self._lock = threading.Lock()
        self._callbacks: list[Callable[[], None]] = []

    def cancel(self) -> None:
        self._event.set()
        # Only internal socket-shutdown callbacks register here. Run them
        # under the lock so unregister is a barrier before recycling a socket.
        with self._lock:
            while self._callbacks:
                self._callbacks.pop()()

    def _register(self, callback: Callable[[], None]) -> Callable[[], None]:
        with self._lock:
            if not self.cancelled:
                self._callbacks.append(callback)

                def unregister() -> None:
                    with self._lock:
                        try:
                            self._callbacks.remove(callback)
                        except ValueError:
                            pass

                return unregister
        try:
            callback()
        except Exception:
            pass
        return lambda: None

    @property
    def cancelled(self) -> bool:
        remaining = self._remaining()
        return self._event.is_set() or (remaining is not None and remaining <= 0)

    def _remaining(self) -> float | None:
        return None if self._deadline is None else self._deadline - time.monotonic()

    def _check(self) -> None:
        if self.cancelled:
            raise _Cancellation()

    def _wait(self, seconds: float) -> None:
        self._check()
        remaining = self._remaining()
        self._event.wait(seconds if remaining is None else min(seconds, max(0, remaining)))
        self._check()
