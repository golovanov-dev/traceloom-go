package traceloom

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWritesCompatibleStructuredJSONL(t *testing.T) {
	directory := t.TempDir()
	tracer, err := New(directory, WithTrustIncomingTraceID(true))
	if err != nil {
		t.Fatal(err)
	}
	trace, err := tracer.Start("request-trace-123")
	if err != nil {
		t.Fatal(err)
	}
	if err := trace.Event("request_start", Data{"method": "POST", "path": "/orders", "token": "secret"}); err != nil {
		t.Fatal(err)
	}
	if err := trace.Event("request_end", Data{"status": 201, "text": "Привет"}); err != nil {
		t.Fatal(err)
	}
	if err := tracer.Close(); err != nil {
		t.Fatal(err)
	}

	events := readTestEvents(t, directory)
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].TraceID != "request-trace-123" || events[1].TraceID != "request-trace-123" {
		t.Fatalf("trace IDs differ: %#v", events)
	}
	if events[0].Sequence != 1 || events[1].Sequence != 2 {
		t.Fatalf("sequence mismatch: %#v", events)
	}
	if _, err := time.Parse("2006-01-02T15:04:05.000000Z", events[0].Timestamp); err != nil {
		t.Fatalf("timestamp is incompatible: %q: %v", events[0].Timestamp, err)
	}
	if events[0].Data["token"] != Redacted || events[1].Data["text"] != "Привет" {
		t.Fatalf("payload mismatch: %#v %#v", events[0].Data, events[1].Data)
	}
}

// An inbound ID is quarantined by default, so a caller cannot inject events into an
// existing timeline. Trusting it is opt-in and belongs behind a trusted gateway.
func TestIncomingTraceIDIsUntrustedByDefault(t *testing.T) {
	tracer, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	trace, err := tracer.Start("attacker-controlled")
	if err != nil {
		t.Fatal(err)
	}
	if trace.ID() == "attacker-controlled" {
		t.Fatal("an inbound trace ID must not become the trace ID by default")
	}
	if trace.ParentID() != "attacker-controlled" {
		t.Fatalf("an inbound trace ID must be kept as the parent: %q", trace.ParentID())
	}
}

func TestUntrustedIncomingTraceIDBecomesParent(t *testing.T) {
	tracer, err := New(t.TempDir(), WithTrustIncomingTraceID(false))
	if err != nil {
		t.Fatal(err)
	}
	trace, err := tracer.Start("upstream-trace")
	if err != nil {
		t.Fatal(err)
	}
	if trace.ID() == "upstream-trace" || trace.ParentID() != "upstream-trace" {
		t.Fatalf("unexpected IDs: trace=%q parent=%q", trace.ID(), trace.ParentID())
	}
	if err := trace.Event("received", nil); err != nil {
		t.Fatal(err)
	}
	directory := tracer.configuration.LogDirectory()
	if err := tracer.Close(); err != nil {
		t.Fatal(err)
	}
	event := readTestEvents(t, directory)[0]
	if event.ParentTraceID != "upstream-trace" || event.TraceID != trace.ID() {
		t.Fatalf("wire parent mismatch: %#v", event)
	}
}

func TestOversizedRecordsDegradeWithoutLoss(t *testing.T) {
	directory := t.TempDir()
	tracer, err := New(directory, WithMaxFileBytes(1024), WithFailOnError(true))
	if err != nil {
		t.Fatal(err)
	}
	trace, err := tracer.Start("oversized-trace")
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 5; index++ {
		if err := trace.Event("big", Data{"blob": strings.Repeat("a", 10_000)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tracer.Close(); err != nil {
		t.Fatal(err)
	}

	events := readTestEvents(t, directory)
	if len(events) != 5 {
		t.Fatalf("events lost: %d", len(events))
	}
	if !strings.Contains(events[0].Data["_encoding_error"].(string), "record_too_large") {
		t.Fatalf("event was not degraded: %#v", events[0])
	}
	for _, path := range testLogFiles(t, directory) {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() > 1024 {
			t.Fatalf("shard overflow: %s = %d", path, info.Size())
		}
	}
}

func TestRotationAndFreshTracerResumeHighestShard(t *testing.T) {
	directory := t.TempDir()
	configuration, err := NewConfiguration(directory, WithMaxFileBytes(300))
	if err != nil {
		t.Fatal(err)
	}
	first, err := NewFromConfiguration(configuration)
	if err != nil {
		t.Fatal(err)
	}
	trace, _ := first.Start("rotation-trace")
	for _, name := range []string{"first", "second", "third"} {
		if err := trace.Event(name, Data{"body": strings.Repeat(name, 20)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	before := len(testLogFiles(t, directory))

	second, err := NewFromConfiguration(configuration)
	if err != nil {
		t.Fatal(err)
	}
	secondTrace, _ := second.Start("rotation-trace-2")
	if err := secondTrace.Event("fourth", Data{"body": strings.Repeat("x", 40)}); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	files := testLogFiles(t, directory)
	if len(files) < before || len(files) < 2 {
		t.Fatalf("rotation missing: %#v", files)
	}
	if len(readTestEvents(t, directory)) != 4 {
		t.Fatal("fresh tracer lost existing events")
	}
}

func TestFailSafeAndStrictModes(t *testing.T) {
	directory := t.TempDir()
	invalidPath := filepath.Join(directory, "not-a-directory")
	if err := os.WriteFile(invalidPath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var reported []error
	safeTracer, err := New(invalidPath, WithOnError(func(err error) { reported = append(reported, err) }))
	if err != nil {
		t.Fatal(err)
	}
	safeTrace, _ := safeTracer.Start("safe-trace")
	if err := safeTrace.Event("lost", nil); err != nil {
		t.Fatalf("fail-safe mode returned error: %v", err)
	}
	if safeTracer.DroppedEventCount() != 1 || len(reported) != 1 {
		t.Fatalf("failure was not observable: dropped=%d errors=%d", safeTracer.DroppedEventCount(), len(reported))
	}

	strictTracer, err := New(invalidPath, WithFailOnError(true))
	if err != nil {
		t.Fatal(err)
	}
	strictTrace, _ := strictTracer.Start("strict-trace")
	if err := strictTrace.Event("lost", nil); err == nil {
		t.Fatal("strict mode should return an error")
	}
}

func TestInvalidEventNamesAndControlCharacters(t *testing.T) {
	directory := t.TempDir()
	tracer, _ := New(directory)
	trace, _ := tracer.Start("event-name-trace")
	if err := trace.Event("   ", nil); err == nil {
		t.Fatal("empty event name must fail")
	}
	if err := trace.Event("webhook\x1b[31m_received", nil); err != nil {
		t.Fatal(err)
	}
	_ = tracer.Close()
	if event := readTestEvents(t, directory)[0]; event.Event != "webhook[31m_received" {
		t.Fatalf("control character survived: %q", event.Event)
	}
}

type recordingWriter struct {
	mu       sync.Mutex
	calls    int
	failCall int
	written  []uint64
}

func (writer *recordingWriter) Write(ctx context.Context, event TraceEvent) error {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	writer.calls++
	if writer.calls == writer.failCall {
		return errors.New("disk full")
	}
	writer.written = append(writer.written, event.Sequence)
	return nil
}
func (writer *recordingWriter) Flush(context.Context) error { return nil }
func (writer *recordingWriter) Close() error                { return nil }

type fixedClock struct {
	mu    sync.Mutex
	start time.Time
	tick  int64
}

func (clock *fixedClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.start.Add(time.Duration(clock.tick) * time.Millisecond)
}
func (clock *fixedClock) Monotonic() time.Duration {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	value := time.Duration(clock.tick) * time.Millisecond
	clock.tick++
	return value
}

// A failed write leaves a gap in sequence instead of renumbering, so a reader of the
// log can tell that an event is missing.
func TestFailedWriteLeavesGapInSequence(t *testing.T) {
	configuration, _ := NewConfiguration("unused")
	writer := &recordingWriter{failCall: 2}
	tracer, err := NewWithDependencies(configuration, writer, newSystemClock())
	if err != nil {
		t.Fatal(err)
	}
	trace, _ := tracer.Start("sequence-trace")
	for index := 0; index < 4; index++ {
		if err := trace.Event("event", nil); err != nil {
			t.Fatal(err)
		}
	}
	if got := writer.written; len(got) != 3 || got[0] != 1 || got[1] != 3 || got[2] != 4 {
		t.Fatalf("unexpected sequences: %#v", got)
	}
	if tracer.DroppedEventCount() != 1 {
		t.Fatalf("dropped count mismatch: %d", tracer.DroppedEventCount())
	}
}

func TestElapsedUsesMonotonicClock(t *testing.T) {
	directory := t.TempDir()
	configuration, _ := NewConfiguration(directory)
	clock := &fixedClock{start: time.Date(2026, 7, 10, 10, 41, 20, 0, time.UTC)}
	tracer, _ := NewWithDependencies(configuration, NewJSONLFileWriter(configuration), clock)
	trace, _ := tracer.Start("fixed-clock")
	_ = trace.Event("first", nil)
	_ = trace.Event("second", nil)
	_ = tracer.Close()
	events := readTestEvents(t, directory)
	if events[0].ElapsedMS != 1 || events[1].ElapsedMS != 2 {
		t.Fatalf("elapsed mismatch: %#v", events)
	}
}

func TestTraceIsSafeForConcurrentGoroutines(t *testing.T) {
	configuration, _ := NewConfiguration("unused", WithFailOnError(true))
	writer := &recordingWriter{failCall: -1}
	tracer, _ := NewWithDependencies(configuration, writer, newSystemClock())
	trace, _ := tracer.Start("goroutine-trace")

	var wait sync.WaitGroup
	for index := 0; index < 100; index++ {
		wait.Add(1)
		go func(value int) {
			defer wait.Done()
			if err := trace.Event("event", Data{"value": value}); err != nil {
				t.Errorf("event failed: %v", err)
			}
		}(index)
	}
	wait.Wait()
	if len(writer.written) != 100 {
		t.Fatalf("events lost: %d", len(writer.written))
	}
	for index, sequence := range writer.written {
		if sequence != uint64(index+1) {
			t.Fatalf("sequence %d = %d", index, sequence)
		}
	}
}

// A settable clock lets a test walk a writer across a UTC date boundary.
type settableClock struct {
	mu  sync.Mutex
	now time.Time
}

func (clock *settableClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *settableClock) Monotonic() time.Duration { return 0 }

func (clock *settableClock) set(now time.Time) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now = now
}

// Retention runs on the first rotation into each UTC date, so a long-lived process keeps
// expiring old shards instead of sweeping once at startup and never again.
func TestRetentionRunsOnEachNewUTCDate(t *testing.T) {
	directory := t.TempDir()
	configuration, _ := NewConfiguration(directory, WithRetentionDays(7))
	clock := &settableClock{now: time.Now().UTC()}
	tracer, err := NewWithDependencies(configuration, NewJSONLFileWriter(configuration), clock)
	if err != nil {
		t.Fatal(err)
	}
	trace, _ := tracer.Start("retention-trace")

	stale := filepath.Join(directory, "2020-01-01.jsonl")
	if err := os.WriteFile(stale, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := trace.Event("first-day", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the first rotation did not expire the stale shard")
	}

	// A shard that goes stale while the process keeps running must still be expired.
	later := filepath.Join(directory, "2020-01-02.jsonl")
	if err := os.WriteFile(later, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	clock.set(clock.Now().AddDate(0, 0, 1))
	if err := trace.Event("next-day", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(later); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("retention did not run again after the UTC date changed")
	}
	if err := tracer.Close(); err != nil {
		t.Fatal(err)
	}
}

// Retention deletes only shards this writer produces. Anything else in the directory
// belongs to somebody else, and a tracing library must not destroy it.
func TestRetentionLeavesForeignFilesAlone(t *testing.T) {
	directory := t.TempDir()
	foreign := []string{
		"2020-01-01-backup.jsonl",
		"2020-01-01-archive-1.jsonl",
		"2020-01-01.jsonl.bak",
		"notes.jsonl",
	}
	own := []string{"2020-01-01.jsonl", "2020-01-01-1.jsonl"}
	for _, name := range append(append([]string{}, foreign...), own...) {
		if err := os.WriteFile(filepath.Join(directory, name), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	tracer, _ := New(directory, WithRetentionDays(7))
	trace, _ := tracer.Start("retention-trace")
	if err := trace.Event("now", nil); err != nil {
		t.Fatal(err)
	}
	if err := tracer.Close(); err != nil {
		t.Fatal(err)
	}

	for _, name := range foreign {
		if _, err := os.Stat(filepath.Join(directory, name)); err != nil {
			t.Fatalf("retention deleted a file it does not own: %s", name)
		}
	}
	for _, name := range own {
		if _, err := os.Stat(filepath.Join(directory, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("retention kept an expired shard: %s", name)
		}
	}
}

// U+2028 and U+2029 are legal inside a JSON string, but some readers treat them as line
// breaks. If they reached the file unescaped, one record would arrive as two broken
// halves. Go's encoder escapes them; this pins that down as a format guarantee.
func TestRecordStaysOnOneLineWithUnicodeLineSeparators(t *testing.T) {
	directory := t.TempDir()
	tracer, err := New(directory, WithFailOnError(true))
	if err != nil {
		t.Fatal(err)
	}
	trace, _ := tracer.Start("separator-trace")
	if err := trace.Event("event", Data{"text": "before\u2028middle\u2029after"}); err != nil {
		t.Fatal(err)
	}
	if err := tracer.Close(); err != nil {
		t.Fatal(err)
	}

	content, err := os.ReadFile(testLogFiles(t, directory)[0])
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(content, []byte("\u2028")) || bytes.Contains(content, []byte("\u2029")) {
		t.Fatal("a raw line separator reached the shard")
	}
	if lines := bytes.Count(bytes.TrimSpace(content), []byte("\n")); lines != 0 {
		t.Fatalf("the record was split across %d lines", lines+1)
	}
	events := readTestEvents(t, directory)
	if events[0].Data["text"] != "before\u2028middle\u2029after" {
		t.Fatalf("the payload did not survive the round trip: %#v", events[0].Data)
	}
}

func TestRetentionAndPOSIXPermissions(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "logs")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "2020-01-01.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tracer, _ := New(directory, WithRetentionDays(7))
	trace, _ := tracer.Start("retention-trace")
	_ = trace.Event("now", nil)
	_ = tracer.Close()
	if len(readTestEvents(t, directory)) != 1 {
		t.Fatal("stale shard was not removed")
	}

	if os.PathSeparator == '\\' {
		t.Skip("POSIX permission bits are not meaningful on Windows")
	}
	permissionDirectory := filepath.Join(root, "permissions")
	permissionTracer, _ := New(permissionDirectory)
	permissionTrace, _ := permissionTracer.Start("permissions-trace")
	_ = permissionTrace.Event("created", nil)
	_ = permissionTracer.Close()
	directoryInfo, _ := os.Stat(permissionDirectory)
	fileInfo, _ := os.Stat(testLogFiles(t, permissionDirectory)[0])
	if directoryInfo.Mode().Perm() != 0o750 || fileInfo.Mode().Perm() != 0o640 {
		t.Fatalf("permissions mismatch: dir=%o file=%o", directoryInfo.Mode().Perm(), fileInfo.Mode().Perm())
	}
}

// Close is terminal: a later event is rejected and counted, not written to a shard that
// would then be reopened and never closed again.
func TestCloseIsTerminalAndIdempotent(t *testing.T) {
	directory := t.TempDir()
	tracer, _ := New(directory, WithFailOnError(true))
	trace, _ := tracer.Start("close-trace")
	if err := trace.Event("before-close", nil); err != nil {
		t.Fatal(err)
	}
	if err := tracer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tracer.Close(); err != nil {
		t.Fatalf("Close must be idempotent: %v", err)
	}

	if err := trace.Event("after-close", nil); err == nil {
		t.Fatal("an event after Close must be rejected")
	}
	if tracer.DroppedEventCount() != 1 {
		t.Fatalf("an event after Close must be counted as dropped: %d", tracer.DroppedEventCount())
	}
	events := readTestEvents(t, directory)
	if len(events) != 1 || events[0].Event != "before-close" {
		t.Fatalf("the writer reopened after Close: %#v", events)
	}
}

// A fail-safe tracer swallows the error, but the event is still counted, never written.
func TestFailSafeEventAfterCloseIsDroppedSilently(t *testing.T) {
	directory := t.TempDir()
	tracer, _ := New(directory)
	trace, _ := tracer.Start("close-trace")
	_ = trace.Event("before-close", nil)
	_ = tracer.Close()

	if err := trace.Event("after-close", nil); err != nil {
		t.Fatalf("fail-safe mode must not surface the error: %v", err)
	}
	if tracer.DroppedEventCount() != 1 {
		t.Fatalf("dropped count mismatch: %d", tracer.DroppedEventCount())
	}
	if events := readTestEvents(t, directory); len(events) != 1 {
		t.Fatalf("the writer reopened after Close: %#v", events)
	}
}

type testWireEvent struct {
	Timestamp     string         `json:"timestamp"`
	TraceID       string         `json:"trace_id"`
	ParentTraceID string         `json:"parent_trace_id"`
	Event         string         `json:"event"`
	Sequence      uint64         `json:"sequence"`
	ElapsedMS     float64        `json:"elapsed_ms"`
	Data          map[string]any `json:"data"`
}

func testLogFiles(t *testing.T, directory string) []string {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	files := make([]string, 0)
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".jsonl") {
			files = append(files, filepath.Join(directory, entry.Name()))
		}
	}
	sort.Strings(files)
	return files
}

func readTestEvents(t *testing.T, directory string) []testWireEvent {
	t.Helper()
	events := make([]testWireEvent, 0)
	for _, path := range testLogFiles(t, directory) {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(content)), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var event testWireEvent
			if err := json.Unmarshal([]byte(line), &event); err != nil {
				t.Fatalf("invalid JSONL in %s: %v\n%s", path, err, line)
			}
			events = append(events, event)
		}
	}
	return events
}
