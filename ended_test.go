package main

import (
	"math"
	"testing"
	"time"
)

// A run of 12 s seen by looks 5 s apart: until 2026-11-11 its last 2 s, and what its processes
// used in them, were lost, so it was learned as 10 s at fewer processors than it used.
func TestWorkIsCountedToTheSecondItEnded(t *testing.T) {
	l := newLooks()
	l.marks = []runMark{{PID: 400, Repo: "demo", What: "python burn.py 2 12"}}
	here := procNode{pid: 400, parent: 300, created: 40, name: "fairshare.exe"}
	burn := procNode{pid: 500, parent: 400, created: 50, name: "python.exe", cores: 10}
	l.look(here, burn)
	l.look(here, burn)
	// It ends 2 s into the next look. The meter kept a handle on both processes, so it still
	// reads what each used after the last look that saw it, and when it ended.
	end := l.now.Add(2 * time.Second)
	l.marks = nil
	hereGone, burnGone := here, burn
	hereGone.gone, hereGone.ended, hereGone.cores = true, end, 0.1
	burnGone.gone, burnGone.ended, burnGone.cores = true, end.Add(-100*time.Millisecond), 3.9
	ended := l.look(hereGone, burnGone)
	if len(ended) != 1 || !ended[0].Ended || ended[0].Kind != "run" {
		t.Fatalf("the ended run: %+v", ended)
	}
	sp := ended[0]
	if math.Abs(sp.Seconds-12) > 1e-6 || math.Abs(sp.Minutes-0.2) > 1e-6 || math.Abs(sp.Cores-2) > 1e-6 {
		t.Fatalf("ran %.2f s, measured %.3f minutes at %.2f processors; want 12 s, 0.2 minutes at 2", sp.Seconds, sp.Minutes, sp.Cores)
	}
	if len(l.t.units) != 0 || len(l.t.owner) != 0 {
		t.Fatalf("an ended run is still followed: %d units, %d processes", len(l.t.units), len(l.t.owner))
	}

	// A process that ends while its work goes on adds what it used and nothing else.
	l = newLooks()
	l.where[200] = [2]string{`C:\Users\Jk101\.t3\worktrees\owl-go\port-engine`, ""}
	claude := procNode{pid: 200, parent: 100, created: 20, name: "claude.exe"}
	rustc := procNode{pid: 600, parent: 200, created: 60, name: "rustc.exe", cores: 5, memGB: 1}
	l.look(claude, rustc)
	u := l.unit("thread", "owl-go")
	rustc.gone, rustc.ended, rustc.cores, rustc.memGB = true, l.now.Add(time.Second), 2, 0
	l.look(claude, rustc)
	if u.procs != 1 || math.Abs(u.cpu-7) > 1e-9 || u.mem != 0 || math.Abs(u.seconds-10) > 1e-9 {
		t.Fatalf("after its compiler ended the thread has %d processes, %.1f processor-seconds, %.1f GB over %.0f s; want 1, 7, 0 over 10", u.procs, u.cpu, u.mem, u.seconds)
	}
	// A process that ended and was nobody's is nobody's.
	stray := procNode{pid: 700, parent: 1, created: 70, name: "git.exe", cores: 3, gone: true, ended: l.now}
	l.look(claude, stray)
	if math.Abs(u.cpu-7) > 1e-9 || len(l.t.units) != 1 {
		t.Fatalf("an ended process of nobody's was counted: %.1f processor-seconds, %d pieces", u.cpu, len(l.t.units))
	}
}

// Measured on Jon's PC on 2026-11-11: a quarter of the processor time in use was used by
// processes that began and ended between two looks 5 s apart.
func TestProcessorTimeNoLookSawGoesToTheWorkWhoseProcessesCameAndWent(t *testing.T) {
	nodes := []procNode{
		{pid: 1, name: "suite.exe", cores: 10, work: true},             // alive at both looks: all of it was seen
		{pid: 2, name: "rustc.exe", cores: 2, work: true, fresh: true}, // began since the last look
		{pid: 3, name: "cargo.exe", cores: 1, work: true, gone: true},  // ended since the last look
		{pid: 4, name: "go.exe", cores: 0.1, work: true, fresh: true},
		{pid: 5, name: "chrome.exe", cores: 0.5, fresh: true}, // not agent work: no compiler job is its
	}
	// 19.6 processor-seconds were used in the look and the processes account for 13.6.
	if got := creditUnseen(nodes, 6); got != 6 {
		t.Fatalf("shared out %.1f of 6", got)
	}
	for i, want := range []float64{10, 4, 3, 2.1, 0.5} {
		if math.Abs(nodes[i].cores-want) > 1e-9 {
			t.Errorf("%s (pid %d) counts as %.2f, want %.2f", nodes[i].name, nodes[i].pid, nodes[i].cores, want)
		}
	}
	var sum float64
	for _, n := range nodes {
		sum += n.cores
	}
	if math.Abs(sum-19.6) > 1e-9 {
		t.Fatalf("the processes add up to %.2f, want the 19.6 that were used", sum)
	}
	// With nothing unaccounted for, or no agent process that came or went, nothing changes.
	steady := []procNode{{pid: 1, cores: 10, work: true}, {pid: 5, cores: 0.5, fresh: true}}
	if creditUnseen(steady, 6) != 0 || steady[0].cores != 10 || steady[1].cores != 0.5 {
		t.Fatalf("processor time was given to processes that neither began nor ended, or are not agent work: %+v", steady)
	}
	young := []procNode{{pid: 2, cores: 2, work: true, fresh: true}}
	if creditUnseen(young, -0.3) != 0 || creditUnseen(young, 0) != 0 || young[0].cores != 2 {
		t.Fatalf("a look with nothing unaccounted for changed a process: %+v", young)
	}
}

func TestARunThatDrivesTheDesktopIsOpenWorkThatDoes(t *testing.T) {
	l := newLooks()
	l.marks = []runMark{{PID: 400, Repo: "owl-go", What: "cargo test port_parity", Desktop: true}, {PID: 410, Repo: "owl-go", What: "cargo build"}}
	l.look(procNode{pid: 400, created: 40, name: "fairshare.exe"}, procNode{pid: 410, created: 41, name: "fairshare.exe"})
	drives := map[string]bool{}
	for _, w := range l.t.live(l.now, learned{}, l.s) {
		drives[w.What] = w.Desktop
	}
	if !drives["cargo test port_parity"] || drives["cargo build"] || len(drives) != 2 {
		t.Fatalf("which open work drives the desktop: %v", drives)
	}
	if got := liveFromMarks(l.marks); len(got) != 2 || !got[0].Desktop || got[1].Desktop {
		t.Fatalf("with no meter the marks say it: %+v", got)
	}
	if got := workWords(LiveWork{Kind: "run", Repo: "owl-go", What: "cargo test port_parity", Desktop: true}); got != "run driving the desktop in owl-go: cargo test port_parity" {
		t.Fatalf("named %q", got)
	}
}
