# Changelog

## 0.1.0 - 2026-07-13

Initial Go implementation of Traceloom.

### Added

- Idiomatic thread-safe API with `Tracer`, `Trace`, functional configuration options, and context-aware event writes.
- PHP/Node.js-compatible JSONL event schema with monotonic elapsed time.
- Dependency-free kernel file locks on every supported platform: `flock` on Linux, macOS, and the BSDs, `LockFileEx` on Windows. A crashed writer cannot strand a lock, and no staleness heuristic can put two writers in one file.
- UTC date shards, size rotation, retention, restrictive POSIX permissions, flush, and close lifecycle.
- Reflection-based payload sanitization for maps, slices, structs, JSON tags, pointers, marshalers, stringers, and byte data.
- Recursive secret masking, UTF-8 byte limits, cycle/depth/item budgets, and degraded-record preservation.
- `maxPayloadNodes` bounds a payload as a whole, while `maxArrayItems` caps each array on its own, so one long array no longer starves the fields beside it.
- `maxKeyBytes` bounds a payload key. A truncated key carries a digest of the original, so two long keys cannot collapse into one and silently overwrite each other's value.
- Fail-safe and strict error policies with dropped-event metrics.
- Minimal hardened `eventtrace show` CLI.
- Unit, integration, CLI, goroutine, and multi-process concurrency tests.

### Performance

- The log directory is verified once instead of on every event, which was the most expensive step in the write path. Throughput on Windows went from 17,000 to 50,000 events/sec. The cached check is cleared on any write failure, so a directory removed at runtime is still recreated.

### Security

- `trustIncomingTraceID` defaults to `false`. A client-controlled trace ID is kept as `parent_trace_id` and a fresh `trace_id` is generated, so a caller cannot inject events into an existing timeline. Set it to `true` only behind a trusted gateway.
- The default sensitive-key list covers `authorization`, `cookie`, `session`, `bearer`, `signature`, `jwt`, and `accesskey` as fragments, so spellings such as `cookies`, `authorization_header`, `session_token`, `jwt_value`, and `access_key_id` are masked. `auth`, `access_key`, and `secret_key` are matched exactly, because `auth` and `key` as fragments would redact `author`, `keyboard`, and `monkey`.
- A payload key that spells a sanitizer marker (`_truncated`, `_binary`, `_encoding_error`, `_omitted_items`) is escaped one underscore deeper, so a payload can neither overwrite a marker nor forge one.
- Retention deletes only shards this writer produces (`<date>.jsonl`, `<date>-<index>.jsonl`). A file such as `2020-01-01-backup.jsonl` left in the log directory is no longer destroyed.
- The CLI escapes Unicode format characters, including U+202E RIGHT-TO-LEFT OVERRIDE, which can visually pass one event name off as another in a terminal, and bounds how much of one line it will read, so a corrupt file cannot exhaust memory.

### Notes

- `Close` is terminal: a write that arrives after it is rejected and counted as dropped instead of lazily reopening the log file.
- A failed write leaves a gap in `sequence` instead of renumbering, so a reader of the log can tell that an event is missing.
- A write that dies mid-record (a full disk) no longer corrupts the record after it: the shard is truncated back to the last complete line, or the fragment is closed with a newline where truncation is refused.
- Retention runs on the first write of each UTC date, so a long-lived process keeps expiring old shards.
- The JSONL schema and every marker name match the PHP and Node.js packages. Truncation follows Node.js: `maxArrayItems` caps each array and `maxPayloadNodes` bounds the payload, whereas PHP spends one shared `maxArrayItems` budget across the whole payload.
