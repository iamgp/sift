# sift-sdk-python

Early Python SDK for emitting `SiftEvent v1` wide events.

## Goals

- Request/task-scoped context accumulation (`set(...)`)
- One event emitted at end of scope (`emit()` / context manager)
- JSONL output by default (works with `sift` immediately)
- Optional HTTP drain support (next step)

## Quick example

```python
from sift_sdk import Logger

logger = Logger(service="checkout-api", environment="development")

with logger.request(method="POST", path="/api/checkout") as req:
    req.set(user={"id": "u_123"})
    req.set(cart={"items": 3, "total_cents": 9999})
    req.info("request completed")
```

This emits a single JSON line matching `sift-spec/schemas/sift-event-v1.schema.json`.

## Sinks

### Stdout JSONL (default)

```python
from sift_sdk import Logger

logger = Logger(service="api")  # writes JSONL to stdout
```

### File JSONL

```python
from sift_sdk import Logger, FileJsonlSink

sink = FileJsonlSink("app.jsonl")
logger = Logger(service="api", sink=sink)
```

### HTTP batching (evlog-style drain pipeline spirit)

```python
from sift_sdk import Logger, HttpBatchSink

sink = HttpBatchSink(
    "http://127.0.0.1:9000/ingest",
    batch_size=50,
    flush_interval_ms=2000,
    max_buffer_size=2000,
    max_attempts=3,
)
logger = Logger(service="api", sink=sink)

# call on shutdown
logger.close()
```

`HttpBatchSink` batches events, retries failed sends, and drops oldest events when the in-memory buffer is full.

## ASGI middleware (FastAPI / Starlette style)

```python
from sift_sdk import Logger, HttpBatchSink, SiftASGIMiddleware

sink = HttpBatchSink("http://127.0.0.1:54687/api/ingest")
logger = Logger(service="api", sink=sink)

app = SiftASGIMiddleware(app, logger)
```

Inside handlers, if you can access ASGI `scope`, you can enrich the request-wide event:

```python
from sift_sdk import get_request_logger_from_scope

reqlog = get_request_logger_from_scope(scope)
if reqlog:
    reqlog.set(user={"id": "u_123"})
```

### Send directly to a running Sift viewer

Start Sift with ingest enabled:

```bash
cd sift
go run . -f ../.tmp/logs.jsonl -ingest
```

Then point the SDK at Sift:

```python
from sift_sdk import Logger, HttpBatchSink

sink = HttpBatchSink("http://127.0.0.1:54687/api/ingest", batch_size=10, flush_interval_ms=1000)
logger = Logger(service="worker", sink=sink)
```
