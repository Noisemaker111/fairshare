package main

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Runs that stay in the agent's shell and could turn out heavy go through `slot`, which measures
// them and waits only for a listed one. Builtins, servers and VAR=value commands stay as they are.
func TestRunsOutsideTheRunnerGoThroughSlot(t *testing.T) {
	dir := t.TempDir()
	bg := map[string]any{"run_in_background": true}
	match(t, testCall(t, dir, "bun run verify:smoke", "bash", "claude", bg).Command, `^'.*' slot bun run verify:smoke$`)
	// In front, the wait is half the agent's time limit, so the call answers before it is cut off.
	match(t, testCall(t, dir, "bun run check > .scratch/check.log 2>&1", "bash", "claude", nil).Command, `^'.*' slot --wait 300000 bun run check > \.scratch/check\.log 2>&1$`)
	match(t, testCall(t, dir, "cd sub && go test ./... | tail -20", "bash", "claude", map[string]any{"timeout": 120000.0}).Command, `^cd sub && '.*' slot --wait 60000 go test \./\.\.\. \| tail -20$`)
	match(t, testCall(t, dir, "bun test a.test.ts > out.txt", "bash", "claude", nil).Command, `^'.*' slot --wait \d+ bun test a\.test\.ts > out\.txt$`)
	match(t, testCall(t, dir, "node scripts/x.mjs", "pwsh", "claude", bg).Command, `^& '.*' slot node scripts/x\.mjs$`)
	for _, c := range []string{"FOO=1 bun run check > out.txt", "echo hi > out.txt", "bun run dev", "cd sub"} {
		if d := testCall(t, dir, c, "bash", "claude", bg); slicesContains(d.Rules, "slot") {
			t.Error("should run as it is:", c, d)
		}
	}
	for _, c := range []string{"Get-ChildItem > out.txt", "npm run dev"} {
		if d := testCall(t, dir, c, "pwsh", "claude", bg); slicesContains(d.Rules, "slot") {
			t.Error("should run as it is:", c, d)
		}
	}
	// In front, an unlisted command that is no test or build run stays in the agent's shell.
	for _, c := range []string{"git status 2>/dev/null", "git log -5", "node -e 1"} {
		if d := testCall(t, dir, c, "bash", "claude", nil); slicesContains(d.Rules, "slot") || slicesContains(d.Rules, "long-run") {
			t.Error("should run as it is:", c, d)
		}
	}
	// In front, the runner takes the slot itself.
	if d := testCall(t, dir, "bun run check", "bash", "claude", nil); !slicesContains(d.Rules, "long-run") || slicesContains(d.Rules, "slot") {
		t.Error(d)
	}
}

func TestBackgroundGateAfterShellPreparationGetsASlot(t *testing.T) {
	dir := t.TempDir()
	command := `git rev-parse HEAD && git status --short | wc -l && bun run verify:smoke > .scratch-gate.log 2>&1; echo "exit=$?" >> .scratch-gate.log`
	d := testCall(t, dir, command, "bash", "claude", map[string]any{"run_in_background": true, "timeout": 3600000.0})
	if !slicesContains(d.Rules, "slot") || !strings.Contains(d.Command, " slot bun run verify:smoke > .scratch-gate.log 2>&1") {
		t.Fatalf("background gate bypassed the heavy-job slot: %+v", d)
	}
}

func TestVerificationPlanAndScenarioListDoNotWaitForAHeavySlot(t *testing.T) {
	dir := t.TempDir()
	for _, command := range []string{"bun run verify:smoke --plan", "bun run smoke:ci --list"} {
		for _, background := range []bool{false, true} {
			d := testCall(t, dir, command, "bash", "claude", map[string]any{"run_in_background": background})
			if slicesContains(d.Rules, "slot") || slicesContains(d.Rules, "long-run") {
				t.Errorf("read-only gate discovery waited for a heavy slot: %s: %+v", command, d)
			}
		}
	}
}

func TestSlotsAreLimitedAndComeBackWhenReleased(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("FAIRSHARE_HEAVY_SLOTS", "2")
	t.Setenv(heldEnv, "")
	a, err := acquireSlot("bun run check", "C:/repo-a", "", heavyNeed{}, time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := acquireSlot("bun test", "C:/repo-b", "", heavyNeed{}, time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	var said string
	if _, err := acquireSlot("go test ./...", "C:/repo-c", "", heavyNeed{}, 1500*time.Millisecond, func(m string) { said = m }); !errors.Is(err, errBusy) {
		t.Fatal("a third slot was given out:", err)
	}
	if !strings.Contains(said, "`bun run check` in repo-a") || !strings.Contains(said, "`bun test` in repo-b") {
		t.Error("the wait note should name who holds the slots:", said)
	}
	a.Close()
	c, err := acquireSlot("go test ./...", "C:/repo-c", "", heavyNeed{}, time.Second, nil)
	if err != nil {
		t.Fatal("a released slot did not come back:", err)
	}
	c.Close()
	b.Close()

	// Work that already holds a slot never waits for another one.
	t.Setenv(heldEnv, "1")
	for range 5 {
		if _, err := acquireSlot("nested", "C:/x", "", heavyNeed{}, time.Millisecond, nil); err != nil {
			t.Fatal("nested work waited:", err)
		}
	}
}

func TestOlderSlotWaiterStartsBeforeNewArrival(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("FAIRSHARE_HEAVY_SLOTS", "1")
	t.Setenv(heldEnv, "")
	holder, err := acquireSlot("holding", "C:/repo", "", heavyNeed{}, time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	type acquired struct {
		name string
		slot *heavySlot
		err  error
	}
	ready := make(chan struct{})
	results := make(chan acquired, 2)
	go func() {
		s, e := acquireSlot("older", "C:/repo", "", heavyNeed{}, 10*time.Second, func(string) { close(ready) })
		results <- acquired{"older", s, e}
	}()
	<-ready
	holder.Close()
	go func() {
		s, e := acquireSlot("newer", "C:/repo", "", heavyNeed{}, 10*time.Second, nil)
		results <- acquired{"newer", s, e}
	}()
	first := <-results
	if first.err != nil {
		t.Fatal(first.err)
	}
	first.slot.Close()
	second := <-results
	if second.err != nil {
		t.Fatal(second.err)
	}
	second.slot.Close()
	if first.name != "older" {
		t.Fatalf("%s took the slot ahead of the already-waiting %s", first.name, second.name)
	}
}

func TestSlotQueueErrorIsNotReportedAsAWaitTimeout(t *testing.T) {
	if os.Getenv("FAIRSHARE_TEST_SLOT_FAILURE") == "1" {
		os.Exit(cmdSlot([]string{"cmd", "/c", "exit", "0"}))
	}
	root := t.TempDir()
	t.Setenv("LOCALAPPDATA", root)
	t.Setenv(heldEnv, "")
	// The command is listed, so it uses the queue; the queue's folder is a file, so it cannot.
	cwd, _ := os.Getwd()
	if err := editHeavy(func([]heavyEntry) []heavyEntry {
		return []heavyEntry{{Repo: repoID(cwd), Cmd: heavyKey("cmd /c exit 0"), CPUSeconds: 300}}
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(slotsDir(), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestSlotQueueErrorIsNotReportedAsAWaitTimeout$")
	child.Env = append(os.Environ(), "FAIRSHARE_TEST_SLOT_FAILURE=1")
	out, err := child.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 || !strings.Contains(string(out), "queue") {
		t.Fatalf("queue setup failure should exit 1 and name the queue: %v, %s", err, out)
	}
}

func TestSlotPassesChildExitStatus(t *testing.T) {
	if os.Getenv("FAIRSHARE_TEST_SLOT_EXIT") == "1" {
		os.Exit(cmdSlot([]string{"cmd", "/c", "exit", "7"}))
	}
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv(heldEnv, "")
	child := exec.Command(os.Args[0], "-test.run=^TestSlotPassesChildExitStatus$")
	child.Env = append(os.Environ(), "FAIRSHARE_TEST_SLOT_EXIT=1")
	out, err := child.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 7 {
		t.Fatalf("slot should return its child's exit 7: %v, %s", err, out)
	}
}

func TestKilledSlotWaiterDoesNotBlockQueue(t *testing.T) {
	if os.Getenv("FAIRSHARE_TEST_WAIT_SLOT") == "1" {
		_, _ = acquireSlot("abandoned", "C:/repo", "", heavyNeed{}, 0, func(string) { _, _ = os.Stdout.WriteString("waiting\n") })
		os.Exit(0)
	}
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("FAIRSHARE_HEAVY_SLOTS", "1")
	t.Setenv(heldEnv, "")
	holder, err := acquireSlot("holding", "C:/repo", "", heavyNeed{}, time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestKilledSlotWaiterDoesNotBlockQueue$")
	child.Env = append(os.Environ(), "FAIRSHARE_TEST_WAIT_SLOT=1")
	out, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	buf := make([]byte, 32)
	n, _ := out.Read(buf)
	if !strings.Contains(string(buf[:n]), "waiting") {
		t.Fatalf("waiter did not queue: %q", buf[:n])
	}
	_ = child.Process.Kill()
	_ = child.Wait()
	holder.Close()
	next, err := acquireSlot("next", "C:/repo", "", heavyNeed{}, 3*time.Second, nil)
	if err != nil {
		t.Fatal("dead waiter blocked its successor:", err)
	}
	next.Close()
	entries, err := os.ReadDir(slotsDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".wait") {
			t.Fatal("stale ticket:", entry.Name())
		}
	}
}

// A run that is killed while it holds a slot must not keep it.
func TestAKilledHolderLosesItsSlot(t *testing.T) {
	if os.Getenv("FAIRSHARE_TEST_HOLD_SLOT") == "1" {
		if _, err := acquireSlot("holder", "C:/held", "", heavyNeed{}, time.Second, nil); err != nil {
			os.Exit(3)
		}
		os.Stdout.WriteString("held\n")
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	local := t.TempDir()
	t.Setenv("LOCALAPPDATA", local)
	t.Setenv("FAIRSHARE_HEAVY_SLOTS", "1")
	t.Setenv(heldEnv, "")
	child := exec.Command(os.Args[0], "-test.run=^TestAKilledHolderLosesItsSlot$")
	child.Env = append(os.Environ(), "FAIRSHARE_TEST_HOLD_SLOT=1")
	out, _ := child.StdoutPipe()
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { child.Process.Kill(); child.Wait() })
	buf := make([]byte, 64)
	n, _ := out.Read(buf)
	if !strings.Contains(string(buf[:n]), "held") {
		t.Fatal("helper never took the slot:", string(buf[:n]))
	}
	if _, err := acquireSlot("waiter", "C:/w", "", heavyNeed{}, 1200*time.Millisecond, nil); !errors.Is(err, errBusy) {
		t.Fatal("the slot was free while the helper held it:", err)
	}
	child.Process.Kill()
	child.Wait()
	s, err := acquireSlot("waiter", "C:/w", "", heavyNeed{}, 3*time.Second, nil)
	if err != nil {
		t.Fatal("a killed holder kept its slot:", err)
	}
	s.Close()
}
