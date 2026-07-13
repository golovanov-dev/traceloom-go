package traceloom

import "sync/atomic"

type metrics struct {
	dropped atomic.Uint64
}

func (metrics *metrics) recordDroppedEvent()   { metrics.dropped.Add(1) }
func (metrics *metrics) droppedEvents() uint64 { return metrics.dropped.Load() }
