package main

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestHeavyRunsAreWholeSuitesNotOneFile(t *testing.T) {
	for _, c := range []string{"bun run check", "bun test", "bun run verify:smoke", "bun run verify:fast", "go test ./...", "bun run lane:close -m \"Fixed: x\"", "node scripts/check.mjs race", "npx tsc --noEmit", "bun run test:dev-tools 2>&1 | tail -5"} {
		if !heavyRun(c) {
			t.Error("should be heavy:", c)
		}
	}
	for _, c := range []string{"bun test packages/core/src/a.test.ts", "go test ./internal/drive/", "go test ./... -run TestStart", "bun install", "npm ci", "bun run dev", "git status", "bun scripts/x.ts"} {
		if heavyRun(c) {
			t.Error("should be light:", c)
		}
	}
}

func TestWholeSuitesOutsideTheRunnerWaitForASlot(t *testing.T) {
	dir := t.TempDir()
	bg := map[string]any{"run_in_background": true}
	match(t, testCall(t, dir, "bun run verify:smoke", "bash", "claude", bg).Command, `^'.*' slot bun run verify:smoke$`)
	// In front, the wait is half the agent's time limit, so the call answers before it is cut off.
	match(t, testCall(t, dir, "bun run check > .scratch/check.log 2>&1", "bash", "claude", nil).Command, `^'.*' slot --wait 300000 bun run check > \.scratch/check\.log 2>&1$`)
	match(t, testCall(t, dir, "cd sub && go test ./... | tail -20", "bash", "claude", map[string]any{"timeout": 120000.0}).Command, `^cd sub && '.*' slot --wait 60000 go test \./\.\.\. \| tail -20$`)
	for _, c := range []string{"bun test a.test.ts > out.txt", "FOO=1 bun run check > out.txt", "git log > out.txt"} {
		if d := testCall(t, dir, c, "bash", "claude", bg); slicesContains(d.Rules, "heavy-slot") {
			t.Error("should not wait for a slot:", c, d)
		}
	}
	// In front, the runner takes the slot itself.
	if d := testCall(t, dir, "bun run check", "bash", "claude", nil); !slicesContains(d.Rules, "long-run") || slicesContains(d.Rules, "heavy-slot") {
		t.Error(d)
	}
}

func TestBackgroundGateAfterShellPreparationGetsASlot(t *testing.T) {
	dir := t.TempDir()
	command := `git rev-parse HEAD && git status --short | wc -l && bun run verify:smoke > .scratch-gate.log 2>&1; echo "exit=$?" >> .scratch-gate.log`
	d := testCall(t, dir, command, "bash", "claude", map[string]any{"run_in_background": true, "timeout": 3600000.0})
	if !slicesContains(d.Rules, "heavy-slot") || !strings.Contains(d.Command, " slot bun run verify:smoke > .scratch-gate.log 2>&1") {
		t.Fatalf("background gate bypassed the heavy-job slot: %+v", d)
	}
}

func TestVerificationPlanAndScenarioListDoNotWaitForAHeavySlot(t *testing.T) {
	dir := t.TempDir()
	for _, command := range []string{"bun run verify:smoke --plan", "bun run smoke:ci --list"} {
		for _, background := range []bool{false, true} {
			d := testCall(t, dir, command, "bash", "claude", map[string]any{"run_in_background": background})
			if slicesContains(d.Rules, "heavy-slot") || slicesContains(d.Rules, "long-run") {
				t.Errorf("read-only gate discovery waited for a heavy slot: %s: %+v", command, d)
			}
		}
	}
}

func TestSlotsAreLimitedAndComeBackWhenReleased(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("FAIRSHARE_HEAVY_SLOTS", "2")
	t.Setenv(heldEnv, "")
	a, err := acquireSlot("bun run check", "C:/repo-a", "", time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := acquireSlot("bun test", "C:/repo-b", "", time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	var said string
	if _, err := acquireSlot("go test ./...", "C:/repo-c", "", 1500*time.Millisecond, func(m string) { said = m }); !errors.Is(err, errBusy) {
		t.Fatal("a third slot was given out:", err)
	}
	if !strings.Contains(said, "`bun run check` in repo-a") || !strings.Contains(said, "`bun test` in repo-b") {
		t.Error("the wait note should name who holds the slots:", said)
	}
	// A run in front gives up at its --wait and does not start the command.
	marker := t.TempDir() + `\ran`
	if code := cmdSlot([]string{"--wait", "1200", "cmd", "/c", "echo x > " + marker}); code != 124 {
		t.Error("slot --wait with no slot free should not start, exit", code)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the command ran without a slot")
	}
	a.Close()
	if code := cmdSlot([]string{"--wait", "1200", "cmd", "/c", "exit 7"}); code != 7 {
		t.Error("slot should run the command once a slot is free and pass on its code, got", code)
	}
	c, err := acquireSlot("go test ./...", "C:/repo-c", "", time.Second, nil)
	if err != nil {
		t.Fatal("a released slot did not come back:", err)
	}
	c.Close()
	b.Close()

	// Work that already holds a slot never waits for another one.
	t.Setenv(heldEnv, "1")
	for range 5 {
		if _, err := acquireSlot("nested", "C:/x", "", time.Millisecond, nil); err != nil {
			t.Fatal("nested work waited:", err)
		}
	}
}

// A run that is killed while it holds a slot must not keep it.
func TestAKilledHolderLosesItsSlot(t *testing.T) {
	if os.Getenv("FAIRSHARE_TEST_HOLD_SLOT") == "1" {
		if _, err := acquireSlot("holder", "C:/held", "", time.Second, nil); err != nil {
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
	if _, err := acquireSlot("waiter", "C:/w", "", 1200*time.Millisecond, nil); !errors.Is(err, errBusy) {
		t.Fatal("the slot was free while the helper held it:", err)
	}
	child.Process.Kill()
	child.Wait()
	s, err := acquireSlot("waiter", "C:/w", "", 3*time.Second, nil)
	if err != nil {
		t.Fatal("a killed holder kept its slot:", err)
	}
	s.Close()
}
