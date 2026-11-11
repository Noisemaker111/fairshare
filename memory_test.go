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
	if got := load(1, 3, 4, 21).kept(2); got != 18 {
		t.Fatalf("one piece: %.1f kept, want its whole reach of 18", got)
	}
	// Eight of them do not build in the same minute: 22.6 is kept (twice their swing together),
	// which is one at its most and a second well on its way, and not eight times 18.
	if got := load(8, 3, 4, 21).kept(2); !near(got, 2*4*math.Sqrt(8)) || got > 144/6 {
		t.Fatalf("eight pieces: %.1f kept, want 22.6", got)
	}
	// Thirty-two of them: 45 is kept, where every piece at its most would be 576.
	if got := load(32, 3, 4, 21).kept(2); !near(got, 2*4*math.Sqrt(32)) {
		t.Fatalf("thirty-two pieces: %.1f kept, want 45.3", got)
	}
	// Steady work that is nearly always at its most: its swing is wide of its reach, and no
	// more is kept than all of it at its most.
	if got := load(4, 9, 3, 10).kept(2); got != 4 {
		t.Fatalf("four pieces that reach 1 above their average: %.1f kept, want 4", got)
	}
	// learn_mem_margin 0 keeps nothing for chance: only what is certain, one piece's reach.
	if got := load(8, 3, 4, 21).kept(0); got != 18 {
		t.Fatalf("with no margin: %.1f kept, want 18", got)
	}
	// Three of one run started together reach their most together: three times one.
	var three memLoad
	three.add(0, 5, 1, 9, 3)
	if three.expect != 15 || three.kept(2) != 12 {
		t.Fatalf("three started together: %.1f expected and %.1f kept, want 15 and 12", three.expect, three.kept(2))
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
	// The seven hold 11.6 GB and are expected to hold 11.7. Room for the one that reaches furthest
	// (an Owl thread three minutes old, 12.1 above what it is expected to hold) is kept once: 12.2
	// owed in all, where adding every thread's reach up made 36.9. The new thread is expected to
	// hold 1.7 and may reach 21.1: that is 7.3 further than any of the seven, so it adds 1.7 and
	// that 7.3, and counts as 9.0, not as 21.1.
	if !near(v.Open.MemGB, held) || v.Open.MemExpect < 11 || v.Open.MemExpect > 12.5 || v.Open.MemKept < 11.5 || v.Open.MemKept > 12.6 {
		t.Fatalf("the open work's memory: %+v", v.Open)
	}
	if v.Sized.MemGB < 1.5 || v.Sized.MemGB > 2 || v.Mem.Need < 8.5 || v.Mem.Need > 9.5 || v.Mem.Pending < 11.5 || v.Mem.Pending > 13 {
		t.Fatalf("a new thread: expected to hold %.1f, counted as %.1f on top of %.1f", v.Sized.MemGB, v.Mem.Need, v.Mem.Pending)
	}
	if total := v.Mem.Used + v.Mem.Pending + v.Mem.Need; total < 48.5 || total > 50 {
		t.Fatalf("counted as %.1f of the %.1f GB that may be taken", total, v.Mem.Limit)
	}
	t.Logf("before: 28.0 in use + %.1f owed + %.1f for the thread = %.1f of %.1f, refused", owedBefore, v.Sized.MemMost, 28+owedBefore+v.Sized.MemMost, v.Mem.Limit)
	t.Logf("now:    28.0 in use + %.1f owed + %.1f for the thread = %.1f of %.1f, let in", v.Mem.Pending, v.Mem.Need, 28+v.Mem.Pending+v.Mem.Need, v.Mem.Limit)

	// It is not let in on the average alone. With 26 GB free the next Owl build would land in
	// the memory that is kept free, and the answer says what was counted.
	tight := r
	tight.MemAvailGB = 26
	if v := decide(tight, s, ask, Need{}); v.OK || v.Mem.OK || !strings.Contains(v.Reason, "(never every piece at its most at once)") ||
		!strings.Contains(v.Reason, fmt.Sprintf("The 7 pieces of work open here hold %.1f GB and are expected to hold", held)) {
		t.Fatalf("26 GB free: %s", v.Reason)
	}
	// An AppForGutters thread, whose threads were never seen above 1 GB, still fits there.
	afg := Ask{Threads: 7, Learned: known.levels("thread", "appforgutters", "", nil, l.now, s)}
	if v := decide(tight, s, afg, Need{}); !v.OK || v.Mem.Need > 1.2 {
		t.Fatalf("an AppForGutters thread with 26 GB free: counted as %.1f GB: %s", v.Mem.Need, v.Reason)
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
	// counted at the most it reaches: memory is not given back by lowering a priority.
	urgent := free()
	urgent.MemAvailGB = 20
	if v := decide(urgent, s, Ask{Urgent: true, Learned: owl}, Need{}); v.OK || v.Mem.OK || v.Mem.Need < 14.5 || v.Mem.Need > 15 {
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
