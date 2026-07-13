package traceloom

import (
	"context"
	"reflect"
)

// Tracer owns shared configuration, writer, sanitizer, and dropped-event metrics.
type Tracer struct {
	configuration Configuration
	writer        Writer
	clock         Clock
	sanitizer     *Sanitizer
	metrics       *metrics
}

// New creates a tracer writing JSONL files into logDirectory.
func New(logDirectory string, options ...Option) (*Tracer, error) {
	configuration, err := NewConfiguration(logDirectory, options...)
	if err != nil {
		return nil, err
	}
	return NewFromConfiguration(configuration)
}

// NewFromConfiguration creates a tracer with the built-in JSONL writer.
func NewFromConfiguration(configuration Configuration) (*Tracer, error) {
	if err := validateBuiltConfiguration(configuration); err != nil {
		return nil, err
	}
	return NewWithDependencies(configuration, NewJSONLFileWriter(configuration), newSystemClock())
}

// NewWithDependencies creates a tracer with a custom writer and clock.
func NewWithDependencies(configuration Configuration, writer Writer, clock Clock) (*Tracer, error) {
	if err := validateBuiltConfiguration(configuration); err != nil {
		return nil, err
	}
	if isNilInterface(writer) {
		return nil, &ConfigurationError{Message: "writer must not be nil"}
	}
	if isNilInterface(clock) {
		return nil, &ConfigurationError{Message: "clock must not be nil"}
	}
	return &Tracer{
		configuration: configuration,
		writer:        writer,
		clock:         clock,
		sanitizer:     NewSanitizer(configuration),
		metrics:       &metrics{},
	}, nil
}

// Start creates a trace, optionally continuing a validated incoming ID.
func (tracer *Tracer) Start(incomingTraceID string) (*Trace, error) {
	incoming, accepted := SanitizeTraceID(incomingTraceID)
	traceID := ""
	parentID := ""

	if accepted && tracer.configuration.trustIncomingTraceID {
		traceID = incoming
	} else {
		generated, err := GenerateTraceID()
		if err != nil {
			return nil, &TracingError{Message: "generate trace ID", Err: err}
		}
		traceID = generated
		if accepted {
			parentID = incoming
		}
	}

	return &Trace{
		traceID:       traceID,
		parentTraceID: parentID,
		configuration: tracer.configuration,
		writer:        tracer.writer,
		sanitizer:     tracer.sanitizer,
		clock:         tracer.clock,
		metrics:       tracer.metrics,
		startedAt:     tracer.clock.Monotonic(),
	}, nil
}

func (tracer *Tracer) DroppedEventCount() uint64 { return tracer.metrics.droppedEvents() }

func (tracer *Tracer) Flush() error { return tracer.FlushContext(context.Background()) }

func (tracer *Tracer) FlushContext(ctx context.Context) error {
	return tracer.writer.Flush(ctx)
}

func (tracer *Tracer) Close() error { return tracer.writer.Close() }

func validateBuiltConfiguration(configuration Configuration) error {
	if configuration.logDirectory == "" || configuration.maxFileBytes <= 0 || configuration.maxRecordBytes <= 0 {
		return &ConfigurationError{Message: "configuration must be created with NewConfiguration"}
	}
	return nil
}

func isNilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func safeOnError(handler func(error), err error) {
	if handler == nil {
		return
	}
	defer func() {
		_ = recover()
	}()
	handler(err)
}
