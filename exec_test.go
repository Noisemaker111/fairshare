package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	binOnce sync.Once
	binErr  string
)

// fairshareBin builds the program once for tests that run it as agents do.
func fairshareBin(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(os.TempDir(), "fairshare-test-bin", "fairshare.exe")
	binOnce.Do(func() {
		out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
		if err != nil {
			binErr = string(out)
		}
	})
	if binErr != "" {
		t.Fatal(binErr)
	}
	return bin
}

func runExec(t *testing.T, shell, cmd string, deadlineMs int, env ...string) (string, int, map[string]any) {
	t.Helper()
	events := filepath.Join(t.TempDir(), "events.jsonl")
	c := exec.Command(fairshareBin(t), "exec", shell, base64.StdEncoding.EncodeToString([]byte(cmd)), itoa(deadlineMs), "test:abc")
	// Its own LOCALAPPDATA: the run reads and writes the heavy list and slots there, not the PC's.
	c.Env = append(append(os.Environ(), "FAIRSHARE_EVENTS="+events, "LOCALAPPDATA="+filepath.Dir(events)), env...)
	out, _ := c.Output()
	code := c.ProcessState.ExitCode()
	var ev map[string]any
	if b, err := os.ReadFile(events); err == nil {
		json.Unmarshal([]byte(strings.TrimSpace(string(b))), &ev)
	}
	return string(out), code, ev
}

func TestFailingRunKeepsErrorLinesTailAndExitCode(t *testing.T) {
	out, code, ev := runExec(t, "pwsh", `1..150 | ForEach-Object { if ($_ % 50 -eq 0) { "line $_ error: boom"; "  at frame $_" } else { "line $_ ok" } }; exit 3`, 0)
	if code != 3 {
		t.Fatal("exit", code, out)
	}
	match(t, out, `^\[fairshare\] exit 3 · \d+ lines · full log: `)
	match(t, out, `: line 50 error: boom\n\d+:   at frame 50`)
	if n := len(strings.Split(out, "\n")); n >= 60 {
		t.Error("summary too long:", n)
	}
	if ev["exit"] != 3.0 || ev["label"] != "test:abc" || ev["out_lines"].(float64) < 150 || ev["procs"] == nil || ev["cpu_ms"] == nil {
		t.Error("event", ev)
	}
}

func TestPassingRunShowsLast20Lines(t *testing.T) {
	out, code, _ := runExec(t, "pwsh", `1..150 | ForEach-Object { "line $_ ok" }`, 0)
	if code != 0 || len(strings.Split(strings.TrimSpace(out), "\n")) != 21 || !strings.HasSuffix(out, "line 150 ok\n") {
		t.Error(code, out)
	}
}

func TestShortOutputPassesThrough(t *testing.T) {
	out, code, _ := runExec(t, "bash", "echo short; echo two", 0)
	if code != 0 || out != "short\ntwo\n" {
		t.Errorf("%d %q", code, out)
	}
}

func TestRunPastItsDeadlineIsStoppedWithItsTree(t *testing.T) {
	t0 := time.Now()
	// a grandchild that would hold the pipe open if the stop missed it
	out, code, ev := runExec(t, "bash", "echo started; (sleep 30; echo late) & sleep 20; echo never", 1500)
	if code != 124 || !strings.Contains(out, "stopped after 2s") || !strings.Contains(out, "started") || strings.Contains(out, "never") {
		t.Errorf("%d %q", code, out)
	}
	if d := time.Since(t0); d > 8*time.Second {
		t.Error("stop took", d)
	}
	if ev["stopped"] != "deadline" {
		t.Error(ev)
	}
}

func TestSilentIdleRunIsStoppedAsHung(t *testing.T) {
	out, code, ev := runExec(t, "bash", "echo waiting for input; sleep 60; echo never", 0, "FAIRSHARE_HUNG_AFTER=12s")
	if code != 124 || !strings.Contains(out, "every thread was blocked") || ev["stopped"] != "hung" {
		t.Errorf("%d %q %v", code, out, ev)
	}
	if ms := ev["wall_ms"].(float64); ms > 25000 {
		t.Error("hung run stopped late:", ms)
	}
}

func TestThreadStatesTellRunningWaitingAndFrozen(t *testing.T) {
	self := uint32(os.Getpid())
	if st, ok := threadStates(map[uint32]bool{self: true}); !ok || st.Running < 1 {
		t.Fatal("this thread is running:", st, ok)
	}
	c := exec.Command("ping", "-n", "30", "127.0.0.1")
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	defer c.Process.Kill()
	pid := uint32(c.Process.Pid)
	time.Sleep(500 * time.Millisecond)
	if st, _ := threadStates(map[uint32]bool{pid: true}); st.Waiting < 1 || st.Running+st.Ready > 0 {
		t.Error("ping waits:", st)
	}
	setSuspended(pid, ntSuspendProcess)
	if st, _ := threadStates(map[uint32]bool{pid: true}); st.Suspended < 1 || st.Waiting > 0 {
		t.Error("suspended ping:", st)
	}
	setSuspended(pid, ntResumeProcess)
}

func TestSilentBusyRunIsNotHung(t *testing.T) {
	// 20 s of silent CPU work must finish: busy is working, not waiting
	out, code, ev := runExec(t, "pwsh", `$t=[Diagnostics.Stopwatch]::StartNew(); while ($t.Elapsed.TotalSeconds -lt 20) { $null = 1+1 }; "done"`, 0, "FAIRSHARE_HUNG_AFTER=8s")
	if code != 0 || !strings.Contains(out, "done") || ev["stopped"] != nil {
		t.Errorf("%d %q %v", code, out, ev)
	}
	// the Store PowerShell breaks out of the run's job; it must land in a side job, counted
	if ev["outside_job"] != nil || ev["cpu_ms"].(float64) < 10000 {
		t.Errorf("busy PowerShell escaped the run's jobs or went uncounted: %v", ev)
	}
}
