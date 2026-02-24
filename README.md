# Sift Monorepo

This repository now contains multiple related projects:

- `sift/`: local JSON/JSONL log viewer (Go backend + embedded TanStack frontend)
- `sift-spec/`: versioned event schema and examples for `SiftEvent`
- `sift-sdk-python/`: early Python SDK implementing the "wide event" / request-scoped logging model

## Goal

Keep Sift as a strong local viewer for arbitrary JSON logs, while adding a reusable event spec and SDKs (Python first) inspired by the `evlog` approach:

- one structured event per request/task
- accumulated context
- transport/drain abstraction
- machine-friendly JSON

## Current Status

- Viewer is implemented in `sift/`
- `SiftEvent v1` schema draft is in `sift-spec/`
- Python SDK scaffold is in `sift-sdk-python/`

