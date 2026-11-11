//go:build windows

package main

import (
	"os"
	"testing"
	"time"
)

// A real look at this PC, with the reserve worked out and not applied: a test never changes what
// Owl holds on the machine it runs on.
func TestALookMeasuresThisPC(t *testing.T) {
	t.Setenv("FAIRSHARE_HOME", t.TempDir())
	t.Setenv("FAIRSHARE_RESERVE", "measure")
	s := defaultSettings()
	var m meter
	if _, got, err := m.look(s, time.Now()); err != nil || got {
		t.Fatalf("the first look only sets the counters: got %v, err %v", got, err)
	}
	// Burn one processor so there is something to see.
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
		}
	}()
	time.Sleep(1200 * time.Millisecond)
	r, got, err := m.look(s, time.Now())
	close(stop)
	if err != nil || !got {
		t.Fatalf("second look: got %v, err %v", got, err)
	}
	if r.Processors < 1 || r.Capacity < 1 || r.Capacity > float64(r.Processors) {
		t.Fatalf("processors %d, capacity %.0f", r.Processors, r.Capacity)
	}
	if total := r.BusyShort + r.UserBusy; total < 0.5 || total > float64(r.Processors)+0.01 {
		t.Fatalf("%.2f processors busy on a PC of %d while this test burns one", total, r.Processors)
	}
	if r.MemTotalGB < 1 || r.MemAvailGB <= 0 || r.MemAvailGB > r.MemTotalGB {
		t.Fatalf("memory %.1f of %.1f GB", r.MemAvailGB, r.MemTotalGB)
	}
	if r.DiskFreeGB <= 0 {
		t.Fatalf("disk free %.1f GB on %s (%v)", r.DiskFreeGB, r.DiskPath, r.Notes)
	}
	if r.Presence.State == "" || r.Presence.State == "none" {
		t.Fatalf("presence %+v", r.Presence)
	}
	if m.perCore < 1 || m.perCore > 8 {
		t.Fatalf("%d logical processors a core", m.perCore)
	}
	// This test's own processes are found among the programs or the others.
	me := 0.0
	for _, p := range append(r.Programs, r.Others...) {
		if p.Name == "fairshare.test.exe" {
			me = p.Cores
		}
	}
	if me < 0.5 {
		t.Fatalf("this test burned a processor and the look saw %.2f for it: %+v %+v", me, r.Programs, r.Others)
	}
	if _, err := os.Stat(appsFile()); err == nil && r.Presence.State != "full-screen" {
		t.Fatal("apps.json written with nothing full screen")
	}
}
