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
		{"threads just let in still count", pc(4, 4, 40, 60, "active"), Ask{}, Need{Cores: 10}, false, "not yet seen running counts as 10.0 more"},
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
		if err := takeGrant(now.Add(time.Duration(i)*time.Minute), settle, grant{Need: Need{Cores: 1, MemGB: 2}, Label: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	if n := pendingNeed(now.Add(3*time.Minute), settle, nil); n.Cores != 3 || n.MemGB != 6 {
		t.Fatalf("pending %+v, want 3 processors and 6 GB", n)
	}
	if n := pendingNeed(now.Add(6*time.Minute+time.Second), settle, nil); n.Cores != 1 {
		t.Fatalf("pending %+v, want only the newest grant", n)
	}
	if n := pendingNeed(now.Add(time.Hour), settle, nil); n.Cores != 0 {
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

// thread is one open thread as the meter reports it.
func thread(repo string, now, expect, swing float64) LiveWork {
	return LiveWork{Kind: "thread", Repo: repo, Now: now, Expect: expect, Swing: swing, MemGB: 1, ExpectMemGB: 1}
}

func TestOpenWorkCountsAsWhatItIsExpectedToUseNotWhatItUsesThisMinute(t *testing.T) {
	s := defaultSettings()
	// Six Owl threads between builds: 2 processors in use this minute and the last ten, where a
	// count by what is in use would let six more in. Each was measured at 2.2 give or take 1.
	quiet := pc(2, 2, 40, 60, "active")
	for range 6 {
		quiet.Work = append(quiet.Work, thread("owl-go", 0.2, 2.2, 1))
	}
	v := decide(quiet, s, Ask{}, Need{})
	// 0.8 of the 2 in use is no thread's; 13.2 expected; the six swing together by sqrt(6) = 2.4.
	if !near(v.Open.Rest, 0.8) || !near(v.Open.Expect, 13.2) || !near(v.Open.Swing, 2.45) || !near(v.CPU.Used, 16.45) || v.OK {
		t.Fatalf("six quiet threads expected to build: used %.2f, open %+v: %s", v.CPU.Used, v.Open, v.Reason)
	}
	if !strings.Contains(v.Reason, "the work open here is expected to use 14.4 more than it does now") ||
		!strings.Contains(v.Reason, "The 6 pieces of work open here use 1.2 this minute and are expected to use 13.2, give or take 2.5") {
		t.Fatalf("the refusal does not say what the open work counts as: %s", v.Reason)
	}
	// The same six threads measured as waiting on their models leave the room they really leave.
	for i := range quiet.Work {
		quiet.Work[i] = thread("appforgutters", 0.2, 0.3, 0.2)
	}
	if v := decide(quiet, s, Ask{}, Need{}); !v.OK || !near(v.CPU.Used, 3.09) {
		t.Fatalf("six threads on their models: used %.2f: %s", v.CPU.Used, v.Reason)
	}
	// What is in use is never counted as less than it is: threads busier than expected count as they are.
	busy := pc(12, 9, 40, 60, "active")
	busy.Work = []LiveWork{thread("owl-go", 11, 4, 1)}
	if v := decide(busy, s, Ask{}, Need{}); !near(v.CPU.Used, 12) {
		t.Fatalf("a thread busier than expected: used %.2f", v.CPU.Used)
	}
}

func TestRoomIsKeptForTheSwingOfTheWorkTogetherNotForEveryPeak(t *testing.T) {
	s := defaultSettings()
	idle := pc(0, 0, 40, 60, "active")
	steady := []level{{Name: "owl-go threads here", Minutes: 600, Cores: 4, MeanSq: 16, MemGB: 4}}
	bursty := []level{{Name: "appforgutters threads here", Minutes: 600, Cores: 1, MeanSq: 10, MemGB: 2}}
	// A steady thread needs its average; a thread that averages 1 in bursts of far more needs
	// its average and its swing of 3.
	if v := decide(idle, s, Ask{Learned: steady}, Need{}); v.CPU.Need < 3.9 || v.CPU.Need > 4.4 || v.Sized.Swing > 0.4 {
		t.Fatalf("a steady thread: need %.2f, %+v", v.CPU.Need, v.Sized)
	}
	if v := decide(idle, s, Ask{Learned: bursty}, Need{}); !near(v.CPU.Need, 3.98) || !near(v.Sized.Cores, 1) || !near(v.Sized.Swing, 2.98) {
		t.Fatalf("a bursty thread: need %.2f, %+v", v.CPU.Need, v.Sized)
	}
	// Ten bursty threads open do not all burst in one minute: together they count as 10 and a
	// swing of 9.5, not as ten peaks, and an eleventh adds 1.4, not 4.
	for range 10 {
		idle.Work = append(idle.Work, thread("appforgutters", 0, 1, 3))
	}
	v := decide(idle, s, Ask{Learned: bursty}, Need{})
	if !near(v.CPU.Used, 19.49) || v.CPU.Need < 1.4 || v.CPU.Need > 1.5 {
		t.Fatalf("an eleventh bursty thread: used %.2f need %.2f", v.CPU.Used, v.CPU.Need)
	}
	// learn_margin 0 keeps no room for swing at all.
	s.LearnMargin = 0
	if v := decide(idle, s, Ask{Learned: bursty}, Need{}); !near(v.CPU.Used, 10) || !near(v.CPU.Need, 1) {
		t.Fatalf("with no margin: used %.2f need %.2f", v.CPU.Used, v.CPU.Need)
	}
	// A run says so, and three of it count three times.
	s = defaultSettings()
	gate := []level{{Name: "`bun run gate` in appforgutters here", Minutes: 60, Cores: 5.2, MeanSq: 27.04, MemGB: 6, Seconds: 300, Own: true}}
	v = decide(pc(13, 13, 40, 60, "active"), s, Ask{Kind: "run", Learned: gate}, Need{})
	if v.OK || !strings.Contains(v.Reason, "has no room for this run") || !strings.Contains(v.Reason, "`bun run gate` in appforgutters here was measured at 5.2 processors and 6.0 GB over 60 minutes (a run lasts about 5 minutes)") {
		t.Fatalf("a gate on a busy PC: %s", v.Reason)
	}
	// Three of it: three times its 4.6 and the swing of the three together (an hour of it is
	// not yet all that is known, so each still swings 1.5), and three times its memory.
	if v := decide(pc(1, 1, 40, 60, "active"), s, Ask{Kind: "run", Count: 3, Learned: gate}, Need{}); !near(v.CPU.Need, 16.35) || !near(v.Mem.Need, 3*5.43) {
		t.Fatalf("three gates: %.2f processors, %.2f GB", v.CPU.Need, v.Mem.Need)
	}
}

func TestAPieceOfWorkIsNeverCountedAsMoreThanTheMachine(t *testing.T) {
	s := defaultSettings()
	// A gate measured on the 24-processor PC, told to an 8-processor host with 16 GB.
	host, err := linuxReading(linuxFiles{Loadavg: formatLoad(0.5), Meminfo: formatMem(16, 14)}, 8)
	if err != nil {
		t.Fatal(err)
	}
	pcSays := []level{{Name: "`cargo test` in owl-go on pc", Minutes: 300, Cores: 17, MeanSq: 300, MemGB: 20}}
	v := decide(host, s, Ask{Kind: "run", Learned: pcSays}, Need{})
	if v.Sized.Cores != 8 || v.Sized.MemGB != 8 || v.CPU.Need > 8+1e-9 {
		t.Fatalf("counted as %.1f processors and %.1f GB on a machine of 8 and 16", v.Sized.Cores, v.Sized.MemGB)
	}
}

func TestWorkLetInCountsOnlyUntilItIsSeenRunning(t *testing.T) {
	at := time.Date(2026, 11, 11, 18, 0, 0, 0, time.UTC)
	grants := []grant{
		{At: at, Need: Need{Cores: 4.5, MemGB: 6}, Repo: "Owl-Go"},
		{At: at.Add(time.Minute), Need: Need{Cores: 2, MemGB: 4}, Repo: "appforgutters", Count: 2},
		{At: at.Add(2 * time.Minute), Need: Need{Cores: 5.2}, Kind: "run", Repo: "appforgutters"},
	}
	open := func(kind, repo string, since time.Time) LiveWork {
		return LiveWork{Kind: kind, Repo: repo, Since: since}
	}
	sum := func(work ...LiveWork) float64 {
		total := 0.0
		for _, g := range unmet(grants, work) {
			total += g.Need.Cores
		}
		return total
	}
	if got := sum(); got != 11.7 {
		t.Fatalf("nothing running yet: %.1f, want all 11.7", got)
	}
	// Threads that were open before the grants are not the threads that were let in.
	if got := sum(open("thread", "owl-go", at.Add(-time.Hour)), open("thread", "appforgutters", at.Add(-time.Minute))); got != 11.7 {
		t.Fatalf("older threads answered for new grants: %.1f", got)
	}
	// The Owl thread shows up: its grant ends, and it counts as itself from here.
	if got := sum(open("thread", "owl-go", at.Add(20*time.Second))); !near(got, 7.2) {
		t.Fatalf("after the Owl thread started: %.1f, want 7.2", got)
	}
	// One of the two AppForGutters threads shows up: half of their grant is left. A thread does
	// not answer for a run, nor a thread of another repo for this one.
	if got := sum(open("thread", "appforgutters", at.Add(90*time.Second)), open("thread", "fairshare", at.Add(3*time.Minute))); !near(got, 4.5+1+5.2) {
		t.Fatalf("one of two arrived: %.1f, want 10.7", got)
	}
	// One piece of work does not answer for two grants.
	both := []grant{{At: at, Need: Need{Cores: 1}}, {At: at, Need: Need{Cores: 1}}}
	if left := unmet(both, []LiveWork{open("thread", "anything", at.Add(time.Second))}); len(left) != 1 {
		t.Fatalf("one thread answered for %d of two grants", 2-len(left))
	}
	// The run starts: all three are running, nothing is pending.
	if got := sum(open("thread", "owl-go", at.Add(20*time.Second)), open("thread", "appforgutters", at.Add(90*time.Second)),
		open("thread", "appforgutters", at.Add(100*time.Second)), open("run", "appforgutters", at.Add(121*time.Second))); got != 0 {
		t.Fatalf("everything running: %.1f still pending", got)
	}
}

func TestMemoryIsKeptForWhatOpenWorkIsExpectedToReach(t *testing.T) {
	s := defaultSettings()
	// A 16 GB host with 9 GB free and three threads that hold 0.3 GB now; their gates take 3.
	host, err := linuxReading(linuxFiles{Loadavg: formatLoad(0.5), Meminfo: formatMem(16, 9)}, 8)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		host.Work = append(host.Work, LiveWork{Kind: "thread", Repo: "appforgutters", Expect: 0.5, MemGB: 0.3, ExpectMemGB: 3})
	}
	v := decide(host, s, Ask{}, Need{})
	if !near(v.Mem.Pending, 8.1) || v.Mem.OK || !strings.Contains(v.Reason, "on top of 8.1 GB that work let in or open here is still expected to take") {
		t.Fatalf("memory pending %.1f, ok %v: %s", v.Mem.Pending, v.Mem.OK, v.Reason)
	}
	// Threads that already hold what they are expected to reach owe nothing more.
	for i := range host.Work {
		host.Work[i].MemGB = 3
	}
	if v := decide(host, s, Ask{}, Need{}); v.Mem.Pending != 0 || !v.Mem.OK {
		t.Fatalf("threads at their expected memory: pending %.1f, ok %v", v.Mem.Pending, v.Mem.OK)
	}
}
