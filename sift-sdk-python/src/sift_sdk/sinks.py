from __future__ import annotations

import json
import threading
import time
from typing import Any, Dict, IO, List, Optional
from urllib import error as urlerror
from urllib import request as urlrequest


class JsonlSink:
    def __init__(self, stream: IO[str]):
        self.stream = stream
        self._lock = threading.Lock()

    def emit(self, event: Dict[str, Any]) -> None:
        line = json.dumps(event, separators=(",", ":"), ensure_ascii=False)
        with self._lock:
            self.stream.write(line)
            self.stream.write("\n")
            self.stream.flush()

    def flush(self) -> None:
        with self._lock:
            self.stream.flush()

    def close(self) -> None:
        self.flush()


class FileJsonlSink(JsonlSink):
    def __init__(self, path: str, encoding: str = "utf-8"):
        self._fh = open(path, "a", encoding=encoding)
        super().__init__(self._fh)

    def close(self) -> None:
        with self._lock:
            self.stream.flush()
            self.stream.close()


class HttpBatchSink:
    def __init__(
        self,
        endpoint: str,
        *,
        headers: Optional[Dict[str, str]] = None,
        batch_size: int = 50,
        flush_interval_ms: int = 5000,
        max_buffer_size: int = 1000,
        max_attempts: int = 3,
        initial_delay_ms: int = 250,
        timeout_s: float = 5.0,
        on_dropped=None,
    ):
        if batch_size <= 0:
            raise ValueError("batch_size must be > 0")
        if flush_interval_ms <= 0:
            raise ValueError("flush_interval_ms must be > 0")
        if max_buffer_size <= 0:
            raise ValueError("max_buffer_size must be > 0")
        if max_attempts <= 0:
            raise ValueError("max_attempts must be > 0")
        self.endpoint = endpoint
        self.headers = {"Content-Type": "application/json", **(headers or {})}
        self.batch_size = batch_size
        self.flush_interval_ms = flush_interval_ms
        self.max_buffer_size = max_buffer_size
        self.max_attempts = max_attempts
        self.initial_delay_ms = initial_delay_ms
        self.timeout_s = timeout_s
        self.on_dropped = on_dropped

        self._lock = threading.Lock()
        self._buffer: List[Dict[str, Any]] = []
        self._closed = False
        self._stop = threading.Event()
        self._wake = threading.Event()
        self._thread = threading.Thread(target=self._run, name="sift-http-batch-sink", daemon=True)
        self._thread.start()

    @property
    def pending(self) -> int:
        with self._lock:
            return len(self._buffer)

    def emit(self, event: Dict[str, Any]) -> None:
        with self._lock:
            if self._closed:
                raise RuntimeError("sink is closed")
            if len(self._buffer) >= self.max_buffer_size:
                dropped = [self._buffer.pop(0)]
                if self.on_dropped:
                    self.on_dropped(dropped, None)
            self._buffer.append(event)
            should_wake = len(self._buffer) >= self.batch_size
        if should_wake:
            self._wake.set()

    def flush(self) -> None:
        while True:
            batch = self._pop_batch()
            if not batch:
                return
            self._send_with_retry(batch)

    def close(self) -> None:
        with self._lock:
            if self._closed:
                return
            self._closed = True
        self._stop.set()
        self._wake.set()
        self._thread.join(timeout=max(1.0, self.timeout_s * 2))
        self.flush()

    def _run(self) -> None:
        interval_s = self.flush_interval_ms / 1000.0
        while not self._stop.is_set():
            self._wake.wait(interval_s)
            self._wake.clear()
            try:
                self.flush()
            except Exception:
                # Never crash the background thread.
                pass

    def _pop_batch(self) -> List[Dict[str, Any]]:
        with self._lock:
            if not self._buffer:
                return []
            batch = self._buffer[: self.batch_size]
            del self._buffer[: len(batch)]
            return batch

    def _send_with_retry(self, batch: List[Dict[str, Any]]) -> None:
        last_error: Optional[Exception] = None
        for attempt in range(1, self.max_attempts + 1):
            try:
                self._post_batch(batch)
                return
            except Exception as exc:  # noqa: BLE001
                last_error = exc
                if attempt < self.max_attempts:
                    delay = min((self.initial_delay_ms / 1000.0) * (2 ** (attempt - 1)), 30.0)
                    time.sleep(delay)
        if self.on_dropped:
            self.on_dropped(batch, last_error)

    def _post_batch(self, batch: List[Dict[str, Any]]) -> None:
        body = json.dumps(batch, separators=(",", ":"), ensure_ascii=False).encode("utf-8")
        req = urlrequest.Request(self.endpoint, data=body, headers=self.headers, method="POST")
        try:
            with urlrequest.urlopen(req, timeout=self.timeout_s) as res:
                status = getattr(res, "status", 200)
                if status >= 400:
                    raise RuntimeError(f"HTTP {status}")
        except urlerror.HTTPError as exc:
            raise RuntimeError(f"HTTP {exc.code}") from exc
        except urlerror.URLError as exc:
            raise RuntimeError(f"URL error: {exc.reason}") from exc

