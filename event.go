package traceloom

import (
	"math"
	"time"
)

// Data is one event's structured payload.
type Data map[string]any

// TraceEvent is the public storage contract passed to Writer implementations.
type TraceEvent struct {
	Timestamp     time.Time
	TraceID       string
	ParentTraceID string
	Name          string
	Sequence      uint64
	Elapsed       time.Duration
	Data          Data
}

type wireRecord struct {
	Timestamp     string  `json:"timestamp"`
	TraceID       string  `json:"trace_id"`
	ParentTraceID string  `json:"parent_trace_id,omitempty"`
	Event         string  `json:"event"`
	Sequence      uint64  `json:"sequence"`
	ElapsedMS     float64 `json:"elapsed_ms"`
	Data          Data    `json:"data"`
}

func (event TraceEvent) utcDate() string {
	return event.Timestamp.UTC().Format("2006-01-02")
}

func (event TraceEvent) record() wireRecord {
	elapsed := float64(event.Elapsed) / float64(time.Millisecond)
	elapsed = math.Round(elapsed*1000) / 1000
	return wireRecord{
		Timestamp:     event.Timestamp.UTC().Format("2006-01-02T15:04:05.000000Z"),
		TraceID:       event.TraceID,
		ParentTraceID: event.ParentTraceID,
		Event:         event.Name,
		Sequence:      event.Sequence,
		ElapsedMS:     elapsed,
		Data:          event.Data,
	}
}

func (event TraceEvent) degradedRecord(reason string) wireRecord {
	record := event.record()
	if len(reason) > 512 {
		reason = truncateUTF8(reason, 512)
	}
	record.Data = Data{"_encoding_error": reason}
	return record
}
