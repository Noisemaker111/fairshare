package main

import (
	"strings"
	"testing"
	"time"
)

// pc is Jon's PC: 24 logical processors, 8 kept for him, 64 GB.
func pc(demandShort, demandLong, memAvail, disk float64, state string) Reading {
	return Reading{Machine: "pc", OS: "windows", Processors: 24, Reserved: 8, Capacity: 16, ShortSeconds: 60, LongSeconds: 600,
		BusyShort: min(demandShort, 16), WaitingShort: max(0, demandShort-16), DemandShort: min(demandShort, 16),
		BusyLong: min(demandLong, 16), WaitingLong: max(0, demandLong-16), DemandLong: min(demandLong, 16),
		MemTotalGB: 64, MemAvailGB: memAvail, WorkMemGB: 64 - memAvail - 10, DiskPath: `C:\Users\Jk101`, DiskFreeGB: disk,
		Presence: Presence{State: state}}
}

func TestRoomFollowsWhatWorkTakesNotHowManyThreads(t *testing.T) {
	s := defaultSettings()
	cases := []struct {
		name    string
		r       Reading
		ask     Ask
		pending Need
		ok      bool
		says    string
	}{
		// The night of 2026-11-10: seven open threads, five of them building Rust. The count cap
		// of seven let them all in.
		{"seven threads with five Rust builds is full", pc(31, 22, 30, 30, "active"), Ask{Threads: 7, Disk: true}, Need{}, false, "using 16.0 of the 16 processors agent work may use, over the last minute (and 15 more threads waited"},
		// The same seven threads waiting on their models: the cap of seven refused an eighth.
		{"seven threads waiting on a model leave room", pc(1.5, 2.5, 40, 60, "active"), Ask{Threads: 7, Disk: true}, Need{}, true, "has room"},
		{"fifteen idle threads still leave room", pc(2, 3, 30, 60, "active"), Ask{Threads: 15, Disk: true}, Need{}, true, "has room"},
		// A calm minute inside a busy ten minutes does not let a thread in, nor the reverse.
		{"a calm minute in a busy stretch is still busy", pc(2, 15.5, 40, 60, "active"), Ask{Threads: 5}, Need{}, false, "last 10 minutes"},
		{"a busy minute after a calm stretch is busy", pc(15.5, 2, 40, 60, "active"), Ask{Threads: 5}, Need{}, false, "processors"},
		// 13 of 16 wanted: fits under 90% (14.4) with one more at the floor of 1, not under the
		// full-screen 75% (12).
		{"room at the desk", pc(13, 13, 40, 60, "active"), Ask{}, Need{}, true, "has room"},
		{"no room while a game fills the screen", pc(13, 13, 40, 60, "full-screen"), Ask{}, Need{}, false, "is full screen"},
		{"away fills the processors", pc(14.5, 14.5, 40, 60, "away"), Ask{}, Need{}, true, "has room"},
		{"memory reserve holds", pc(2, 2, 9, 60, "active"), Ask{}, Need{}, false, "8.0 GB is kept free"},
		{"disk reserve holds for a checkout", pc(2, 2, 40, 18, "active"), Ask{Disk: true}, Need{}, false, "15 GB is kept free"},
		{"disk is not checked without a checkout", pc(2, 2, 40, 6.9, "active"), Ask{}, Need{}, true, "has room"},
		{"threads just let in still count", pc(4, 4, 40, 60, "active"), Ask{}, Need{Cores: 10}, false, "count as 10.0 more"},
		{"a batch counts each thread", pc(8, 8, 40, 60, "active"), Ask{Count: 7}, Need{}, false, "7 new threads"},
		{"an explicit need replaces the estimate", pc(8, 8, 40, 60, "active"), Ask{Explicit: Need{Cores: 8}}, Need{}, false, "counts as 8.0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := decide(c.r, s, c.ask, c.pending)
			if v.OK != c.ok {
				t.Fatalf("ok = %v, want %v: %s", v.OK, c.ok, v.Reason)
			}
			if !strings.Contains(v.Reason, c.says) {
				t.Fatalf("reason lacks %q: %s", c.says, v.Reason)
			}
		})
	}
}

func TestANewThreadCountsAsTheMachinesOwnAverageButNeverUnderTheFloor(t *testing.T) {
	s := defaultSettings()
	// 12 processors wanted by 4 threads: 3 each. One more would need 15 of a 14.4 limit.
	if v := decide(pc(12, 12, 40, 60, "active"), s, Ask{Threads: 4}, Need{}); v.OK || v.CPU.Need != 3 {
		t.Fatalf("4 heavy threads: ok %v need %.1f, want refused at 3.0", v.OK, v.CPU.Need)
	}
	// The same 12 from 20 threads is 0.6 each, counted at the floor of 1: 13 fits.
	if v := decide(pc(12, 12, 40, 60, "active"), s, Ask{Threads: 20}, Need{}); !v.OK || v.CPU.Need != 1 {
		t.Fatalf("20 light threads: ok %v need %.1f, want room at 1.0", v.OK, v.CPU.Need)
	}
	// One thread burning the whole machine does not make the next one count as all of it.
	if v := decide(pc(40, 40, 40, 60, "active"), s, Ask{Threads: 1}, Need{}); v.CPU.Need != 4 {
		t.Fatalf("need %.1f, want the cap of a quarter of 16", v.CPU.Need)
	}
}

func TestALinuxHostIsJudgedByTheSameRule(t *testing.T) {
	s := defaultSettings()
	host := func(load, memAvail float64) Reading {
		r, err := linuxReading(linuxFiles{Loadavg: formatLoad(load), Meminfo: formatMem(16, memAvail)}, 8)
		if err != nil {
			t.Fatal(err)
		}
		r.Machine, r.DiskFreeGB = "t3-host", 80
		return r
	}
	// The 2026-11-09 host: nine threads, load 15 to 24 on 8 processors, out of memory.
	if v := decide(host(18, 1.5), s, Ask{Threads: 9}, Need{}); v.OK || v.CPU.OK || v.Mem.OK {
		t.Fatalf("an overloaded host was given room: %s", v.Reason)
	}
	// Three threads at 3 GB each on 16 GB: a fourth fits, a sixth would not.
	if v := decide(host(3, 7), s, Ask{Threads: 3}, Need{}); !v.OK {
		t.Fatalf("a host with 3 threads has room: %s", v.Reason)
	}
	if v := decide(host(5, 2.5), s, Ask{Threads: 5}, Need{}); v.OK || v.Mem.OK {
		t.Fatalf("a host with 2.5 GB free was given room: %s", v.Reason)
	}
}

func TestGrantsCountOnlyForTheSettleTime(t *testing.T) {
	t.Setenv("FAIRSHARE_HOME", t.TempDir())
	now := time.Date(2026, 11, 10, 21, 0, 0, 0, time.UTC)
	settle := 5 * time.Minute
	for i := range 3 {
		if err := takeGrant(now.Add(time.Duration(i)*time.Minute), settle, Need{Cores: 1, MemGB: 2}, "t"); err != nil {
			t.Fatal(err)
		}
	}
	if n := pendingNeed(now.Add(3*time.Minute), settle); n.Cores != 3 || n.MemGB != 6 {
		t.Fatalf("pending %+v, want 3 processors and 6 GB", n)
	}
	if n := pendingNeed(now.Add(6*time.Minute+time.Second), settle); n.Cores != 1 {
		t.Fatalf("pending %+v, want only the newest grant", n)
	}
	if n := pendingNeed(now.Add(time.Hour), settle); n.Cores != 0 {
		t.Fatalf("pending %+v, want none", n)
	}
}

func TestSettingsRefuseANumberOutOfRangeInsteadOfUsingIt(t *testing.T) {
	s := defaultSettings()
	if err := s.check(); err != nil {
		t.Fatalf("the defaults fail their own check: %v", err)
	}
	s.CPUTarget = 7 // a thread count where a share belongs
	if err := s.check(); err == nil || !strings.Contains(err.Error(), "share") {
		t.Fatalf("cpu_target 7 passed: %v", err)
	}
}

// 2026-11-11: launches were refused for 30 minutes at "13.1 in use, 38.1 waiting for one" of 22
// processors. The 38 were threads caught ready at the instant of a look: bursts of compiler jobs,
// and one program's four at every look with half the PC idle.
func TestRoomIsProcessorsInUseNotThreadsCaughtReady(t *testing.T) {
	if w := waitingWhenFull(38.1, 13.1, 22); w != 0 {
		t.Fatalf("38 ready threads beside 9 idle processors count as %.1f waiting, want 0", w)
	}
	if w := waitingWhenFull(26, 21.5, 22); w != 26 {
		t.Fatalf("26 ready threads with the processors full count as %.1f waiting, want 26", w)
	}
	if w := waitingWhenFull(3, 0, 0); w != 0 {
		t.Fatalf("no processors to be full: %.1f waiting, want 0", w)
	}
	s := defaultSettings()
	that := Reading{Machine: "pc", OS: "windows", Processors: 24, Reserved: 2, Capacity: 22, ShortSeconds: 60, LongSeconds: 540,
		BusyShort: 13.1, DemandShort: 13.1, BusyLong: 11.8, DemandLong: 11.8, MemTotalGB: 64, MemAvailGB: 31.5, WorkMemGB: 20, DiskFreeGB: 66,
		Presence: Presence{State: "away"}}
	if v := decide(that, s, Ask{Threads: 12, Disk: true}, Need{}); !v.OK {
		t.Fatalf("13.1 of 22 processors in use left no room: %s", v.Reason)
	}
	// Two clean Rust builds on top keep every processor busy: that is short, and is refused.
	full := that
	full.BusyShort, full.DemandShort, full.WaitingShort = 21.7, 21.7, 26
	if v := decide(full, s, Ask{Threads: 12, Disk: true}, Need{}); v.OK || !strings.Contains(v.Reason, "26 more threads waited") {
		t.Fatalf("21.7 of 22 processors in use had room, or the reason lacks who waited: %s", v.Reason)
	}
}

// A Linux load average also counts tasks waiting on a disk or the network: a host read as "77
// wanted of 8" is judged by the processors its counters say were in use.
func TestALinuxHostIsJudgedByProcessorsInUseNotItsLoadAverage(t *testing.T) {
	s := defaultSettings()
	host := func(load float64, use cpuUse) Reading {
		r, err := linuxReading(linuxFiles{Loadavg: formatLoad(load), Meminfo: formatMem(16, 9), Use: &use}, 8)
		if err != nil {
			t.Fatal(err)
		}
		r.Machine, r.DiskFreeGB = "t3-host", 80
		return r
	}
	waiters := host(77, cpuUse{Short: 2.5, Long: 3, ShortSeconds: 60, LongSeconds: 600})
	if v := decide(waiters, s, Ask{Threads: 2}, Need{}); !v.OK || waiters.WaitingShort != 0 {
		t.Fatalf("a load of 77 with 2.5 of 8 processors in use left no room (waiting %.1f): %s", waiters.WaitingShort, v.Reason)
	}
	busy := host(77, cpuUse{Short: 7.9, Long: 7.5, ShortSeconds: 60, LongSeconds: 600})
	if v := decide(busy, s, Ask{Threads: 2}, Need{}); v.OK || busy.WaitingShort != 69 {
		t.Fatalf("8 of 8 processors in use had room, or waiting is %.1f, want 69: %s", busy.WaitingShort, v.Reason)
	}
	b, total, ok := parseProcStat("cpu  100 0 50 800 50 0 0 0 30 0\ncpu0 1 2 3 4\n")
	if !ok || b != 150 || total != 1000 {
		t.Fatalf("/proc/stat read as %.0f busy of %.0f (ok %v), want 150 of 1000: idle and waiting on a disk are not busy", b, total, ok)
	}
}
