package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeRepo makes a main checkout with an origin and one worktree of it, as git lays them out.
func fakeRepo(t *testing.T, origin string) (main, worktree string) {
	t.Helper()
	root := t.TempDir()
	main = filepath.Join(root, "proj")
	wtGit := filepath.Join(main, ".git", "worktrees", "w1")
	if err := os.MkdirAll(wtGit, 0o755); err != nil {
		t.Fatal(err)
	}
	config := "[core]\n\tbare = false\n"
	if origin != "" {
		config += "[remote \"origin\"]\n\turl = " + origin + "\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n"
	}
	os.WriteFile(filepath.Join(main, ".git", "config"), []byte(config), 0o644)
	os.WriteFile(filepath.Join(wtGit, "commondir"), []byte("../..\n"), 0o644)
	worktree = filepath.Join(root, "worktrees", "w1")
	os.MkdirAll(filepath.Join(worktree, "sub"), 0o755)
	os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: "+wtGit+"\n"), 0o644)
	return main, worktree
}

func TestRepoIdentityIsSharedByEveryWorktree(t *testing.T) {
	main, wt := fakeRepo(t, "git@github.com:Owner/Proj.git")
	if a, b, c := repoID(main), repoID(wt), repoID(filepath.Join(wt, "sub")); a != "github.com/owner/proj" || b != a || c != a {
		t.Fatalf("one repo, three identities: %q %q %q", a, b, c)
	}
	if got := normalizeRemote("https://user@github.com/Owner/Proj.git/"); got != "github.com/owner/proj" {
		t.Error("https and ssh forms differ:", got)
	}
	main, wt = fakeRepo(t, "")
	if repoID(main) != "proj" || repoID(wt) != "proj" {
		t.Error("a repo without origin is named after its main checkout:", repoID(main), repoID(wt))
	}
	// exec sees the agent's text, slot the words the shell made of it: one key.
	if heavyKey(`bun run lane:close  -m "Fixed: x"`) != heavyKey("bun run lane:close -m Fixed: x") {
		t.Error("quoting changed the key")
	}
}

func TestACommandJoinsTheListWhenOneRunIsHeavy(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("FAIRSHARE_EVENTS", filepath.Join(t.TempDir(), "events.jsonl"))
	main, wt := fakeRepo(t, "https://github.com/o/p")
	now := time.Now()
	if recordHeavy(heavyRun{Dir: wt, Cmd: "bun run build", CPUSeconds: heavyCPUSeconds - 1, At: now}) {
		t.Fatal("a run under the threshold joined")
	}
	if _, ok := heavyListed(main, "bun run build"); ok {
		t.Fatal("listed after a light run")
	}
	if !recordHeavy(heavyRun{Dir: wt, Cmd: "bun run build", CPUSeconds: heavyCPUSeconds, WallSeconds: 60, PeakMB: 900, At: now}) {
		t.Fatal("a heavy run did not join")
	}
	// Another worktree of the same repo shares the entry; a light run keeps it up to date.
	recordHeavy(heavyRun{Dir: main, Cmd: "bun  run build", CPUSeconds: 5, At: now})
	e, ok := heavyListed(main, "bun run build")
	if !ok || e.Runs != 2 || e.CPUSeconds != 5 || e.Repo != "github.com/o/p" {
		t.Fatalf("entry not shared or not updated: %+v %v", e, ok)
	}
	// Deleting the entry by hand takes the command off.
	if err := editHeavy(func([]heavyEntry) []heavyEntry { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, ok := heavyListed(wt, "bun run build"); ok {
		t.Error("still listed after the entry was deleted")
	}
	b, _ := os.ReadFile(eventsFile())
	if !strings.Contains(string(b), `"ev":"heavy"`) {
		t.Error("joining was not logged")
	}
}

// The rule itself: with every slot held, a command not on the list runs at once, and a listed one
// waits, names the holders and points at boat.
func TestOnlyListedCommandsWaitForASlot(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("FAIRSHARE_EVENTS", filepath.Join(t.TempDir(), "events.jsonl"))
	t.Setenv("FAIRSHARE_HEAVY_SLOTS", "1")
	t.Setenv(heldEnv, "")
	_, wt := fakeRepo(t, "https://github.com/o/p")
	t.Chdir(wt)
	holder, err := acquireSlot("bun run verify", `C:\repo-a`, "", heavyNeed{}, time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()

	start := time.Now()
	if code := cmdSlot([]string{"--wait", "3000", "cmd", "/c", "exit", "7"}); code != 7 {
		t.Fatal("an unlisted command did not run while the slots were held, exit", code)
	}
	if waited := time.Since(start); waited > 2*time.Second {
		t.Fatal("an unlisted command waited", waited)
	}

	if err := editHeavy(func(l []heavyEntry) []heavyEntry {
		return append(l, heavyEntry{Repo: repoID(wt), Cmd: heavyKey("cmd /c exit 7"), CPUSeconds: 300, FirstSeen: "2026-11-06T00:00:00Z"})
	}); err != nil {
		t.Fatal(err)
	}
	if code := cmdSlot([]string{"--wait", "1200", "cmd", "/c", "exit", "7"}); code != 124 {
		t.Fatal("a listed command started with no slot free, exit", code)
	}
	e, _ := heavyListed(wt, "cmd /c exit 7")
	note := waitNote(e, "cmd /c exit 7", describeHolders(slotHolders()))
	for _, want := range []string{"heavy list", "`bun run verify` in repo-a", "fairshare lease boat -- ~/.ascii/bin/boat.exe run --no-env --timeout 1200 --push . -- cmd /c exit 7"} {
		if !strings.Contains(note, want) {
			t.Errorf("the wait note should say %q: %s", want, note)
		}
	}
	holder.Close()
	if code := cmdSlot([]string{"--wait", "3000", "cmd", "/c", "exit", "7"}); code != 7 {
		t.Fatal("a listed command did not run once a slot was free, exit", code)
	}
}

// exec, the runner every foreground test or build goes through, follows the same rule.
func TestExecWaitsOnlyForAListedCommand(t *testing.T) {
	local := t.TempDir()
	t.Setenv("LOCALAPPDATA", local)
	t.Setenv("FAIRSHARE_HEAVY_SLOTS", "1")
	t.Setenv(heldEnv, "")
	holder, err := acquireSlot("go test ./...", `C:\repo-b`, "", heavyNeed{}, time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	env := []string{"LOCALAPPDATA=" + local, "FAIRSHARE_HEAVY_SLOTS=1"}
	if out, code, _ := runExec(t, "pwsh", "Write-Output unlisted", 4000, env...); code != 0 || !strings.Contains(out, "unlisted") {
		t.Fatal("an unlisted run did not start while the slot was held:", code, out)
	}
	cwd, _ := os.Getwd()
	editHeavy(func(l []heavyEntry) []heavyEntry {
		return append(l, heavyEntry{Repo: repoID(cwd), Cmd: heavyKey("Write-Output listed"), CPUSeconds: 300, FirstSeen: "2026-11-06T00:00:00Z"})
	})
	out, code, ev := runExec(t, "pwsh", "Write-Output listed", 4000, env...)
	if code != 124 || ev["stopped"] != "no-slot" {
		t.Fatal("a listed run started with no slot free:", code, out)
	}
	for _, want := range []string{"heavy list", "`go test ./...` in repo-b", "fairshare lease boat -- ~/.ascii/bin/boat.exe run --no-env --timeout 1200 --push . -- Write-Output listed"} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal should say %q: %s", want, out)
		}
	}
}

func TestSeedingCountsRunsAlreadyMeasured(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	events := filepath.Join(t.TempDir(), "events.jsonl")
	t.Setenv("FAIRSHARE_EVENTS", events)
	_, wt := fakeRepo(t, "https://github.com/o/p")
	gone := filepath.Join(filepath.Dir(wt), "deleted-worktree") // a worktree removed since its run
	log := func(cmd, cwd string, cpuMs int) {
		logEvent("run", map[string]any{"cmd": cmd, "cwd": cwd, "cpu_ms": cpuMs, "wall_ms": 1000})
	}
	log("go test ./...", wt, 300000)
	log("go test ./...", gone, 400000)
	log("tsc --noEmit", wt, 10000)
	n, err := seedHeavy()
	if err != nil || n != 1 {
		t.Fatalf("seeded %d entries (%v), want 1", n, err)
	}
	if e, ok := heavyListed(wt, "go test ./..."); !ok || e.Runs != 2 || e.CPUSeconds != 400 {
		t.Errorf("the gone worktree's run should count for its repo: %+v %v", e, ok)
	}
	if _, ok := heavyListed(wt, "tsc --noEmit"); ok {
		t.Error("a light run was seeded")
	}
	if n, _ := seedHeavy(); n != 0 {
		t.Error("seeding ran twice")
	}
}
