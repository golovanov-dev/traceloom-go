package eventtracecli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// maxLineBytes caps how much of one JSONL line the reader will hold. A file without
// newlines is corrupt, and reading it whole is how a log turns into an OOM.
const maxLineBytes = 8 * 1024 * 1024

type Command struct {
	Out io.Writer
	Err io.Writer
}

type timelineEvent struct {
	Timestamp     string   `json:"timestamp"`
	TraceID       string   `json:"trace_id"`
	ParentTraceID string   `json:"parent_trace_id"`
	Event         string   `json:"event"`
	Sequence      uint64   `json:"sequence"`
	ElapsedMS     *float64 `json:"elapsed_ms"`
}

func (command Command) Run(arguments []string) int {
	out := command.Out
	errOut := command.Err
	if out == nil {
		out = os.Stdout
	}
	if errOut == nil {
		errOut = os.Stderr
	}

	operation := ""
	if len(arguments) > 0 {
		operation = arguments[0]
	}
	if operation != "show" {
		printUsage(errOut)
		if operation == "" || operation == "help" || operation == "--help" || operation == "-h" {
			return 0
		}
		return 1
	}

	traceID := ""
	if len(arguments) > 1 {
		traceID = strings.TrimSpace(arguments[1])
	}
	directory := optionValue(arguments, "--dir")
	if directory == "" {
		directory = "logs"
	}
	if traceID == "" {
		fmt.Fprintln(errOut, "Trace ID is required.")
		fmt.Fprintln(errOut)
		printUsage(errOut)
		return 1
	}

	files, err := logFiles(directory)
	if err != nil {
		fmt.Fprintf(errOut, "Log directory does not exist: %s\n", safe(directory))
		return 1
	}
	events, malformed := findEvents(files, traceID, errOut)
	if malformed > 0 {
		fmt.Fprintf(errOut, "Warning: skipped %d malformed JSONL line(s).\n", malformed)
	}
	if len(events) == 0 {
		fmt.Fprintf(out, "Trace not found: %s\n", safe(traceID))
		return 2
	}

	printTimeline(out, traceID, events)
	return 0
}

func optionValue(arguments []string, name string) string {
	for index, argument := range arguments {
		if argument == name && index+1 < len(arguments) {
			return arguments[index+1]
		}
		if strings.HasPrefix(argument, name+"=") {
			return strings.TrimPrefix(argument, name+"=")
		}
	}
	return ""
}

func logFiles(directory string) ([]string, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	files := make([]string, 0)
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".jsonl") {
			files = append(files, filepath.Join(directory, entry.Name()))
		}
	}
	sort.Strings(files)
	return files, nil
}

func findEvents(files []string, traceID string, errOut io.Writer) ([]timelineEvent, int) {
	events := make([]timelineEvent, 0)
	malformed := 0
	needle := []byte(traceID)

	for _, path := range files {
		file, err := os.Open(path)
		if err != nil {
			fmt.Fprintf(errOut, "Warning: unable to read %s\n", safe(path))
			continue
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
		for scanner.Scan() {
			line := scanner.Bytes()
			if !bytes.Contains(line, needle) {
				continue
			}
			var event timelineEvent
			if err := json.Unmarshal(bytes.TrimSpace(line), &event); err != nil {
				malformed++
			} else if event.TraceID == traceID {
				events = append(events, event)
			}
		}
		// A line longer than the cap is a corrupt file, not a record: report it and move
		// on rather than reading an unbounded amount of it into memory.
		if err := scanner.Err(); err != nil {
			if errors.Is(err, bufio.ErrTooLong) {
				malformed++
				fmt.Fprintf(errOut, "Warning: %s contains a line over %d bytes; skipped the rest of the file.\n",
					safe(path), maxLineBytes)
			} else {
				fmt.Fprintf(errOut, "Warning: unable to read %s\n", safe(path))
			}
		}
		_ = file.Close()
	}

	sort.SliceStable(events, func(left, right int) bool {
		if events[left].Timestamp != events[right].Timestamp {
			return events[left].Timestamp < events[right].Timestamp
		}
		return events[left].Sequence < events[right].Sequence
	})
	return events, malformed
}

func printTimeline(out io.Writer, traceID string, events []timelineEvent) {
	fmt.Fprintf(out, "Trace: %s\n", safe(traceID))
	if events[0].ParentTraceID != "" {
		fmt.Fprintf(out, "Parent: %s\n", safe(events[0].ParentTraceID))
	}

	var previous *float64
	last := 0.0
	for _, event := range events {
		delta := ""
		if event.ElapsedMS != nil {
			last = *event.ElapsedMS
			if previous != nil {
				value := *event.ElapsedMS - *previous
				if value < 0 {
					value = 0
				}
				delta = " +" + formatMilliseconds(value)
			}
			copy := *event.ElapsedMS
			previous = &copy
		}
		fmt.Fprintf(out, "%s %s%s\n", formatTime(event.Timestamp), safe(event.Event), delta)
	}
	fmt.Fprintf(out, "Total duration: %s\n", formatMilliseconds(last))
}

// safe renders a value read from a log file, which may have been written by anything.
// Besides C0 controls it escapes format characters such as U+202E RIGHT-TO-LEFT
// OVERRIDE, which can visually pass one event name off as another in the operator's
// terminal, and U+2028/U+2029, which some readers treat as line breaks.
func safe(value string) string {
	if !utf8.ValidString(value) {
		return "[UNPRINTABLE]"
	}
	var builder strings.Builder
	for _, char := range value {
		switch {
		case char <= 0x1f || char == 0x7f:
			fmt.Fprintf(&builder, "\\x%02X", char)
		case char == '\u2028' || char == '\u2029' || unicode.In(char, unicode.Cf, unicode.Co, unicode.Cs):
			fmt.Fprintf(&builder, "\\u%04X", char)
		default:
			builder.WriteRune(char)
		}
	}
	return builder.String()
}

func formatTime(timestamp string) string {
	parsed, err := time.Parse(time.RFC3339Nano, timestamp)
	if err == nil {
		return parsed.Format("15:04:05.000")
	}
	if timestamp == "" {
		return "unknown-time"
	}
	return safe(timestamp)
}

func formatMilliseconds(milliseconds float64) string {
	formatted := strconv.FormatFloat(milliseconds, 'f', 3, 64)
	formatted = strings.TrimRight(strings.TrimRight(formatted, "0"), ".")
	if formatted == "" || formatted == "-0" {
		formatted = "0"
	}
	return formatted + " ms"
}

func printUsage(out io.Writer) {
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  eventtrace show <trace-id> --dir=logs")
}
