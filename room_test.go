package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestHeavyRoom(t *testing.T) {
	idle := pcLoad{CPUs: 24, CPUPct: 30, AvailMB: 30 << 10}
	light := heavyNeed{Cores: 0.6, PeakMB: 8800}   // owl-go check.mjs: 1,526 CPU-s over 2,434 s
	allCores := heavyNeed{Cores: 11, PeakMB: 8400} // appforgutters verify: 1,384 CPU-s over 129 s
	settled := time.Minute
	cases := []struct {
		name   string
		held   int
		newest time.Duration
		load   pcLoad
		need   heavyNeed
		ok     bool
		why    string
	}{
		{"the first two always run, even on a full PC", 1, 0, pcLoad{CPUs: 24, CPUPct: 100, FullScreen: "game.exe"}, allCores, true, ""},
		{"a third fits an idle PC", 2, settled, idle, light, true, ""},
		{"never more than the most", maxHeavySlots, settled, idle, light, false, "the most at once"},
		{"never while a game fills the screen", 2, settled, pcLoad{CPUs: 24, CPUPct: 10, AvailMB: 40 << 10, FullScreen: "7daystodie.exe"}, light, false, "7daystodie.exe is full screen"},
		{"waits while the newest job starts up", 2, 10 * time.Second, idle, light, false, "does not show yet"},
		{"a busy PC has no room", 2, settled, pcLoad{CPUs: 24, CPUPct: 95, AvailMB: 30 << 10}, light, false, "95% CPU"},
		{"an all-core job needs its cores idle", 2, settled, pcLoad{CPUs: 24, CPUPct: 50, AvailMB: 30 << 10}, allCores, false, "11.0 cores"},
		{"its peak memory must fit with a reserve", 2, settled, pcLoad{CPUs: 24, CPUPct: 20, AvailMB: 12 << 10}, light, false, "GB of memory is free"},
	}
	for _, c := range cases {
		ok, why := heavyRoom(c.held, c.newest, c.load, c.need)
		if ok != c.ok || !strings.Contains(why, c.why) {
			t.Errorf("%s: got %v %q", c.name, ok, why)
		}
	}
}

// Past the first two, a waiter takes a slot only when the pool's room rule says the PC has room,
// and says why it waits.
func TestRoomGatesSlotsPastTheMinimum(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("FAIRSHARE_HEAVY_SLOTS", "")
	t.Setenv(heldEnv, "")
	room := false
	pool := heavyPool()
	pool.room = func(held []slotHolder) (bool, string) {
		if len(held) < minHeavySlots || room {
			return true, ""
		}
		return false, "the PC is at 95% CPU"
	}
	if pool.count != maxHeavySlots {
		t.Fatalf("pool has %d lock files, want %d", pool.count, maxHeavySlots)
	}
	a, err := pool.acquire("a", "C:/a", "", time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := pool.acquire("b", "C:/b", "", time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var said string
	_, err = pool.acquire("c", "C:/c", "", 1500*time.Millisecond, func(m string) { said = m })
	if !errors.Is(err, errBusy) || !strings.Contains(err.Error(), "95% CPU") {
		t.Fatal("a third slot was given without room, or the refusal hid why:", err)
	}
	if !strings.Contains(said, "`a` in a") || !strings.Contains(said, "It waits because the PC is at 95% CPU") {
		t.Error("the wait note should name the holders and the reason:", said)
	}
	room = true
	c, err := pool.acquire("c", "C:/c", "", time.Second, nil)
	if err != nil {
		t.Fatal("a third slot was refused with room:", err)
	}
	defer c.Close()
	if c.Beside != 2 {
		t.Errorf("granted beside %d holders, want 2", c.Beside)
	}
}

// FAIRSHARE_HEAVY_SLOTS fixes the count and turns the room rule off.
func TestFixedSlotsOverride(t *testing.T) {
	t.Setenv("FAIRSHARE_HEAVY_SLOTS", "3")
	if p := heavyPool(); p.count != 3 || p.room != nil {
		t.Errorf("override gave %d slots, room rule set: %v", p.count, p.room != nil)
	}
}

func TestFullScreenAppDoesNotFail(t *testing.T) {
	t.Logf("full screen now: %q", fullScreenApp()) // a desktop session or a service with none: either way, no crash
}
