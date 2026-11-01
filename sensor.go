package main

import (
	"runtime"
	"slices"
	"sync"
	"time"
)

// WakeSensor is a normal-priority thread that sleeps 1 ms at a time and records how late it wakes
// up: the same thing that makes a game's frame or a key press late. Idle priority keeps agent
// work behind the foreground almost always, but not quite: Windows briefly runs any thread that
// has waited about 4 seconds above normal priority, so thousands of always-busy agent threads
// still add delay. The sensor sees that delay whatever its cause.
type WakeSensor struct {
	mu      sync.Mutex
	samples []float64 // ring of the latest oversleeps, ms
	next    int
	LagMs   float64 // lagging when the 95th percentile is above this
	CalmMs  float64 // calm when it is below this
}

func NewWakeSensor() *WakeSensor {
	s := &WakeSensor{samples: make([]float64, 0, 300), LagMs: 3, CalmMs: 1.5}
	go func() {
		runtime.LockOSThread()
		for {
			t := nowMs()
			time.Sleep(time.Millisecond)
			late := nowMs() - t - 1
			s.mu.Lock()
			if len(s.samples) < cap(s.samples) {
				s.samples = append(s.samples, late)
			} else {
				s.samples[s.next] = late
				s.next = (s.next + 1) % len(s.samples)
			}
			s.mu.Unlock()
		}
	}()
	return s
}

// P95 is the 95th-percentile wake delay over about the last third of a second, in ms.
func (s *WakeSensor) P95() float64 {
	s.mu.Lock()
	xs := slices.Clone(s.samples)
	s.mu.Unlock()
	return percentile(xs, .95)
}

// State reports whether the foreground is being held up now, or clearly is not.
func (s *WakeSensor) State() (lagging, calm bool) {
	p := s.P95()
	return p > s.LagMs, p < s.CalmMs
}
