# Sift

Local log viewer for JSON/JSONL files with a browser UI.

## Usage

Run against a file (JSONL, JSON array, or single JSON object):

```bash
sift -f ./logs.jsonl
```

Tail a file for appended lines:

```bash
sift -f ./logs.jsonl -tail
```

Stream from stdin (live mode):

```bash
tail -f ./logs.jsonl | sift
```

Or explicitly read stdin:

```bash
tail -f ./logs.jsonl | sift -f -
```

Set retained event cap (server + UI memory cap):

```bash
sift -f ./logs.jsonl -cap 50000
```

## Notes

- Sift starts a local HTTP server on `127.0.0.1` and prints the URL to open.
- `-tail` follows appended lines in the target file (polling-based).
- Search and field filters are ANDed in the UI.
- Bookmarks, search/filter state, and column visibility are saved in `localStorage`.
