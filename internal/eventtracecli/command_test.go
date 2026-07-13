package eventtracecli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestShowTimelineAndNotFoundExitCode(t *testing.T) {
	directory := t.TempDir()
	writeJSONL(t, directory, map[string]any{
		"timestamp": "2026-07-10T10:00:00.001000Z",
		"trace_id":  "cli-trace-123", "event": "request_start", "sequence": 1, "elapsed_ms": 1.0, "data": map[string]any{},
	}, map[string]any{
		"timestamp": "2026-07-10T10:00:00.006000Z",
		"trace_id":  "cli-trace-123", "event": "request_end", "sequence": 2, "elapsed_ms": 6.0, "data": map[string]any{},
	})

	var out, errOut bytes.Buffer
	command := Command{Out: &out, Err: &errOut}
	if code := command.Run([]string{"show", "cli-trace-123", "--dir=" + directory}); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	for _, expected := range []string{"Trace: cli-trace-123", "request_start", "request_end +5 ms", "Total duration: 6 ms"} {
		if !strings.Contains(out.String(), expected) {
			t.Fatalf("missing %q in:\n%s", expected, out.String())
		}
	}

	out.Reset()
	if code := command.Run([]string{"show", "missing-trace", "--dir", directory}); code != 2 {
		t.Fatalf("missing trace exit code = %d", code)
	}
}

func TestEscapesANSIAndAggregatesMalformedWarnings(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "2026-07-10.jsonl")
	good, _ := json.Marshal(map[string]any{
		"timestamp": "2026-07-10T10:00:00.000000Z", "trace_id": "unsafe-trace-id",
		"event": "\x1b[2JFAKE-ALERT", "sequence": 1, "elapsed_ms": 0, "data": map[string]any{},
	})
	garbage := strings.Repeat("{\"trace_id\":\"unsafe-trace-id\" TRUNCATED\n", 3)
	if err := os.WriteFile(path, append(append(good, '\n'), []byte(garbage)...), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	code := (Command{Out: &out, Err: &errOut}).Run([]string{"show", "unsafe-trace-id", "--dir=" + directory})
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if strings.Contains(out.String(), "\x1b") || !strings.Contains(out.String(), "\\x1B") {
		t.Fatalf("ANSI was not escaped: %q", out.String())
	}
	if strings.Count(errOut.String(), "malformed") != 1 || !strings.Contains(errOut.String(), "skipped 3 malformed") {
		t.Fatalf("malformed warnings were not aggregated: %q", errOut.String())
	}
}

// The CLI reads files it did not necessarily write. A bidi override can visually pass one
// event name off as another in the operator's terminal, and a line separator can break the
// output apart, so both are escaped rather than printed.
func TestEscapesBidiOverrideAndLineSeparators(t *testing.T) {
	directory := t.TempDir()
	writeJSONL(t, directory, map[string]any{
		"timestamp": "2026-07-10T10:00:00.000000Z", "trace_id": "bidi-trace",
		"event": "payment_\u202Ednuferter", "sequence": 1, "elapsed_ms": 0, "data": map[string]any{},
	}, map[string]any{
		"timestamp": "2026-07-10T10:00:00.001000Z", "trace_id": "bidi-trace",
		"event": "split\u2028here", "sequence": 2, "elapsed_ms": 1, "data": map[string]any{},
	})

	var out, errOut bytes.Buffer
	if code := (Command{Out: &out, Err: &errOut}).Run([]string{"show", "bidi-trace", "--dir=" + directory}); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	rendered := out.String()
	if strings.ContainsRune(rendered, '\u202E') || strings.ContainsRune(rendered, '\u2028') {
		t.Fatalf("a dangerous character reached the terminal: %q", rendered)
	}
	if !strings.Contains(rendered, "\\u202E") || !strings.Contains(rendered, "\\u2028") {
		t.Fatalf("characters were not escaped: %q", rendered)
	}
	// The record with the separator is still one event, not two halves.
	if strings.Count(rendered, "\n") != 4 {
		t.Fatalf("output line count changed: %q", rendered)
	}
}

// A file without newlines is corrupt. The reader must report it, not pull it into memory.
func TestOversizedLineIsSkippedInsteadOfBuffered(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "2026-07-10.jsonl")
	good, _ := json.Marshal(map[string]any{
		"timestamp": "2026-07-10T10:00:00.000000Z", "trace_id": "huge-trace",
		"event": "kept", "sequence": 1, "elapsed_ms": 0, "data": map[string]any{},
	})
	oversized := append([]byte(`{"trace_id":"huge-trace","data":"`), bytes.Repeat([]byte("a"), maxLineBytes+1)...)
	if err := os.WriteFile(path, append(append(good, '\n'), oversized...), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	code := (Command{Out: &out, Err: &errOut}).Run([]string{"show", "huge-trace", "--dir=" + directory})
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "kept") {
		t.Fatalf("the intact record before the corrupt line was lost: %q", out.String())
	}
	if !strings.Contains(errOut.String(), "contains a line over") {
		t.Fatalf("the oversized line was not reported: %q", errOut.String())
	}
}

func writeJSONL(t *testing.T, directory string, records ...map[string]any) {
	t.Helper()
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	for _, record := range records {
		if err := encoder.Encode(record); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(directory, "2026-07-10.jsonl"), buffer.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}
