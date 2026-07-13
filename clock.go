package traceloom

import "time"

// Clock separates wall-clock timestamps from monotonic duration readings.
type Clock interface {
	Now() time.Time
	Monotonic() time.Duration
}

type systemClock struct {
	origin time.Time
}

func newSystemClock() *systemClock                  { return &systemClock{origin: time.Now()} }
func (clock *systemClock) Now() time.Time           { return time.Now().UTC() }
func (clock *systemClock) Monotonic() time.Duration { return time.Since(clock.origin) }
