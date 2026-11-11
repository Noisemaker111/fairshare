package main

import (
	"math"
	"testing"
	"time"
)

// looks runs a tracker over looks five seconds apart; each look is the processes alive in it.
type looks struct {
	t     tracker
	now   time.Time
	asked map[uint32]int
	where map[uint32][2]string
	s     Settings
	marks []runMark
}

func newLooks() *looks {
	s := defaultSettings()
	s.Runners = []string{"claude.exe"}
	return &looks{now: time.Date(2026, 11, 11, 17, 0, 0, 0, time.UTC), asked: map[uint32]int{}, where: map[uint32][2]string{}, s: s}
}

func (l *looks) look(nodes ...procNode) []stretch {
	l.now = l.now.Add(5 * time.Second)
	for i := range nodes {
		nodes[i].work = true
	}
	return l.t.look(l.now, 5, nodes, l.marks, func(pid uint32, _ uint64) (string, string) {
		l.asked[pid]++
		return l.where[pid][0], l.where[pid][1]
	}, l.s)
}

func (l *looks) unit(kind, repo string) *unit {
	for _, id := range l.t.ids() {
		if u := l.t.units[id]; u.kind == kind && u.repo == repo {
			return u
		}
	}
	return nil
}

// The boat host of 2026-11-11: a thread's land outlives the shell that started it and hangs
// under init, where the runner's own tree used 0.05 processors and the machine ran at 3.2.
func TestWorkFollowsADetachedJobBackToItsThread(t *testing.T) {
	l := newLooks()
	tree := "/home/user/.t3/worktrees/appforgutters/agent-uploads-offline"
	l.where[200] = [2]string{tree, "mcp:b847"}
	init1 := procNode{pid: 1, created: 1, name: "systemd"}
	t3 := procNode{pid: 100, parent: 1, created: 10, name: "node"}
	claude := procNode{pid: 200, parent: 100, created: 20, name: "claude"}
	shell := procNode{pid: 300, parent: 200, created: 30, name: "bash"}
	land := procNode{pid: 400, parent: 300, created: 40, name: "bash", cores: 0.5}
	l.look(init1, t3, claude, shell, land)
	u := l.unit("thread", "appforgutters")
	if u == nil || u.procs != 3 || u.tree != "agent-uploads-offline" || u.thread != "mcp:b847" {
		t.Fatalf("the runner, its shell and the land are one thread: %+v", u)
	}
	// The shell ends: Linux hands the land to init. It is still the thread's, and so is what it starts.
	land.parent = 1
	vitest := procNode{pid: 500, parent: 400, created: 50, name: "node (vitest)", cores: 15}
	l.look(init1, t3, claude, land, vitest)
	if u.procs != 3 || math.Abs(u.cpu-16) > 1e-9 {
		t.Fatalf("after its shell ended the land has %d processes and %.1f processor-seconds under the thread, want 3 and 16", u.procs, u.cpu)
	}
	if l.asked[400] != 0 || l.asked[500] != 0 {
		t.Fatalf("a process that was seen under its thread is not asked where it works: %v", l.asked)
	}

	// Never seen under the thread (it was detached between two looks): found by the thread it
	// was started for, wherever it runs.
	l.where[600] = [2]string{"/tmp", "mcp:b847"}
	orphan := procNode{pid: 600, parent: 1, created: 60, name: "bun", cores: 5}
	// On Windows there is no thread id: found by a worktree named after the thread's.
	l.where[700] = [2]string{"/home/user/.t3/worktrees/appforgutters/.land-batch-agent-uploads-offline/packages/backend", ""}
	batch := procNode{pid: 700, parent: 1, created: 70, name: "convex-local-backend", cores: 5}
	// Work in a worktree no thread here owns is a job of that repo.
	l.where[800] = [2]string{`C:\Users\Jk101\.t3\worktrees\owl-go\port-engine\target`, ""}
	loose := procNode{pid: 800, parent: 1, created: 80, name: "cargo.exe", cores: 20}
	// And a process that works nowhere is nobody's, and is asked once.
	l.where[900] = [2]string{"/home/user", ""}
	sshd := procNode{pid: 900, parent: 1, created: 90, name: "sshd", cores: 1}
	// A process id that was reused: its "parent" started after it, so it is not that parent's.
	reused := procNode{pid: 950, parent: 200, created: 5, name: "cron"}
	l.where[950] = [2]string{"/", ""}
	for range 3 {
		l.look(init1, t3, claude, land, vitest, orphan, batch, loose, sshd, reused)
	}
	if u.procs != 5 {
		t.Fatalf("the thread has %d processes, want its runner, land, vitest, the detached job and the land's worktree", u.procs)
	}
	job := l.unit("job", "owl-go")
	if job == nil || job.what != "port-engine" || job.procs != 1 {
		t.Fatalf("work in a worktree without a thread: %+v", job)
	}
	if len(l.t.units) != 2 {
		t.Fatalf("%d pieces of work, want the thread and the job", len(l.t.units))
	}
	if l.asked[900] != 1 || l.asked[950] != 1 || l.asked[600] != 1 || l.asked[1] != 1 {
		t.Fatalf("each process with nothing above it is asked once: %v", l.asked)
	}
}

func TestARunIsItsOwnWorkAndNotItsThreads(t *testing.T) {
	l := newLooks()
	l.where[200] = [2]string{`C:\Users\Jk101\.t3\worktrees\owl-go\port-engine`, ""}
	l.marks = []runMark{{PID: 400, Repo: "Owl-Go", What: "cargo test"}, {PID: 999, Repo: "gone", What: "its process is not in the look"}}
	claude := procNode{pid: 200, parent: 100, created: 20, name: "claude.exe"}
	shell := procNode{pid: 300, parent: 200, created: 30, name: "bash.exe"}
	here := procNode{pid: 400, parent: 300, created: 40, name: "fairshare.exe"}
	cargo := procNode{pid: 500, parent: 400, created: 50, name: "cargo.exe", cores: 30, memGB: 3}
	// A process that happens to have a marked id but is not fairshare is not a run.
	l.marks = append(l.marks, runMark{PID: 300, Repo: "x", What: "y"})
	var learnt []stretch
	for range 12 {
		learnt = append(learnt, l.look(claude, shell, here, cargo)...)
	}
	run, thread := l.unit("run", "owl-go"), l.unit("thread", "owl-go")
	if run == nil || thread == nil || run.what != "cargo test" || run.procs != 2 || thread.procs != 2 || len(l.t.units) != 2 {
		t.Fatalf("run %+v thread %+v of %d", run, thread, len(l.t.units))
	}
	// One minute of looks: each piece's minute is handed over to be learned from.
	if len(learnt) != 2 {
		t.Fatalf("%d stretches after a minute, want one each: %+v", len(learnt), learnt)
	}
	for _, sp := range learnt {
		want := map[string]float64{"run": 6, "thread": 0}[sp.Kind]
		if math.Abs(sp.Minutes-1) > 1e-9 || math.Abs(sp.Cores-want) > 1e-9 || sp.Ended {
			t.Errorf("%s: %.2f minutes at %.1f processors, want 1 at %.0f", sp.Kind, sp.Minutes, sp.Cores, want)
		}
		if (sp.Kind == "run") != (sp.What == "cargo test") {
			t.Errorf("only a run is learned by its command: %+v", sp)
		}
	}
	// The run ends 20 s into the next minute: its last stretch is learned at once, with how long it ran.
	for range 3 {
		l.look(claude, shell, here, cargo)
	}
	ended := l.look(claude, shell)
	if len(ended) != 1 || !ended[0].Ended || ended[0].Kind != "run" || math.Abs(ended[0].Seconds-75) > 1e-9 || math.Abs(ended[0].Minutes-0.25) > 1e-9 || ended[0].MemGB != 3 {
		t.Fatalf("the ended run: %+v", ended)
	}
	if l.unit("run", "owl-go") != nil {
		t.Fatal("an ended run is still open")
	}
}

func TestARunCaughtBeforeItLeftItsMarkIsStillARun(t *testing.T) {
	l := newLooks()
	l.where[200] = [2]string{"/home/user/.t3/worktrees/owl-go/port-engine", ""}
	claude := procNode{pid: 200, parent: 100, created: 20, name: "claude"}
	here := procNode{pid: 400, parent: 200, created: 40, name: "fairshare-linux"}
	git := procNode{pid: 500, parent: 400, created: 50, name: "git", cores: 5}
	// Reached over SSH, a run hangs under nothing; this look puts it under nobody, and its
	// thread's own under the thread.
	ssh := procNode{pid: 600, parent: 1, created: 60, name: "fairshare"}
	bun := procNode{pid: 700, parent: 600, created: 70, name: "bun", cores: 5}
	l.look(claude, here, git, ssh, bun)
	if u := l.unit("thread", "owl-go"); u == nil || u.procs != 3 || len(l.t.units) != 1 {
		t.Fatalf("before the marks: %+v of %d", u, len(l.t.units))
	}
	l.marks = []runMark{{PID: 400, Repo: "owl-go", What: "cargo test"}, {PID: 600, Repo: "appforgutters", What: "bun run gate"}}
	// A runner started by the run is a thread of its own, and stays one.
	inner := procNode{pid: 800, parent: 400, created: 80, name: "claude"}
	l.look(claude, here, git, ssh, bun, inner)
	l.look(claude, here, git, ssh, bun, inner)
	local, far := l.unit("run", "owl-go"), l.unit("run", "appforgutters")
	if local == nil || local.procs != 2 || far == nil || far.procs != 2 || len(l.t.units) != 4 {
		t.Fatalf("after the marks: local %+v, far %+v, of %d", local, far, len(l.t.units))
	}
	if u := l.unit("thread", "owl-go"); u.procs != 1 || math.Abs(local.cpu-10) > 1e-9 || math.Abs(far.cpu-10) > 1e-9 {
		t.Fatalf("the thread keeps %d processes; the runs used %.0f and %.0f since their marks, want 10 each", u.procs, local.cpu, far.cpu)
	}
}

func TestOpenWorkIsExpectedToUseWhatItAndWorkLikeItWasMeasuredUsing(t *testing.T) {
	l := newLooks()
	known := learned{}
	at := l.now
	// Owl threads here: two hours at 4 processors, give or take 2, and 6 GB.
	for range 60 {
		known.add(at, stretch{Kind: "thread", Repo: "owl-go", Minutes: 1, Cores: 2, MemGB: 6}, l.s)
		known.add(at, stretch{Kind: "thread", Repo: "owl-go", Minutes: 1, Cores: 6, MemGB: 6}, l.s)
	}
	l.where[200] = [2]string{`C:\Users\Jk101\.t3\worktrees\owl-go\port-engine`, ""}
	claude := procNode{pid: 200, parent: 100, created: 20, name: "claude.exe", memGB: 0.3}
	l.look(claude)
	// Just started and reading its brief: it uses nothing, and is expected to use what Owl threads do.
	w := l.t.live(l.now, known, l.s)
	if len(w) != 1 || w[0].Now != 0 || math.Abs(w[0].Expect-3.98) > 0.01 || math.Abs(w[0].Swing-2.01) > 0.01 || math.Abs(w[0].ExpectMemGB-5.98) > 0.01 {
		t.Fatalf("a new Owl thread: %+v", w)
	}
	// Forty minutes of waiting on its model later it counts mostly as itself, and still a little
	// as an Owl thread: it may yet build.
	for range 480 {
		l.look(claude)
	}
	w = l.t.live(l.now, known, l.s)
	if w[0].Expect < 0.8 || w[0].Expect > 1.2 || w[0].Cores != 0 || w[0].Minutes < 20 {
		t.Fatalf("after forty quiet minutes: %+v", w[0])
	}
	// The same thread then building for forty minutes at 6 counts as what it used lately (the
	// quiet minutes before weigh less by now), give or take the difference between the two.
	claude.cores = 30
	for range 480 {
		l.look(claude)
	}
	w = l.t.live(l.now, known, l.s)
	if w[0].Now != 6 || w[0].Expect < 4 || w[0].Expect > 6 || w[0].Swing < 2 {
		t.Fatalf("after forty busy minutes: %+v", w[0])
	}
	// A machine whose meter is not running still counts a run, at what it was let in as.
	got := liveFromMarks([]runMark{{PID: 7, Repo: "AppForGutters", What: "bun run gate", Need: Need{Cores: 5.2, MemGB: 6}}})
	if len(got) != 1 || got[0].Expect != 5.2 || got[0].ExpectMemGB != 6 || got[0].Repo != "appforgutters" {
		t.Fatalf("a run known only by its mark: %+v", got)
	}
}

func TestWorkFoundLongAfterItBeganCountsAsWhatItUsedSinceThen(t *testing.T) {
	l := newLooks()
	l.where[200] = [2]string{"/home/user/.t3/worktrees/owl-go/port-engine", ""}
	// The meter starts under a thread that has built Rust for two hours: 21600 processor-seconds
	// in 7200 seconds is 3 processors, though this very look catches it waiting on its model.
	began := l.now.Add(-2 * time.Hour)
	claude := procNode{pid: 200, parent: 100, created: 20, name: "claude", started: began, total: 600}
	cargo := procNode{pid: 300, parent: 200, created: 30, name: "cargo", started: began.Add(time.Hour), total: 21000, memGB: 3}
	l.look(claude, cargo)
	w := l.t.live(l.now, learned{}, l.s)
	if len(w) != 1 || !near(w[0].Cores, 3) || w[0].Minutes != 30 || !near(w[0].Expect, 2.5) || !w[0].Since.Equal(began) {
		t.Fatalf("found two hours in: %+v", w)
	}
	// A thread found as it starts has no such past, and one that used nothing has nothing to stand in.
	l = newLooks()
	l.look(procNode{pid: 200, parent: 100, created: 20, name: "claude", started: l.now.Add(2 * time.Second), total: 1})
	if w := l.t.live(l.now, learned{}, l.s); w[0].Minutes != 0 || w[0].Expect != 1 {
		t.Fatalf("found as it started: %+v", w)
	}
}

func TestRepoOfNamesTheRepoASessionWorksIn(t *testing.T) {
	for dir, want := range map[string]string{
		`C:\Users\Jk101\.t3\worktrees\appforgutters\claude-upload-fix`:    "appforgutters",
		`C:\Users\Jk101\.t3\worktrees\owl-go\port-computer`:               "owl-go",
		`C:\Users\Jk101\work\owl-go\.claude\worktrees\port-engine-abc123`: "owl-go",
		`C:\Users\Jk101\work\AppForGutters`:                               "appforgutters",
		`C:\Users\Jk101\work\fairshare\`:                                  "fairshare",
		`/home/user/.t3/worktrees/fairshare/claude-priority-room`:         "fairshare",
		`C:\`: "",
		``:    "",
	} {
		if got := repoOf(dir); got != want {
			t.Errorf("repoOf(%q) = %q, want %q", dir, got, want)
		}
	}
	for dir, want := range map[string][2]string{
		`/home/user/.t3/worktrees/fairshare/claude-priority-room/scripts`: {"fairshare", "claude-priority-room"},
		`C:\Users\Jk101\work\owl-go\.claude\worktrees\port-engine-abc123`: {"owl-go", "port-engine-abc123"},
		`C:\Users\Jk101\work\owl-go`:                                      {"", ""},
		`/home/user/.t3/worktrees/fairshare`:                              {"", ""},
	} {
		if repo, tree := worktreeOf(dir); repo != want[0] || tree != want[1] {
			t.Errorf("worktreeOf(%q) = %q, %q, want %q", dir, repo, tree, want)
		}
	}
	if !isRunner("claude", []string{"claude.exe"}) || !isRunner("Codex.EXE", []string{"codex"}) || isRunner("claude-helper", []string{"claude.exe"}) {
		t.Error("a runner is named with or without .exe, in any case, and nothing else")
	}
}
