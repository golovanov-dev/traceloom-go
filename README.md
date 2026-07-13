# Traceloom for Go

A lightweight, dependency-free Go library for recording structured event timelines grouped by trace ID.

Traceloom is intended for small APIs, backend services, jobs, webhooks, and command-line programs where you want to reconstruct one logical process without running a full observability stack. Its JSONL wire format is compatible with the PHP and Node.js Traceloom packages.

It is not a replacement for a general-purpose logging library, OpenTelemetry, or a distributed tracing platform.

## Requirements

- Go 1.22 or newer
- A Unix-like system or Windows; interprocess coordination uses native kernel file locks on both

The module has no external dependencies.

## Installation

```bash
go get github.com/golovanov-dev/traceloom-go
```

## Quick start

```go
package main

import (
    "log"

    traceloom "github.com/golovanov-dev/traceloom-go"
)

func main() {
    tracer, err := traceloom.New("./logs")
    if err != nil {
        log.Fatal(err)
    }
    defer tracer.Close()

    trace, err := tracer.Start("")
    if err != nil {
        log.Fatal(err)
    }

    if err := trace.Event("request_start", traceloom.Data{
        "method": "POST",
        "path":   "/orders",
    }); err != nil {
        log.Fatal(err)
    }

    _ = trace.Event("auth_success", traceloom.Data{"user_id": 42})
    _ = trace.Event("request_end", traceloom.Data{"status": 201})
}
```

Every event written through the same `Trace` shares one `trace_id`. A trace is safe to call from multiple goroutines; sequence values advance in the order writes acquire the trace lock.

## JSONL output

```json
{"timestamp":"2026-07-10T10:41:20.112345Z","trace_id":"9f1a8e7c2d4b4a9f93e2b2b1454f0c0a","event":"request_start","sequence":1,"elapsed_ms":0.121,"data":{"method":"POST","path":"/orders"}}
{"timestamp":"2026-07-10T10:41:20.117381Z","trace_id":"9f1a8e7c2d4b4a9f93e2b2b1454f0c0a","event":"auth_success","sequence":2,"elapsed_ms":5.157,"data":{"user_id":42}}
```

Timestamps are UTC with six fractional digits. `elapsed_ms` uses Go's monotonic clock reading, retained separately from wall-clock time, so NTP and daylight-saving changes do not distort durations.

## Continue an existing trace

```go
trace, err := tracer.Start(incomingTraceID)
```

Valid incoming IDs contain 8–128 characters from `A-Z`, `a-z`, `0-9`, `.`, `_`, `-`, and `:`. Invalid or empty IDs are replaced with a cryptographically random 32-character lowercase hex ID.

## Configuration

Configuration uses functional options and becomes immutable after validation:

```go
tracer, err := traceloom.New(
    "./logs",
    traceloom.WithMaxFileBytes(50*1024*1024),
    traceloom.WithMaxStringBytes(64*1024),
    traceloom.WithMaxKeyBytes(256),
    traceloom.WithMaxRecordBytes(256*1024),
    traceloom.WithMaxArrayItems(1000),
    traceloom.WithMaxPayloadNodes(10000),
    traceloom.WithMaxDepth(16),
    traceloom.WithSensitiveKeys("payment_token"),
    traceloom.WithStrictSensitiveKeys(false),
    traceloom.WithDirectoryMode(0o750),
    traceloom.WithFileMode(0o640),
    traceloom.WithRetentionDays(0),
    traceloom.WithTrustIncomingTraceID(false),
    traceloom.WithFailOnError(false),
    traceloom.WithOnError(func(err error) {
        log.Printf("tracing: %v", err)
    }),
)
```

For dependency injection:

```go
configuration, err := traceloom.NewConfiguration("./logs")
tracer, err := traceloom.NewWithDependencies(configuration, writer, clock)
```

`Writer`, `Clock`, `TraceEvent`, `Configuration`, and the built-in `JSONLFileWriter` are public contracts.

`maxRecordBytes` is clamped to `maxFileBytes`, and `maxStringBytes` is clamped to `maxRecordBytes`.

## Context and concurrency

`Event` is synchronous, as is conventional for a small Go I/O API. Applications already have goroutines when they need concurrent work:

```go
go func() {
    _ = trace.Event("background_step", traceloom.Data{"step": 1})
}()
```

Use `EventContext` when lock acquisition should honor request cancellation or a deadline:

```go
err := trace.EventContext(ctx, "database_complete", traceloom.Data{
    "rows": 10,
})
```

`FlushContext` is available on both `Trace` and `Tracer`. Without a caller deadline, filesystem lock waits are bounded internally.

## Files, rotation, and interprocess locking

Files are selected by UTC date and rotated into numbered shards:

```text
logs/
  2026-07-10.jsonl
  2026-07-10-1.jsonl
  2026-07-10-2.jsonl
```

Traceloom uses kernel file locks on every supported platform, with no dependencies: `syscall.Flock` on Linux, macOS, and the BSDs, and `LockFileEx` (resolved from `kernel32`) on Windows.

- `.traceloom.lock` coordinates shard selection and rotation;
- the active JSONL file is locked for one complete append;
- the kernel releases a lock when the process holding it dies, so a crashed writer cannot strand one;
- PHP, Node.js, and Go writers can append to the same directory.

Supported platforms are Unix-like systems and Windows. There is no lock-file fallback: a homemade lock has to guess when an owner has died, and guessing wrong puts two writers in the same file.

`WithRetentionDays(n)` expires shards older than `n` days. Retention runs on the first write of each UTC date, so a long-lived process keeps expiring old shards rather than sweeping once at startup.

Directories are created as `0750` and files as `0640` on POSIX systems. Keep trace files outside the web root: payloads can still contain sensitive information after masking.

## Sensitive data

Masking is recursive and enabled by default. Keys are folded by case and punctuation, so `api_key`, `apiKey`, `API-KEY`, and `X-Api-Key` are recognized. Secret fragments are also matched, so spellings such as `cookies`, `authorization_header`, and `session_token` are covered:

`password`, `passwd`, `secret`, `token`, `apikey`, `privatekey`, `credential`, `authorization`, `cookie`, `session`, `bearer`, `signature`, `jwt`, `accesskey`.

A fragment is a substring that appears in no ordinary English word, so matching it cannot redact an innocent key. That rules out `auth` and `key`, which would swallow `author`, `keyboard`, and `monkey`; those spellings are covered by the exact key list instead, which includes `auth`, `access_key`, and `secret_key`.

Masked values become:

```json
"[REDACTED]"
```

Set `WithStrictSensitiveKeys(true)` to disable fragment matching. Custom keys are merged with the defaults.

Masking uses names, including struct JSON field names, not value inspection. Two consequences are worth knowing:

- A credential stored under an innocent key such as `note` is not detected, and neither is a secret inside a value: a JWT in a message, a `?token=` in a URL, or a password in a DSN.
- Key folding is ASCII. A key spelled with lookalike Unicode letters (`pаssword` with a Cyrillic `а`) does not match.

## Payload support

The event root is `traceloom.Data`, an alias-friendly `map[string]any`. Nested values can include:

- maps with string keys;
- slices and arrays;
- structs and exported fields with JSON tags;
- pointers;
- strings, integers, unsigned integers, floats, booleans, and nil;
- `json.Marshaler`, `encoding.TextMarshaler`, and `fmt.Stringer`;
- `time.Time`;
- byte slices and byte arrays.

Reflection traversal is cycle-aware and bounded by two independent limits: `maxArrayItems` caps each array on its own, so one long array does not starve the fields beside it, while `maxPayloadNodes` bounds the payload as a whole and stops a wide or deeply nested value from costing unbounded work.

Keys are bounded too. A key longer than `maxKeyBytes` is truncated and given a digest of the original, so `verylongkey…~1f0a2b3c4d5e6f70` stays unique: two long keys cannot collapse into one and silently overwrite each other's value. Without this, a single oversized key is enough to push a record past `maxRecordBytes` and degrade the whole event.

| Marker | Cause |
| --- | --- |
| `{ "_truncated": true, ... }` | UTF-8 string exceeds `maxStringBytes` |
| `{ "_binary": true, ... }` | Byte data or an invalid UTF-8 string |
| `[CIRCULAR_REFERENCE]` | A map, slice, or pointer refers back to itself |
| `[MAX_DEPTH_EXCEEDED]` | Nesting is deeper than `maxDepth` |
| `[SERIALIZATION_FAILED: Type]` | A marshaler or stringer errors or panics |
| `[UNSUPPORTED_TYPE: type]` | Function, channel, complex number, or other unsupported value |
| `{ "_omitted_items": N }` | Entries exceed `maxArrayItems` or `maxPayloadNodes` |

A payload key that spells one of these markers is escaped one underscore deeper: a payload key `_truncated` is recorded as `__truncated`. A record can therefore neither forge a marker nor overwrite one.

Non-finite floats are stored as strings. If a complete record cannot be encoded or exceeds `maxRecordBytes`, its data becomes `{ "_encoding_error": "..." }`, preserving the event in the timeline.

A `String()` or `MarshalJSON()` that panics is contained and reported as `[SERIALIZATION_FAILED: Type]`. One that never returns — an infinite loop, or a `String()` that recurses into itself — cannot be contained, so payload types are expected to terminate.

## Trusting incoming trace IDs

An incoming ID is **not** trusted by default. A caller cannot inject events into an existing timeline: Traceloom generates a fresh `trace_id` and keeps the accepted incoming value as `parent_trace_id`.

```go
trace, err := tracer.Start(request.Header.Get("X-Trace-Id"))
```

Behind a trusted gateway or a service mesh, where the inbound ID is not client-controlled, continue the incoming trace instead:

```go
tracer, err := traceloom.New(
    "./logs",
    traceloom.WithTrustIncomingTraceID(true),
)
```

## HTTP integration

```go
func handler(response http.ResponseWriter, request *http.Request) {
    trace, err := tracer.Start(request.Header.Get("X-Trace-Id"))
    if err != nil {
        http.Error(response, "trace unavailable", http.StatusInternalServerError)
        return
    }

    _ = trace.EventContext(request.Context(), "request_start", traceloom.Data{
        "method": request.Method,
        "path":   request.URL.Path,
    })

    response.Header().Set("X-Trace-Id", trace.ID())
    response.WriteHeader(http.StatusOK)

    _ = trace.EventContext(request.Context(), "request_end", traceloom.Data{
        "status": http.StatusOK,
    })
}
```

Framework-specific middleware is outside the core module.

## Error handling

Runtime tracing failures are fail-safe by default. They increment `DroppedEventCount()` and optionally call `OnError`, but `Event` returns nil so tracing does not break the host application.

A dropped event still consumes its `sequence` number, so a failed write leaves a gap in the log rather than being renumbered away. A reader of a timeline that jumps from `sequence` 4 to 6 can see that an event is missing.

Configuration errors, trace-ID generation failures, and invalid event names always return errors. Set `WithFailOnError(true)` in tests or strict development environments to return I/O and payload-write failures as well.

```go
if tracer.DroppedEventCount() > 0 {
    // The timeline has lost events.
}
```

`OnError` panics are recovered so an observability callback cannot crash the application.

## Flush and close

`Flush` calls `fsync` on the active shard. Ordinary event writes use the operating system's file buffers without an `fsync` per line.

`Close` releases the active handle and is terminal: an event written after it is rejected and counted by `DroppedEventCount()` rather than reopening the shard. It is safe to call more than once.

## CLI

Install the command:

```bash
go install github.com/golovanov-dev/traceloom-go/cmd/eventtrace@latest
```

Show a timeline:

```bash
eventtrace show 9f1a8e7c2d4b4a9f93e2b2b1454f0c0a --dir=logs
```

Example:

```text
Trace: 9f1a8e7c2d4b4a9f93e2b2b1454f0c0a
10:41:20.112 request_start
10:41:20.117 auth_success +5.036 ms
Total duration: 5.157 ms
```

During development:

```bash
go run ./cmd/eventtrace show <trace-id> --dir=logs
```

The CLI streams arbitrarily long JSONL records, escapes terminal control bytes, and aggregates malformed-line warnings.

## Development

```bash
go test ./...
go test -race ./...
go vet ./...
go build ./cmd/eventtrace
go run ./benchmarks/event 1000
```

Tests cover payload normalization, goroutine safety, multi-process append and rotation, CLI hardening, retention, strict/fail-safe behavior, and distribution-compatible examples.

## When not to use Traceloom

Use a general-purpose logger when you need levels, handlers, and broad logging integrations. Use OpenTelemetry for standard distributed tracing. Use an observability platform for aggregation, dashboards, alerting, or cross-service querying.

Traceloom intentionally remains small: local structured timeline tracing without external infrastructure.
