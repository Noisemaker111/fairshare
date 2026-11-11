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
func TestTheReserveIsTwoUnlessAGameRuns(t *testing.T) {
	s := defaultSettings()
	var rv reserver
	now := time.Date(2026, 11, 10, 21, 0, 0, 0, time.UTC)
	step := func(game string, cores, waiting float64, p Presence) (int, string) {
		now = now.Add(5 * time.Second)
		return rv.want(now, game, cores, waiting, p, s, 24, 2)
	}
	desk, away := Presence{State: "active"}, Presence{State: "away", IdleMinutes: 12}
	full := Presence{State: "full-screen", App: "7DaysToDie.exe"}

	// 2026-11-11: nobody at the PC, Windows' own work (dwm, Defender, the indexer) at 3.7
	// processors. It is not Jon: 2 kept, not 6.
	var n int
	var why string
	for range 12 {
		n, why = step("", 0, 0, away)
	}
	if n != 2 || !strings.Contains(why, "no game running") {
		t.Fatalf("no game, away: keeps %d (%s), want 2", n, why)
	}
	// At the desk, busy browser and all, still 2: the reserve matters only for games.
	for range 6 {
		n, _ = step("", 0, 3, desk)
	}
	if n != 2 {
		t.Fatalf("no game at the desk keeps %d, want 2", n)
	}
	// A browser playing a video full screen is not a game.
	if n, why = step("", 0, 0, Presence{State: "full-screen", App: "chrome.exe"}); n != 2 || !strings.Contains(why, "not a game") {
		t.Fatalf("a full-screen browser keeps %d (%s), want 2", n, why)
	}
	// A game full screen using 5: game-sized, 10.
	for range 6 {
		n, _ = step("", 5, 0, full)
	}
	if n != 10 {
		t.Fatalf("a game using 5 keeps %d, want 10", n)
	}
	// Its threads waiting for a processor add a core.
	step("", 5, 2, full)
	if n, why = step("", 5, 2, full); n != 12 || !strings.Contains(why, "waiting for a processor") {
		t.Fatalf("with its threads waiting it keeps %d (%s), want 12", n, why)
	}
	// Game closed: back to 2 at once.
	if n, _ = step("", 0, 0, desk); n != 2 {
		t.Fatalf("after the game it keeps %d, want 2", n)
	}
	// The game running again, alt-tabbed (not full screen): it is known, so game-sized from its
	// first look, at what it needed last time.
	if !rv.isGame("7daystodie.exe", s) || rv.isGame("chrome.exe", s) {
		t.Fatal("the game was not learned, or the browser was")
	}
	if n, why = step("7daystodie.exe", 0.4, 0, desk); n != 10 || !strings.Contains(why, "7daystodie.exe") {
		t.Fatalf("the game's first look keeps %d (%s), want the 10 it needed last time", n, why)
	}
	// Never more than half the machine, however hungry the game.
	for range 3 {
		n, _ = step("7daystodie.exe", 30, 0, desk)
	}
	if n != 12 {
		t.Fatalf("a runaway game keeps %d, want the most, 12", n)
	}
	// The least is a setting.
	s.ReserveMinProcessors = 4
	if n, _ = step("", 0, 0, desk); n != 4 {
		t.Fatalf("reserve_min_processors 4 keeps %d", n)
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

func TestALinuxProcessIsReadWithItsEndedChildrensTime(t *testing.T) {
	// A name with spaces and brackets, as vitest sets it.
	p, ok := parsePidStat("103875 (node (vitest) 2) S 103852 103852 103852 0 -1 4194560 100 200 0 0 1500 250 4000 750 20 0 11 0 12345678 9000000 65536 18446744073709551615 1 1 0 0 0 0 0 0 0 0 0 0 17 3 0 0 0 0 0")
	if !ok || p.pid != 103875 || p.parent != 103852 || p.name != "node (vitest) 2" || p.ticks != 6500 || p.start != 12345678 || p.pages != 65536 {
		t.Fatalf("read %+v %v", p, ok)
	}
	for _, bad := range []string{"", "12 (x", "12 (x) S 1 2", "x (y) S 1 1 1 0 -1 0 0 0 0 0 1 1 1 1 0 0 0 0 5 0 7"} {
		if _, ok := parsePidStat(bad); ok {
			t.Errorf("%q was read as a process", bad)
		}
	}
	if boot, ok := parseBootTime("cpu  1 2 3\nbtime 1794400000\nprocesses 5\n"); !ok || boot != 1794400000 {
		t.Fatalf("boot time %v %v", boot, ok)
	}

	// Two looks. A build shell started compiler jobs that began and ended between them: their
	// 40 seconds are on the shell's waited-for time. A test worker that was alive at the first
	// look (30 s counted then) ended with 36: only its last 6 are new.
	shell, worker, root := procKey{10, 1}, procKey{20, 2}, procKey{1, 0}
	before := map[procKey]procSeen{root: {seconds: 5}, shell: {seconds: 2, parent: root}, worker: {seconds: 30, parent: shell}}
	now := map[procKey]procSeen{root: {seconds: 5}, shell: {seconds: 2 + 1 + 40 + 36, parent: root}}
	carry := map[procKey]float64{}
	used := usedSince(now, before, carry, false)
	if used[shell] != 47 || used[root] != 0 || len(carry) != 0 {
		t.Fatalf("the shell used %.0f, want its own 1, the jobs' 40 and the worker's last 6: %v", used[shell], used)
	}
	// The worker is gone before its time lands on the shell: nothing is counted twice, and the
	// difference is settled in the look in which it lands.
	early := map[procKey]procSeen{root: {seconds: 5}, shell: {seconds: 3, parent: root}}
	used = usedSince(early, before, carry, false)
	if used[shell] != 0 || carry[shell] != -29 {
		t.Fatalf("read early: used %.0f, carried %.0f", used[shell], carry[shell])
	}
	used = usedSince(now, early, carry, false)
	if used[shell] != 47 || len(carry) != 0 {
		t.Fatalf("settled a look later: used %.0f, carry %v", used[shell], carry)
	}
	// A whole branch that ended is taken off the nearest ancestor still alive.
	leaf := procKey{30, 3}
	before[leaf] = procSeen{seconds: 10, parent: worker}
	gone := map[procKey]procSeen{root: {seconds: 5 + 2 + 30 + 10 + 8}}
	if used = usedSince(gone, before, map[procKey]float64{}, false); used[root] != 8 {
		t.Fatalf("after the branch ended the root has %.0f new seconds, want 8", used[root])
	}
	// The first look counts nothing: there is nothing to measure it against.
	if used = usedSince(now, map[procKey]procSeen{}, map[procKey]float64{}, true); len(used) != 0 {
		t.Fatalf("a first look counted %v", used)
	}
}
