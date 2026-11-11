package main

import (
	"testing"
	"time"
)

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
}

func TestARepoCountsAsItsOwnMeasuredSessions(t *testing.T) {
	s := defaultSettings()
	now := time.Date(2026, 11, 11, 10, 40, 0, 0, time.UTC)
	h := repoHistory{}
	// An hour of two Owl sessions compiling Rust and two AppForGutters sessions on their model.
	for i := range 60 {
		at := now.Add(-time.Duration(60-i) * time.Minute)
		h.add(at, 1, []sessionUse{{"owl-go", 4.5, 6}, {"owl-go", 5.5, 8}, {"appforgutters", 0.8, 1.2}, {"appforgutters", 1.2, 1.8}, {"", 9, 9}}, 8)
	}
	owl, ok := h.need("owl-go", now, s)
	if !ok || owl.Cores != 5 || owl.MemGB != 7 || owl.Minutes != 120 {
		t.Fatalf("owl-go = %+v %v", owl, ok)
	}
	afg, ok := h.need("AppForGutters", now, s)
	if !ok || afg.Cores != 1 || afg.MemGB != 1.5 {
		t.Fatalf("appforgutters = %+v %v", afg, ok)
	}
	// The estimate a new thread counts as: its repo's, not the machine's 4.5 per thread.
	r := gates("active")
	r.DemandShort, r.DemandLong = 18, 18
	machine := estimate(r, s, Ask{Threads: 4})
	mine := estimate(r, s, Ask{Threads: 4, Repo: &afg})
	if machine.Cores != 4.5 || mine.Cores != 1 {
		t.Fatalf("machine estimate %.1f, appforgutters %.1f; want 4.5 and 1", machine.Cores, mine.Cores)
	}
	if heavy := estimate(r, s, Ask{Threads: 4, Repo: &owl}); heavy.Cores != 5 || heavy.MemGB != 7 {
		t.Fatalf("an owl-go thread counts as %.1f processors and %.1f GB, want 5 and 7", heavy.Cores, heavy.MemGB)
	}
	// Too few minutes, an unknown repo, or minutes older than repo_days: the machine's average.
	h2 := repoHistory{}
	h2.add(now, 10, []sessionUse{{"newrepo", 0.5, 1}}, 8)
	if _, ok := h2.need("newrepo", now, s); ok {
		t.Fatal("10 session-minutes replaced the machine's average")
	}
	if _, ok := h.need("nothing", now, s); ok {
		t.Fatal("an unmeasured repo had a need")
	}
	if _, ok := h.need("owl-go", now.AddDate(0, 0, 9), s); ok {
		t.Fatal("minutes older than repo_days still counted")
	}
	// Days older than keep are forgotten on the next add.
	h.add(now.AddDate(0, 0, 10), 1, nil, 8)
	if len(h) != 0 {
		t.Fatalf("old days kept: %v", h)
	}
}

func TestSessionOfFollowsParentsToTheRunnerAndNotAReusedPid(t *testing.T) {
	procs := []procNode{
		{pid: 100, parent: 40, created: 10, name: "t3 code.exe"},
		{pid: 200, parent: 100, created: 20, name: "claude.exe"},
		{pid: 300, parent: 200, created: 30, name: "bash.exe"},
		{pid: 400, parent: 300, created: 40, name: "cargo.exe"},
		{pid: 410, parent: 400, created: 41, name: "rustc.exe"},
		{pid: 500, parent: 100, created: 50, name: "codex.exe"},
		{pid: 510, parent: 500, created: 51, name: "node.exe"},
		// A process whose parent id was reused by a newer claude.exe is not that session's.
		{pid: 600, parent: 700, created: 60, name: "go.exe"},
		{pid: 700, parent: 100, created: 70, name: "claude.exe"},
		{pid: 800, parent: 9999, created: 80, name: "orphan.exe"},
	}
	got := sessionOf(procs, []string{"claude.exe", "codex.exe"})
	want := []int{-1, 1, 1, 1, 1, 5, 5, -1, 8, -1}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s %d: runner %d, want %d", procs[i].name, procs[i].pid, got[i], want[i])
		}
	}
}
