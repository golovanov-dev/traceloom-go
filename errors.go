package traceloom

import "fmt"

// ConfigurationError reports an invalid immutable configuration.
type ConfigurationError struct {
	Message string
}

func (e *ConfigurationError) Error() string { return e.Message }

// TracingError reports a runtime tracing failure.
type TracingError struct {
	Message string
	Err     error
}

func (e *TracingError) Error() string {
	if e.Err == nil {
		return e.Message
	}
	return fmt.Sprintf("%s: %v", e.Message, e.Err)
}

func (e *TracingError) Unwrap() error { return e.Err }
