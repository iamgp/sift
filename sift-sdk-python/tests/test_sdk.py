import asyncio
import json
import sys
import unittest
from io import StringIO

sys.path.insert(0, "src")

from sift_sdk import Logger, SiftASGIMiddleware, get_request_logger_from_scope  # noqa: E402


class TestSiftSDK(unittest.TestCase):
    def test_request_logger_emits_wide_event(self):
        buf = StringIO()
        logger = Logger(service="demo", sink=buf)
        with logger.request(method="GET", path="/health") as req:
            req.set(app={"version": "1.0"})
            req.status(200).info("ok")
        event = json.loads(buf.getvalue().strip())
        self.assertEqual(event["schema_version"], "sift.event/v1")
        self.assertEqual(event["service"], "demo")
        self.assertEqual(event["request"]["path"], "/health")
        self.assertEqual(event["status_code"], 200)
        self.assertEqual(event["attributes"]["app"]["version"], "1.0")

    def test_asgi_middleware_emits_status_and_allows_enrichment(self):
        buf = StringIO()
        logger = Logger(service="demo", sink=buf)

        async def app(scope, receive, send):
            req = get_request_logger_from_scope(scope)
            assert req is not None
            req.set(user={"id": "u1"})
            await send({"type": "http.response.start", "status": 201, "headers": []})
            await send({"type": "http.response.body", "body": b"ok", "more_body": False})

        wrapped = SiftASGIMiddleware(app, logger)

        async def receive():
            return {"type": "http.request", "body": b"", "more_body": False}

        sent = []

        async def send(message):
            sent.append(message)

        scope = {
            "type": "http",
            "method": "POST",
            "path": "/jobs",
            "headers": [(b"x-request-id", b"req_123")],
            "state": {},
        }
        asyncio.run(wrapped(scope, receive, send))
        self.assertEqual(sent[0]["status"], 201)
        event = json.loads(buf.getvalue().strip())
        self.assertEqual(event["request"]["method"], "POST")
        self.assertEqual(event["request"]["path"], "/jobs")
        self.assertEqual(event["request"]["id"], "req_123")
        self.assertEqual(event["status_code"], 201)
        self.assertEqual(event["attributes"]["user"]["id"], "u1")


if __name__ == "__main__":
    unittest.main()

