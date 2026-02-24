from __future__ import annotations

from typing import Any, Awaitable, Callable, Dict, Optional

from .logger import Logger, RequestLogger

ASGIApp = Callable[[Dict[str, Any], Callable[[], Awaitable[Dict[str, Any]]], Callable[[Dict[str, Any]], Awaitable[None]]], Awaitable[None]]


class SiftASGIMiddleware:
    def __init__(
        self,
        app: ASGIApp,
        logger: Logger,
        *,
        request_attr: str = "sift_logger",
        include_headers: bool = False,
    ) -> None:
        self.app = app
        self.logger = logger
        self.request_attr = request_attr
        self.include_headers = include_headers

    async def __call__(self, scope: Dict[str, Any], receive, send) -> None:
        if scope.get("type") != "http":
            await self.app(scope, receive, send)
            return

        req = self.logger.request(
            method=scope.get("method"),
            path=scope.get("path"),
            route=_route_from_scope(scope),
            request_id=_header(scope, b"x-request-id"),
        )
        if self.include_headers:
            req.set_request(headers=_safe_headers(scope))

        scope.setdefault("state", {})
        scope["state"][self.request_attr] = req
        status_code: Optional[int] = None

        async def wrapped_send(message: Dict[str, Any]) -> None:
            nonlocal status_code
            if message.get("type") == "http.response.start":
                try:
                    status_code = int(message.get("status", 0))
                    req.status(status_code)
                except Exception:
                    pass
            await send(message)

        try:
            await self.app(scope, receive, wrapped_send)
            if status_code is None:
                req.info("request completed")
        except Exception as exc:
            req.error(exc)
            raise
        finally:
            req.emit()


def get_request_logger_from_scope(scope: Dict[str, Any], key: str = "sift_logger") -> Optional[RequestLogger]:
    state = scope.get("state")
    if not isinstance(state, dict):
        return None
    val = state.get(key)
    return val if isinstance(val, RequestLogger) else None


def _header(scope: Dict[str, Any], name: bytes) -> Optional[str]:
    headers = scope.get("headers") or []
    for k, v in headers:
        if bytes(k).lower() == name:
            try:
                return bytes(v).decode("utf-8", errors="replace")
            except Exception:
                return str(v)
    return None


def _safe_headers(scope: Dict[str, Any]) -> Dict[str, str]:
    out: Dict[str, str] = {}
    for k, v in scope.get("headers") or []:
        kb = bytes(k).lower()
        if kb in {b"authorization", b"cookie", b"set-cookie"}:
            continue
        out[kb.decode("utf-8", errors="replace")] = bytes(v).decode("utf-8", errors="replace")
    return out


def _route_from_scope(scope: Dict[str, Any]) -> Optional[str]:
    route = scope.get("route")
    if route is None:
        return None
    path = getattr(route, "path", None)
    if isinstance(path, str):
        return path
    return None

