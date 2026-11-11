package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func formatLoad(load float64) string {
	return fmt.Sprintf("%.2f %.2f %.2f 3/900 12345\n", load, load, load)
}

func formatMem(totalGB, availGB float64) string {
	return fmt.Sprintf("MemTotal:       %d kB\nMemFree:        1000 kB\nMemAvailable:   %d kB\n", int(totalGB*1024*1024), int(availGB*1024*1024))
}

// The reserve on a 24-processor PC with two logical processors a core, one look every 5 s.
func TestTheReserveFollowsWhatTheUsersProgramsUse(t *testing.T) {
	s := defaultSettings()
	var rv reserver
	now := time.Date(2026, 11, 10, 21, 0, 0, 0, time.UTC)
	step := func(cores, waiting float64, p Presence) (int, string) {
		now = now.Add(5 * time.Second)
		return rv.want(now, cores, waiting, p, s, 24, 2)
	}
	desk, away := Presence{State: "active"}, Presence{State: "away", IdleMinutes: 12}
	game := Presence{State: "full-screen", App: "7daystodie.exe"}

	// A light desk: a browser and a chat window, 0.8 processors. Not eight.
	var n int
	for range 12 {
		n, _ = step(0.8, 0, desk)
	}
	if n != 2 {
		t.Fatalf("light desk keeps %d, want 2", n)
	}
	// One burst to 3 processors raises it on that look, and it stays up for the window.
	if n, _ = step(3, 0, desk); n != 6 {
		t.Fatalf("after a burst to 3 it keeps %d, want 6 (3 x 1.5 = 4.5, in whole cores)", n)
	}
	if n, _ = step(0.5, 0, desk); n != 6 {
		t.Fatalf("one quiet look later it keeps %d, want still 6", n)
	}
	// Their programs waiting for a processor adds a core.
	step(3, 2, desk)
	if n, why := step(3, 2, desk); n != 8 || !strings.Contains(why, "waiting for a processor") {
		t.Fatalf("with their threads waiting it keeps %d (%s), want 8", n, why)
	}
	// A game takes the foreground and uses 5: ten processors, more than the old fixed eight.
	for range 6 {
		n, _ = step(5, 0, game)
	}
	if n != 10 {
		t.Fatalf("a game using 5 keeps %d, want 10", n)
	}
	// Away: once the window has gone quiet, nearly the whole PC is the agents'.
	for range 40 {
		n, _ = step(0.1, 0, away)
	}
	if n != 2 {
		t.Fatalf("away keeps %d, want the least, 2", n)
	}
	// The first key press puts back what the desk needed, before any load shows.
	if n, _ = step(0.2, 0, desk); n < 6 {
		t.Fatalf("back at the desk it keeps %d, want what the desk needed before", n)
	}
	// The game starts again: what it needed last time, on its first look.
	for range 40 {
		step(0.3, 0, desk)
	}
	if n, why := step(0.4, 0, game); n != 10 || !strings.Contains(why, "7daystodie.exe") {
		t.Fatalf("the game's first look keeps %d (%s), want the 10 it needed last time", n, why)
	}
	// Never more than half the machine, however hungry the foreground.
	for range 3 {
		n, _ = step(30, 0, game)
	}
	if n != 12 {
		t.Fatalf("a runaway foreground keeps %d, want the most, 12", n)
	}
}

func TestLinuxReadingUsesTheContainersOwnLimits(t *testing.T) {
	f := linuxFiles{
		Loadavg: "9.50 6.00 3.00 4/512 999\n",
		Meminfo: formatMem(64, 50), // the host's memory, as a container sees it
		// 16 GB limit, 13 GB charged of which 2 GB is file cache: 5 GB really free.
		CgroupMemMax: "17179869184\n", CgroupMemCurrent: "13958643712\n", CgroupMemStat: "anon 100\ninactive_file 1073741824\nactive_file 1073741824\n",
		CgroupCPUMax: "800000 100000\n",
		PressureCPU:  "some avg10=40.00 avg60=22.50 avg300=9.00 total=1\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n",
		Use:          &cpuUse{Short: 8, Long: 6, ShortSeconds: 60, LongSeconds: 300},
	}
	r, err := linuxReading(f, 32)
	if err != nil {
		t.Fatal(err)
	}
	if r.Capacity != 8 || r.MemTotalGB != 16 || r.MemAvailGB != 5 {
		t.Fatalf("capacity %.0f, memory %.0f GB, free %.1f GB; want 8, 16 and 5", r.Capacity, r.MemTotalGB, r.MemAvailGB)
	}
	if r.BusyShort != 8 || r.WaitingShort != 1.5 || r.DemandLong != 6 {
		t.Fatalf("busy %.1f waiting %.1f long demand %.1f; want 8, 1.5 and 6", r.BusyShort, r.WaitingShort, r.DemandLong)
	}
	if len(r.Notes) != 1 || !strings.Contains(r.Notes[0], "22%") && !strings.Contains(r.Notes[0], "23%") {
		t.Fatalf("notes %q, want the processor pressure", r.Notes)
	}
	// No cgroup limit ("max"): the machine's own numbers.
	plain, err := linuxReading(linuxFiles{Loadavg: formatLoad(1), Meminfo: formatMem(16, 9), CgroupMemMax: "max\n", CgroupCPUMax: "max 100000\n"}, 8)
	if err != nil || plain.Capacity != 8 || plain.MemAvailGB != 9 {
		t.Fatalf("plain host: %+v %v", plain, err)
	}
	if _, err := linuxReading(linuxFiles{Loadavg: "", Meminfo: formatMem(16, 9)}, 8); err == nil {
		t.Fatal("an unreadable loadavg gave a reading")
	}
}

// The night of 2026-11-10: ten heavy minutes, the meter restarted, then a calm look.
func TestARestartedMeterStillKnowsTheLastTenMinutes(t *testing.T) {
	now := time.Date(2026, 11, 11, 2, 31, 0, 0, time.UTC)
	var lines []historyLineRead
	for i := 12; i >= 1; i-- {
		lines = append(lines, historyLineRead{At: now.Add(-time.Duration(i) * time.Minute), Capacity: 20, Busy: 15, Waiting: 12, MemAvailGB: 30})
	}
	var w window
	w.seed(lines, now, 10*time.Minute)
	w.add(sample{at: now.Add(5 * time.Second), seconds: 5, busy: 3, memAvailGB: 31}, 10*time.Minute)
	busy, waiting, _, _, _, _, _, seconds := w.means(now.Add(5*time.Second), 10*time.Minute)
	if seconds < 500 || busy < 14 || waiting < 11 {
		t.Fatalf("after a restart the long window holds %.0f s at %.1f busy and %.1f waiting; want the ten heavy minutes", seconds, busy, waiting)
	}
	r := pc(3, busy+waiting, 31, 60, "active")
	r.Capacity = 20
	if v := decide(r, defaultSettings(), Ask{Threads: 7}, Need{}); v.OK {
		t.Fatalf("a calm look after ten heavy minutes was given room: %s", v.Reason)
	}
	// History older than the window, or from the future, is not load.
	var old window
	old.seed([]historyLineRead{{At: now.Add(-11 * time.Minute), Busy: 20}, {At: now.Add(time.Minute), Busy: 20}}, now, 10*time.Minute)
	if len(old.samples) != 0 {
		t.Fatalf("seeded %d looks from outside the window", len(old.samples))
	}
}

func TestWindowAveragesByTimeAndKeepsTheLeastMemory(t *testing.T) {
	var w window
	t0 := time.Date(2026, 11, 10, 21, 0, 0, 0, time.UTC)
	// Nine minutes at 2 processors, then one minute at 16 with 20 threads waiting.
	for i := 1; i <= 120; i++ {
		s := sample{at: t0.Add(time.Duration(i) * 5 * time.Second), seconds: 5, busy: 2, memAvailGB: 30, programs: map[string]float64{"node.exe": 5}}
		if i > 108 {
			s.busy, s.waiting, s.memAvailGB = 16, 20, 30-float64(i-108)
			s.programs = map[string]float64{"rustc.exe": 70}
		}
		w.add(s, 10*time.Minute)
	}
	now := t0.Add(10 * time.Minute)
	busy, waiting, _, _, mem, programs, _, seconds := w.means(now, time.Minute)
	if busy != 16 || waiting != 20 || mem != 18 || seconds != 60 {
		t.Fatalf("last minute: busy %.1f waiting %.1f least memory %.1f over %.0f s", busy, waiting, mem, seconds)
	}
	if top := topPrograms(programs, seconds, nil, nil, 3); len(top) != 1 || top[0].Name != "rustc.exe" || top[0].Cores != 14 {
		t.Fatalf("top programs %+v, want rustc at 14", top)
	}
	busy, waiting, _, _, _, _, _, seconds = w.means(now, 10*time.Minute)
	if got := round2(busy); got != 3.4 || round2(waiting) != 2 {
		t.Fatalf("ten minutes: busy %.2f waiting %.2f over %.0f s, want 3.4 and 2", got, waiting, seconds)
	}
	if len(w.samples) > 121 {
		t.Fatalf("the window kept %d looks", len(w.samples))
	}
}

// Owl's keeper takes reserved processors from agent processes and never gives them back; the
// meter does, for a process still on the set of a larger reserve, and for no other.
func TestOnlyAProcessLeftOnALargerReservesProcessorsIsWidened(t *testing.T) {
	cases := []struct {
		name     string
		mask     uint64
		reserved int
		want     bool
	}{
		{"held off 12 when the reserve is 4", 0xFFF000, 4, true},
		{"held off 6 when the reserve is 4", 0xFFFFC0, 4, true},
		{"already on the reserve's own set", 0xFFFFF0, 4, false},
		{"still has reserved processors: Owl's to move", 0xFFFFFF, 4, false},
		{"held off 6 when the reserve is 8: Owl's to move", 0xFFFFC0, 8, false},
		{"a set it chose for itself", 0x00F000, 4, false},
		{"one processor in the middle", 0x000100, 4, false},
	}
	for _, c := range cases {
		if got := staleReserveMask(c.mask, c.reserved, 24); got != c.want {
			t.Errorf("%s (%#x, reserve %d): widened %v, want %v", c.name, c.mask, c.reserved, got, c.want)
		}
	}
	if m := agentMask(4, 24); m != 0xFFFFF0 {
		t.Fatalf("the agent processors with 4 reserved of 24 are %#x, want 0xFFFFF0", m)
	}
}
