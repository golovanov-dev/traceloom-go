package traceloom

import (
	"context"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	maxEventNameBytes = 128
	invalidEventName  = "[INVALID_EVENT_NAME]"
)

// Trace groups a thread-safe sequence of events under one trace ID.
type Trace struct {
	mu            sync.Mutex
	traceID       string
	parentTraceID string
	configuration Configuration
	writer        Writer
	sanitizer     *Sanitizer
	clock         Clock
	metrics       *metrics
	startedAt     time.Duration
	sequence      uint64
}

func (trace *Trace) ID() string       { return trace.traceID }
func (trace *Trace) ParentID() string { return trace.parentTraceID }

func (trace *Trace) Event(name string, data Data) error {
	return trace.EventContext(context.Background(), name, data)
}

func (trace *Trace) EventContext(ctx context.Context, name string, data Data) error {
	normalizedName, err := normalizeEventName(name)
	if err != nil {
		return err
	}
	normalizedData := trace.sanitizer.Sanitize(data)

	trace.mu.Lock()
	defer trace.mu.Unlock()

	// The sequence advances even when the write fails, so a dropped event leaves a gap
	// rather than being renumbered away: a reader of the log can see that it is missing.
	trace.sequence++
	event := TraceEvent{
		Timestamp:     trace.clock.Now(),
		TraceID:       trace.traceID,
		ParentTraceID: trace.parentTraceID,
		Name:          normalizedName,
		Sequence:      trace.sequence,
		Elapsed:       trace.clock.Monotonic() - trace.startedAt,
		Data:          normalizedData,
	}
	if err := trace.writer.Write(ctx, event); err != nil {
		trace.metrics.recordDroppedEvent()
		return trace.handleFailure(err)
	}
	return nil
}

func (trace *Trace) Flush() error { return trace.FlushContext(context.Background()) }

func (trace *Trace) FlushContext(ctx context.Context) error {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	if err := trace.writer.Flush(ctx); err != nil {
		return trace.handleFailure(err)
	}
	return nil
}

func (trace *Trace) handleFailure(err error) error {
	if trace.configuration.failOnError {
		return err
	}
	safeOnError(trace.configuration.onError, err)
	return nil
}

func normalizeEventName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", &TracingError{Message: "trace event name must not be empty"}
	}
	if !utf8.ValidString(name) {
		name = invalidEventName
	} else {
		name = strings.Map(func(char rune) rune {
			if unicode.IsControl(char) || unicode.In(char, unicode.Cf, unicode.Co, unicode.Cs) {
				return -1
			}
			return char
		}, name)
		name = strings.TrimSpace(name)
		if name == "" {
			name = invalidEventName
		}
	}
	if len(name) > maxEventNameBytes {
		name = strings.TrimRightFunc(truncateUTF8(name, maxEventNameBytes), unicode.IsSpace)
	}
	if name == "" {
		name = invalidEventName
	}
	return name, nil
}
