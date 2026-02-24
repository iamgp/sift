# sift-spec

Versioned schema and examples for `SiftEvent`.

## Design intent

- Language-neutral source of truth (JSON Schema first)
- Compatible with Sift viewer (generic JSON still supported)
- Follows the "wide event" spirit: one event per request/task with accumulated context

## Layout

- `schemas/sift-event-v1.schema.json`: canonical JSON Schema draft for `SiftEvent v1`
- `examples/request-success.json`: example success event
- `examples/request-error.json`: example error event

## Compatibility

The viewer in `../sift` should treat these as normal JSON logs today, and can add first-class rendering for this schema later.

