package main

import (
	"testing"
	"time"
)

func TestCalibrateHitsTarget(t *testing.T) {
	iters := calibrate(2500 * time.Microsecond)
	if iters <= 0 {
		t.Fatalf("calibrate returned %d", iters)
	}
	buf := make([]uint64, 8192)
	best := 1e9
	for range 10 {
		s := nowMs()
		busy(iters, buf)
		best = min(best, nowMs()-s)
	}
	if best < 1.5 || best > 6 {
		t.Fatalf("%d iterations took %.2f ms, want about 2.5 ms", iters, best)
	}
}
