from __future__ import annotations

import socket
import sys
import time
import traceback
from copy import deepcopy
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any, Dict, IO, Optional

from .sinks import JsonlSink

SDK_VERSION = "0.1.0"


def _iso_now() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z")


def _deep_merge(dst: Dict[str, Any], src: Dict[str, Any]) -> Dict[str, Any]:
    for key, value in src.items():
        if key in dst and isinstance(dst[key], dict) and isinstance(value, dict):
            _deep_merge(dst[key], value)
        else:
            dst[key] = value
    return dst


def _normalize_level(level: str) -> str:
    v = (level or "info").lower()
    if v in {"warning"}:
        return "warn"
    return v if v in {"trace", "debug", "info", "warn", "error", "fatal"} else "info"


@dataclass
class Logger:
    service: str
    environment: str = "development"
    sink: Any = sys.stdout
    sdk_name: str = "sift-sdk-python"
    default_attributes: Dict[str, Any] = field(default_factory=dict)

    def __post_init__(self) -> None:
        if hasattr(self.sink, "emit"):
            return
        if hasattr(self.sink, "write"):
            self.sink = JsonlSink(self.sink)
            return
        raise TypeError("sink must provide emit(event) or be a text stream with write()")

    def request(self, *, method: str | None = None, path: str | None = None, route: str | None = None, request_id: str | None = None) -> "RequestLogger":
        request: Dict[str, Any] = {}
        if request_id:
            request["id"] = request_id
        if method:
            request["method"] = method
        if path:
            request["path"] = path
        if route:
            request["route"] = route
        return RequestLogger(parent=self, request=request)

    def emit_event(self, event: Dict[str, Any]) -> None:
        self.sink.emit(event)

    def flush(self) -> None:
        if hasattr(self.sink, "flush"):
            self.sink.flush()

    def close(self) -> None:
        if hasattr(self.sink, "close"):
            self.sink.close()


class RequestLogger:
    def __init__(self, parent: Logger, request: Optional[Dict[str, Any]] = None) -> None:
        self.parent = parent
        self.request: Dict[str, Any] = request or {}
        self.attributes: Dict[str, Any] = deepcopy(parent.default_attributes)
        self.level: str = "info"
        self.message: str = "request completed"
        self.status_code: Optional[int] = None
        self.outcome: str = "success"
        self.error_obj: Optional[Dict[str, Any]] = None
        self._started = time.perf_counter()
        self._emitted = False

    def __enter__(self) -> "RequestLogger":
        return self

    def __exit__(self, exc_type, exc, tb) -> bool:
        if exc is not None:
            self.error(exc)
            if self.error_obj and "stack" not in self.error_obj:
                self.error_obj["stack"] = "".join(traceback.format_exception(exc_type, exc, tb)).strip()
        self.emit()
        return False

    def set(self, **fields: Any) -> "RequestLogger":
        _deep_merge(self.attributes, fields)
        return self

    def set_request(self, **fields: Any) -> "RequestLogger":
        _deep_merge(self.request, fields)
        return self

    def status(self, status_code: int) -> "RequestLogger":
        self.status_code = int(status_code)
        if self.status_code >= 500:
            self.level = "error"
            self.outcome = "error"
        elif self.status_code >= 400 and self.level not in {"error", "fatal"}:
            self.level = "warn"
        return self

    def info(self, message: str = "request completed", **fields: Any) -> "RequestLogger":
        self.level = "info"
        self.message = message
        if fields:
            self.set(**fields)
        return self

    def warn(self, message: str, **fields: Any) -> "RequestLogger":
        self.level = "warn"
        self.message = message
        if fields:
            self.set(**fields)
        return self

    def error(self, error: Exception | str, **fields: Any) -> "RequestLogger":
        self.level = "error"
        self.outcome = "error"
        self.message = str(error) if isinstance(error, str) else (str(error) or error.__class__.__name__)
        if isinstance(error, Exception):
            self.error_obj = {
                "type": error.__class__.__name__,
                "message": str(error) or error.__class__.__name__,
            }
        else:
            self.error_obj = {"message": str(error)}
        if fields:
            _deep_merge(self.error_obj, fields)
        return self

    def emit(self) -> Dict[str, Any]:
        if self._emitted:
            return {}
        self._emitted = True

        duration_ms = (time.perf_counter() - self._started) * 1000.0
        event: Dict[str, Any] = {
            "schema_version": "sift.event/v1",
            "timestamp": _iso_now(),
            "level": _normalize_level(self.level),
            "service": self.parent.service,
            "environment": self.parent.environment,
            "message": self.message,
            "outcome": self.outcome,
            "duration_ms": round(duration_ms, 3),
            "request": self.request,
            "attributes": self.attributes,
            "sdk": {
                "name": self.parent.sdk_name,
                "version": SDK_VERSION,
                "language": "python",
                "runtime": f"cpython-{sys.version_info.major}.{sys.version_info.minor}",
            },
            "host": {
                "hostname": socket.gethostname(),
            },
        }
        if self.status_code is not None:
            event["status_code"] = self.status_code
        if self.error_obj:
            event["error"] = self.error_obj
        self.parent.emit_event(event)
        return event
