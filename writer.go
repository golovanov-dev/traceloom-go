package traceloom

import "context"

// Writer persists normalized trace events.
type Writer interface {
	Write(context.Context, TraceEvent) error
	Flush(context.Context) error
	Close() error
}
