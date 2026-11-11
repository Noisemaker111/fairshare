package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// What a kind of work holds is its average, how far its minutes stray from that, and the most
// one minute held: three different numbers, and only the first is what a piece is expected to hold.
func TestMemoryIsLearnedAsAnAverageASwingAndAMost(t *testing.T) {
	s := defaultSettings()
	now := time.Date(2026, 11, 11, 18, 0, 0, 0, time.UTC)
	l := learned{}
	// A thread that waits on its model at 2 GB and, one minute in twenty, builds at 21.2.
	for i := range 100 {
		gb := 2.0
		if i%20 == 0 {
			gb = 21.2
		}
		l.add(now, stretch{Kind: "thread", Repo: "owl-go", Minutes: 1, Cores: 1, MemGB: gb}, s)
	}
	lv := l.levels("thread", "owl-go", "", nil, now, s)
	own := lv[len(lv)-1]
	if !near(own.MemGB, 2.96) || !near(spread(own.MemGB, own.memSq()), 4.18) || own.memMost() != 21.2 {
		t.Fatalf("average %.2f, swing %.2f, most %.2f; want 2.96, 4.18 and 21.2", own.MemGB, spread(own.MemGB, own.memSq()), own.memMost())
	}
	sz := size(Need{Cores: s.ThreadCores, MemGB: s.ThreadMemGB}, lv, s.LearnTrustMinutes)
	if sz.MemGB < 2.9 || sz.MemGB > 3 || sz.MemSwing < 4 || sz.MemSwing > 4.3 || sz.MemMost < 20.8 || sz.MemMost > 21.2 {
		t.Fatalf("a new Owl thread: %+v", sz)
	}
	if words := sizedWords(sz); !strings.Contains(words, "and 3.0 GB (21.2 at its most) over 100 minutes") {
		t.Fatalf("the words: %s", words)
	}
	row := l.table(now, s)[0]
	if !near(row.MemGB, 2.96) || !near(row.MemSwing, 4.18) || row.MemMost != 21.2 {
		t.Fatalf("`fairshare learned`: %+v", row)
	}
	// What this machine measured goes to the others whole, and is read back whole.
	told := measured(lv, "pc")
	back := learned{}.levels("thread", "owl-go", "", told, now, s)
	if told == nil || len(back) != 1 || !near(back[0].MemGB, 2.96) || !near(spread(back[0].MemGB, back[0].memSq()), 4.18) || back[0].memMost() != 21.2 {
		t.Fatalf("told %+v, read back %+v", told, back)
	}

	// A file written before the squares were kept: its average and its most stand, and how far
	// its minutes strayed is not known, so it is nothing rather than everything.
	t.Setenv("FAIRSHARE_HOME", t.TempDir())
	b, _ := json.Marshal(map[string]any{"thread||": map[string]any{"at": time.Now().UTC(), "minutes": 1900, "cores": 1900, "sq": 3800, "mem_gb": 5700, "mem_peak": 21.2}})
	if err := os.WriteFile(filepath.Join(homeDir(), "learned.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	old := loadLearned()
	got := size(Need{Cores: 1, MemGB: 2}, old.levels("thread", "", "", nil, time.Now(), s), s.LearnTrustMinutes)
	if got.MemGB < 2.9 || got.MemGB > 3.01 || got.MemSwing > 0.1 || got.MemMost < 21 {
		t.Fatalf("an old file: %+v", got)
	}
	// And the minutes measured after it are learned with their squares on top of it.
	for range 1900 {
		old.add(time.Now(), stretch{Kind: "thread", Minutes: 1, MemGB: 7}, s)
	}
	if p := old[learnKey("thread", "", "")]; !near(p.mem(), 5) || !near(spread(p.mem(), p.memSq()), 2) {
		t.Fatalf("after as many minutes at 7 GB: average %.2f, swing %.2f; want 5 and 2", p.mem(), spread(p.mem(), p.memSq()))
	}
}

// The room kept above what open work is expected to hold: the furthest one piece reaches, always;
// every piece at its most, never more; between them, the swing of the pieces taken together.
func TestRoomIsKeptOnceForOpenWorkToHoldMoreNotOncePerPiece(t *testing.T) {
	load := func(pieces int, expect, swing, most float64) memLoad {
		var m memLoad
		for range pieces {
			m.add(expect, expect, swing, most, 1)
		}
		return m
	}
	// One thread that builds at 21 will build: all of its reach is kept, whatever its swing says.
	if got := load(1, 3, 4, 21).kept(2, 0); got != 18 {
		t.Fatalf("one piece: %.1f kept, want its whole reach of 18", got)
	}
	// Eight of them do not build in the same minute: 22.6 is kept (twice their swing together),
	// which is one at its most and a second well on its way, and not eight times 18.
	if got := load(8, 3, 4, 21).kept(2, 0); !near(got, 2*4*math.Sqrt(8)) || got > 144/6 {
		t.Fatalf("eight pieces: %.1f kept, want 22.6", got)
	}
	// Thirty-two of them: 45 is kept, where every piece at its most would be 576.
	if got := load(32, 3, 4, 21).kept(2, 0); !near(got, 2*4*math.Sqrt(32)) {
		t.Fatalf("thirty-two pieces: %.1f kept, want 45.3", got)
	}
	// Steady work that is nearly always at its most: its swing is wide of its reach, and no
	// more is kept than all of it at its most.
	if got := load(4, 9, 3, 10).kept(2, 0); got != 4 {
		t.Fatalf("four pieces that reach 1 above their average: %.1f kept, want 4", got)
	}
	// learn_mem_margin 0 keeps nothing for chance: only what is certain, one piece's reach.
	if got := load(8, 3, 4, 21).kept(0, 0); got != 18 {
		t.Fatalf("with no margin: %.1f kept, want 18", got)
	}
	// Three of one run started together reach their most together: three times one.
	var three memLoad
	three.add(0, 5, 1, 9, 3)
	if three.expect != 15 || three.kept(2, 0) != 12 {
		t.Fatalf("three started together: %.1f expected and %.1f kept, want 15 and 12", three.expect, three.kept(2, 0))
	}

	// The machine keeps a reserve free whatever the work does, and the rarest minute of one piece
	// lands in it first. With 8 GB kept free, the one thread that builds at 21 has 10 kept on top:
	// the 18 it reaches still fit in the machine, and are not kept twice.
	if got := load(1, 3, 4, 21).kept(2, 8); got != 10 {
		t.Fatalf("one piece on a machine that keeps 8 free: %.1f kept, want 10", got)
	}
	// The swing is every hour's ordinary up and down, not a rare minute: none of it is taken out
	// of the reserve. Eight of them keep the same 22.6.
	if got := load(8, 3, 4, 21).kept(2, 8); !near(got, 2*4*math.Sqrt(8)) {
		t.Fatalf("eight pieces on a machine that keeps 8 free: %.1f kept, want 22.6", got)
	}
	// A reserve wider than any reach keeps nothing less than the swing, and never less than nothing.
	if got := load(1, 3, 1, 5).kept(2, 8); got != 2 {
		t.Fatalf("one piece that reaches 2 on a machine that keeps 8 free: %.1f kept, want 2", got)
	}
	if got := load(1, 3, 0, 3).kept(2, 8); got != 0 {
		t.Fatalf("one steady piece: %.1f kept, want 0", got)
	}
}

// pcOfThatEvening is Jon's PC on 2026-11-11 as it was when it refused launches: 64 GB with 36
// free, and seven threads open that hold 0.5 to 6 GB each. Everything in it comes the way it does
// on the PC: the minutes are learned by learned.add and the open threads are followed by the
// tracker, look by look.
func pcOfThatEvening(t *testing.T) (Reading, learned, *looks) {
	t.Helper()
	l := newLooks()
	known := learned{}
	at := l.now
	// What had been measured: Owl threads wait at 1.5 GB and build Rust at up to 18.4, AppForGutters
	// threads hold about 1, and one thread of another repo once held 21.2 for a few minutes.
	for i := range 600 {
		gb := 1.5
		if i%10 == 0 {
			gb = 12 + float64(i%60)/60*6.4
		}
		if i == 10 {
			gb = 18.4
		}
		known.add(at, stretch{Kind: "thread", Repo: "owl-go", Minutes: 1, Cores: 2, MemGB: gb}, l.s)
		known.add(at, stretch{Kind: "thread", Repo: "appforgutters", Minutes: 1, Cores: 1, MemGB: 1}, l.s)
	}
	for i := range 300 {
		gb := 1.0
		if i < 5 {
			gb = 21.4 // an hour later, when the launch asks, it has faded to 21.2
		}
		known.add(at, stretch{Kind: "thread", Repo: "t3code", Minutes: 1, Cores: 1, MemGB: gb}, l.s)
	}
	thread := func(pid uint32, repo, tree string) procNode {
		l.where[pid] = [2]string{`C:\Users\Jk101\.t3\worktrees\` + repo + `\` + tree, ""}
		return procNode{pid: pid, parent: 1, created: uint64(pid), name: "claude.exe"}
	}
	a, b := thread(201, "appforgutters", "uploads"), thread(202, "appforgutters", "invoices")
	c, d := thread(203, "owl-go", "port-engine"), thread(204, "owl-go", "desktop-tests")
	e := thread(205, "fairshare", "memory")
	f, g := thread(206, "owl-go", "keeper"), thread(207, "owl-go", "frames")
	// An hour of the five older threads. One Owl thread built at 9 GB a while ago and is back at
	// 2; the other is in a build now and holds 6.
	for i := range 720 {
		a.memGB, b.memGB, c.memGB, d.memGB, e.memGB = 0.5, 0.8, 2, 1.5, 1.2
		if i >= 300 && i < 360 {
			c.memGB = 9
		}
		if i >= 690 {
			d.memGB = 6
		}
		nodes := []procNode{a, b, c, d, e}
		// The two youngest began three minutes ago and are still reading their briefs.
		if i >= 684 {
			f.memGB, g.memGB = 0.6, 0.5
			nodes = append(nodes, f, g)
		}
		for _, sp := range l.look(nodes...) {
			known.add(l.now, sp, l.s)
		}
	}
	r := pc(6, 6, 36, 60, "active")
	r.Work = l.t.live(l.now, known, l.s)
	return r, known, l
}

func TestThePCWith36GBFreeTakesAThread(t *testing.T) {
	s := defaultSettings()
	r, known, l := pcOfThatEvening(t)
	if len(r.Work) != 7 {
		t.Fatalf("%d pieces of work open, want seven threads", len(r.Work))
	}
	held := 0.0
	for _, w := range r.Work {
		held += w.MemGB
		if w.MemGB < 0.5 || w.MemGB > 6 {
			t.Fatalf("an open thread holds %.1f GB; that evening they held 0.5 to 6", w.MemGB)
		}
	}
	ask := Ask{Threads: 7, Learned: known.levels("thread", "gutter-quotes", "", nil, l.now, s)}
	v := decide(r, s, ask, Need{})

	// The rule that refused it: every open thread counted as the most it might reach (which is
	// what its expect_mem_gb was), added up, and the new thread as the most any thread ever held.
	owedBefore := 0.0
	for _, w := range r.Work {
		owedBefore += w.MemMost - w.MemGB
	}
	if before := v.Mem.Used + owedBefore + v.Sized.MemMost; before <= v.Mem.Limit || owedBefore < 33 || owedBefore > 40 || v.Sized.MemMost < 21 || v.Sized.MemMost > 21.2 {
		t.Fatalf("the old rule: a new thread at %.1f GB on top of %.1f GB still expected, %.1f of %.1f; that evening it said 21.1 on top of 36.5 and refused",
			v.Sized.MemMost, owedBefore, before, v.Mem.Limit)
	}

	// The same PC, the same threads, the same minutes: there is room.
	if !v.OK || !v.Mem.OK {
		t.Fatalf("36 of 64 GB free and seven threads holding %.1f GB: %s", held, v.Reason)
	}
	// The seven hold 11.6 GB and are expected to hold 11.7. Room for them to hold more is kept
	// once: 12.2 owed in all, where adding every thread's reach up made 36.9. The new thread is
	// expected to hold 1.7 and may reach 21.1. The 8 GB the PC keeps free takes the first of
	// that rare minute, so it adds 1.7 and the 1.2 that is still further than what is kept
	// already, and counts as 2.9: not as 21.1, and not as 9.0 either, which is what it counted
	// as while the reach was kept on top of the reserve.
	if !near(v.Open.MemGB, held) || v.Open.MemExpect < 11 || v.Open.MemExpect > 12.5 || v.Open.MemKept < 11.5 || v.Open.MemKept > 12.6 {
		t.Fatalf("the open work's memory: %+v", v.Open)
	}
	if v.Sized.MemGB < 1.5 || v.Sized.MemGB > 2 || v.Mem.Need < 2.5 || v.Mem.Need > 3.3 || v.Mem.Pending < 11.5 || v.Mem.Pending > 13 {
		t.Fatalf("a new thread: expected to hold %.1f, counted as %.1f on top of %.1f", v.Sized.MemGB, v.Mem.Need, v.Mem.Pending)
	}
	if total := v.Mem.Used + v.Mem.Pending + v.Mem.Need; total < 42 || total > 43.5 {
		t.Fatalf("counted as %.1f of the %.1f GB that may be taken", total, v.Mem.Limit)
	}
	t.Logf("before: 28.0 in use + %.1f owed + %.1f for the thread = %.1f of %.1f, refused", owedBefore, v.Sized.MemMost, 28+owedBefore+v.Sized.MemMost, v.Mem.Limit)
	t.Logf("now:    28.0 in use + %.1f owed + %.1f for the thread = %.1f of %.1f, let in", v.Mem.Pending, v.Mem.Need, 28+v.Mem.Pending+v.Mem.Need, v.Mem.Limit)

	// It is not let in on the average alone. With 22 GB free the seven's ordinary swing would
	// land in the memory that is kept free, and the answer says what was counted.
	tight := r
	tight.MemAvailGB = 22
	if v := decide(tight, s, ask, Need{}); v.OK || v.Mem.OK || !strings.Contains(v.Reason, "(never every piece at its most at once)") ||
		!strings.Contains(v.Reason, fmt.Sprintf("The 7 pieces of work open here hold %.1f GB and are expected to hold", held)) {
		t.Fatalf("22 GB free: %s", v.Reason)
	}
	// An AppForGutters thread, whose threads were never seen above 1 GB, still fits there.
	afg := Ask{Threads: 7, Learned: known.levels("thread", "appforgutters", "", nil, l.now, s)}
	if v := decide(tight, s, afg, Need{}); !v.OK || v.Mem.Need > 1.2 {
		t.Fatalf("an AppForGutters thread with 22 GB free: counted as %.1f GB: %s", v.Mem.Need, v.Reason)
	}
}

// Three ways the same PC, with the same 36 GB free, really would run out. Each is refused, and
// none of them by adding up peaks.
func TestWhatWouldRunThePCOutOfMemoryIsStillRefused(t *testing.T) {
	s := defaultSettings()
	free := func() Reading { return pc(6, 6, 36, 60, "active") }

	// 1. Work that holds more than is free, all the time it runs: a build measured at 30 GB,
	// give or take nothing. 36 free less the 8 that are kept is 28.
	build := []level{{Name: "`cargo test --workspace` in owl-go here", Minutes: 300, Cores: 12, MeanSq: 144, MemGB: 30, MemSq: 900, MemPeak: 30.5, Seconds: 600, Own: true}}
	v := decide(free(), s, Ask{Kind: "run", Learned: build}, Need{})
	if v.OK || v.Mem.OK || v.Mem.Need < 29 || !strings.Contains(v.Reason, "36.0 GB of 64 GB of memory is free, 8.0 GB is kept free, and this run counts as") {
		t.Fatalf("a 30 GB build with 28 GB to give: %s", v.Reason)
	}

	// 2. Open work that has not taken its memory yet. Four such builds were let in a minute ago
	// and hold 0.4 GB each so far: the PC looks as free as before, and 36 GB will not hold what
	// they are on their way to, on average, with nothing at a peak.
	starting := free()
	for range 4 {
		starting.Work = append(starting.Work, LiveWork{Kind: "run", Repo: "owl-go", What: "cargo test --workspace", MemGB: 0.4, ExpectMemGB: 14, MemSwing: 1, MemMost: 15})
	}
	v = decide(starting, s, Ask{Threads: 3}, Need{})
	if v.OK || v.Mem.OK || v.Mem.Pending < 54 || !strings.Contains(v.Reason, "The 4 pieces of work open here hold 1.6 GB and are expected to hold 56.0") {
		t.Fatalf("four builds that have not taken their memory yet: %s", v.Reason)
	}

	// 3. Many threads that swing. Twelve Owl threads that each hold 3 GB on average and build at
	// 15 one minute in five hold 36 GB between them on average: all 36 free would go to that,
	// before any two build at once.
	busy := free()
	for range 12 {
		busy.Work = append(busy.Work, LiveWork{Kind: "thread", Repo: "owl-go", MemGB: 1, ExpectMemGB: 3, MemSwing: 4.8, MemMost: 15})
	}
	owl := []level{{Name: "owl-go threads here", Minutes: 600, Cores: 2, MeanSq: 8, MemGB: 3, MemSq: 9 + 4.8*4.8, MemPeak: 15, Own: true}}
	v = decide(busy, s, Ask{Threads: 12, Learned: owl}, Need{})
	if v.OK || v.Mem.OK {
		t.Fatalf("a thirteenth such thread: %s", v.Reason)
	}
	// The room kept for their swing is 33 GB: under three of the twelve at their most (36), where
	// adding their peaks up would have kept 144.
	if v.Open.MemKept < 32 || v.Open.MemKept > 34 {
		t.Fatalf("kept %.1f GB for twelve threads' swing, want 33.3", v.Open.MemKept)
	}

	// And an urgent launch, which the open work steps aside for on its processors, is still
	// counted with its swing whole (3 expected and twice 4.8): memory is not given back by
	// lowering a priority.
	urgent := free()
	urgent.MemAvailGB = 20
	if v := decide(urgent, s, Ask{Urgent: true, Learned: owl}, Need{}); v.OK || v.Mem.OK || v.Mem.Need < 12 || v.Mem.Need > 13 {
		t.Fatalf("an urgent Owl thread with 20 GB free: counted as %.2f GB: %s", v.Mem.Need, v.Reason)
	}
}

// An open piece of work is counted as itself: what its own minutes held, the recent ones most.
func TestAnOpenThreadIsExpectedToHoldWhatItsOwnMinutesHeld(t *testing.T) {
	r, _, _ := pcOfThatEvening(t)
	by := map[string]LiveWork{}
	for _, w := range r.Work {
		by[w.What] = w
	}
	// An hour old, never above 0.5 GB, in a repo whose threads hold 1: expected to hold 0.6, and
	// to reach no more than 0.7.
	if w := by["uploads"]; w.MemGB != 0.5 || w.ExpectMemGB < 0.55 || w.ExpectMemGB > 0.65 || w.MemSwing > 0.3 || w.MemMost > 0.75 {
		t.Fatalf("a quiet AppForGutters thread: %+v", w)
	}
	// The Owl thread that built at 9 GB half an hour ago holds 2 now. It is expected to hold 2.6,
	// and to reach 7.7: its own build, which counts for less as it gets older, and a share of
	// what Owl threads reach.
	if w := by["port-engine"]; w.MemGB != 2 || w.ExpectMemGB < 2.3 || w.ExpectMemGB > 3 || w.MemSwing < 1 || w.MemMost < 7 || w.MemMost > 8.5 {
		t.Fatalf("the Owl thread that built a while ago: %+v", w)
	}
	// The Owl thread in a build now holds 6, more than it is expected to hold: it reaches at
	// least what it already holds.
	if w := by["desktop-tests"]; w.MemGB != 6 || w.ExpectMemGB > 4 || w.MemMost < 6 {
		t.Fatalf("the Owl thread in a build: %+v", w)
	}
	// Three minutes old, it is still mostly an Owl thread like the others: expected to hold 2.2
	// and to reach 14.3, not to hold 18.4.
	if w := by["keeper"]; w.MemGB != 0.6 || w.ExpectMemGB < 2 || w.ExpectMemGB > 3 || w.MemMost < 14 || w.MemMost > 18.4 {
		t.Fatalf("an Owl thread three minutes old: %+v", w)
	}
}

// pcOfThatNight is Jon's PC later the same day, 2026-11-11 at 17:36, from its own `fairshare status
// --json`: 64 GB with 35.2 free, 4 of 22 processors in use, and nine threads open that hold 9 GB.
// One Owl thread four minutes old holds 0.8 GB and might build Rust at 18.5, because one minute
// in 2,500 of every thread measured had held 25.3.
func pcOfThatNight() Reading {
	r := pc(8.3, 5.3, 35.17, 52, "away")
	r.Reserved, r.Capacity = 2, 22
	for _, w := range []LiveWork{
		{Repo: "owl-go", What: "t3-rust-port-machinery", MemGB: 0.76, ExpectMemGB: 2.41, MemSwing: 1.16, MemMost: 18.52},
		{Repo: "jk101", MemGB: 3.51, ExpectMemGB: 3.81, MemSwing: 1.94, MemMost: 7.9},
		{Repo: "manager", MemGB: 0.86, ExpectMemGB: 1.42, MemSwing: 1.11, MemMost: 9.78},
		{Repo: "owl-go", What: "claude-tests-off-jons-desktop", MemGB: 0.57, ExpectMemGB: 1.35, MemSwing: 1.09, MemMost: 8.19},
		{Repo: "fairshare", MemGB: 0.53, ExpectMemGB: 0.94, MemSwing: 0.3, MemMost: 3.47},
		{Repo: "appforgutters", MemGB: 0.58, ExpectMemGB: 1.33, MemSwing: 1.32, MemMost: 4.09},
		{Repo: "jgengine", MemGB: 1.63, ExpectMemGB: 1.53, MemSwing: 0.61, MemMost: 3.44},
		{Repo: "jk101", MemGB: 0.46, ExpectMemGB: 0.68, MemSwing: 0.66, MemMost: 4.62},
		{Repo: "scratch", MemGB: 0.08, ExpectMemGB: 0.09, MemSwing: 0.12, MemMost: 0.22},
	} {
		w.Kind = "thread"
		r.Work = append(r.Work, w)
	}
	return r
}

// The rarest minute of one piece lands in the memory the machine keeps free: it is not kept a
// second time on top of it.
func TestARarePeakUsesTheMemoryKeptFreeAndIsNotKeptTwice(t *testing.T) {
	s := defaultSettings()
	r := pcOfThatNight()
	// What had been measured: every thread 1.4 GB give or take 0.7, and 25.3 in its heaviest
	// minute; Owl threads 2.8 give or take 1, and the same 25.3; fairshare threads 0.9, never
	// above it.
	every := level{Name: "threads here", Minutes: 2547, Cores: 0.82, MeanSq: 0.82*0.82 + 1.72*1.72, MemGB: 1.44, MemSq: 1.44*1.44 + 0.71*0.71, MemPeak: 25.26}
	owl := level{Name: "owl-go threads here", Minutes: 438, Cores: 2.4, MeanSq: 2.4*2.4 + 2.4*2.4, MemGB: 2.82, MemSq: 2.82*2.82 + 1.04*1.04, MemPeak: 25.26, Own: true}
	small := level{Name: "fairshare threads here", Minutes: 79, Cores: 1, MeanSq: 1.09, MemGB: 0.89, MemSq: 0.89 * 0.89, MemPeak: 0.89, Own: true}

	// The rule that refused it kept the furthest reach on top of the reserve.
	_, _, _, open := openWork(r.Work)
	used := r.MemTotalGB - r.MemAvailGB
	limit := r.MemTotalGB * (1 - s.MemReserveFraction)
	if owed := open.expect + open.reach - open.held; !near(open.reach, 16.11) || used+owed < 49 || limit-used-owed > 7 {
		t.Fatalf("that night: %.1f in use and %.1f owed with the reach kept whole, of %.1f", used, owed, limit)
	}
	for _, c := range []struct {
		name            string
		levels          []level
		before          float64 // what it counted as while the reach was kept on top of the reserve
		least, most     float64 // what it counts as now
		totalLo, totalH float64
	}{
		{"a thread in a repo not measured yet", []level{every}, 9.06, 8.9, 9.2, 50, 51.2},
		{"an Owl thread", []level{every, owl}, 9.14, 9, 9.3, 50, 51.3},
	} {
		v := decide(r, s, Ask{Learned: c.levels}, Need{})
		if before := used + open.expect + open.reach - open.held + c.before; before <= limit {
			t.Fatalf("%s: the rule before counted %.1f of %.1f and would have let it in; that night it refused", c.name, before, limit)
		}
		if !v.OK || !v.Mem.OK || v.Mem.Need < c.least || v.Mem.Need > c.most {
			t.Fatalf("%s: counted as %.2f GB: %s", c.name, v.Mem.Need, v.Reason)
		}
		if total := v.Mem.Used + v.Mem.Pending + v.Mem.Need; total < c.totalLo || total > c.totalH {
			t.Fatalf("%s: %.1f of %.1f GB counted as taken", c.name, total, v.Mem.Limit)
		}
		// 8.1 is kept on top of the 8 the PC keeps free: together the 16.1 the Owl thread reaches.
		if !near(v.Open.MemKept, 8.11) || !near(v.Open.MemReach, 16.11) {
			t.Fatalf("%s: the open work: %+v", c.name, v.Open)
		}
	}
	// A thread of a repo never seen above 1 GB counts as about that, and has room to spare.
	if v := decide(r, s, Ask{Learned: []level{every, small}}, Need{}); !v.OK || v.Mem.Need > 1.5 {
		t.Fatalf("a fairshare thread: counted as %.2f GB: %s", v.Mem.Need, v.Reason)
	}

	// What has to hold still holds. The most one piece was seen holding fits in the machine: with
	// 26 GB free, 25.3 for a new thread on top of what the nine are expected to hold does not,
	// and it is refused, with both numbers in the answer.
	tight := r
	tight.MemAvailGB = 26
	v := decide(tight, s, Ask{Learned: []level{every}}, Need{})
	if v.OK || v.Mem.OK || !strings.Contains(v.Reason, "with 8.1 kept for when they hold more (never every piece at its most at once).") ||
		!strings.Contains(v.Reason, "The furthest one piece was seen reaching is 16.1 GB above that: the memory kept free takes the first of it.") {
		t.Fatalf("26 GB free: %s", v.Reason)
	}
	if fits := tight.MemTotalGB - tight.MemAvailGB + (open.expect - open.held) + v.Sized.MemMost; fits <= tight.MemTotalGB {
		t.Fatalf("26 GB free: the most a new thread reaches would fit in the machine (%.1f of %.0f), and it was refused", fits, tight.MemTotalGB)
	}
	// And a thread that was never seen above 1 GB still fits there.
	if v := decide(tight, s, Ask{Learned: []level{every, small}}, Need{}); !v.OK {
		t.Fatalf("a fairshare thread with 26 GB free: %s", v.Reason)
	}
	// A machine that keeps nothing free has nothing to take a rare minute out of: all of it is kept.
	none := s
	none.MemReserveFraction = 0
	if v := decide(r, none, Ask{Learned: []level{every}}, Need{}); !near(v.Open.MemKept, 16.11) || v.Mem.Need < 8.9 {
		t.Fatalf("no reserve: %+v, a new thread counted as %.2f", v.Open, v.Mem.Need)
	}
}
