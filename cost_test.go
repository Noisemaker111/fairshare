package main

import (
	"os"
	"testing"
)

// What one look of adopt costs: go test -bench Look -run X. A look is one snapshot; twice a second
// the same snapshot also reads the lanes' threads.
func BenchmarkLookSnapshot(b *testing.B) {
	for b.Loop() {
		snapshot()
	}
}

func BenchmarkLookSnapshotWithThreads(b *testing.B) {
	self := uint32(os.Getpid())
	for b.Loop() {
		scan(func(e procEntry) bool { return e.pid == self }, func(threadInfo) {})
	}
}
