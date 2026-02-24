# Sift

Local log viewer for JSON/JSONL files with a browser UI.

## Usage

From the repo root:

```bash
cd sift
```

Run against a file (JSONL, JSON array, or single JSON object):

```bash
go run . -f ./logs.jsonl
```

Tail a file for appended lines:

```bash
go run . -f ./logs.jsonl -tail
```

Enable HTTP ingest (append JSON objects/arrays while the viewer is running):

```bash
go run . -f ./logs.jsonl -ingest
```

Then `POST` to `http://127.0.0.1:<port>/api/ingest` with a JSON object or JSON array.

Stream from stdin (live mode):

```bash
tail -f ./logs.jsonl | go run .
```

Or explicitly read stdin:

```bash
tail -f ./logs.jsonl | go run . -f -
```

Set retained event cap (server + UI memory cap):

```bash
go run . -f ./logs.jsonl -cap 500000
```

## Notes

- Sift starts a local HTTP server on `127.0.0.1` and prints the URL to open.
- `-tail` follows appended lines in the target file (polling-based).
- `-ingest` enables `POST /api/ingest` for appending JSON object/array events into the live viewer.
- Search and field filters are ANDed in the UI.
- Bookmarks, search/filter state, and column visibility are saved in `localStorage`.
