package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Two fake chats (cmd.exe stands in for the harness): one runs twice as many busy threads as the
// PC has cores, the other 2. Without lanes every thread gets the same CPU; with them each of the
// light chat's threads must get far more than each of the heavy chat's, and the heavy chat must
// show it was held back. Both run at idle priority, so other work on the PC slows both alike: the
// check compares the two chats, never CPU against a fixed amount, which failed whenever other
// chats were busy (2026-11-03).
func TestLanesGiveALightChatItsShare(t *testing.T) {
	h, l := laneRun2(t, true)
	// Measured 2026-11-03: 2.3-2.9x with lanes, 0.6-0.8x without.
	if r := perThreadRatio(h, l); r < 1.6 {
		t.Errorf("each light-chat thread got %.1fx the CPU of a heavy-chat thread, want at least 1.6x: %v", r, l)
	}
	if h["holds"].(float64) < 1 || h["held_ms"].(float64) < 2000 {
		t.Errorf("heavy chat was not held back: %v", h)
	}
	if l["holds"].(float64) != 0 {
		t.Errorf("light chat was held back: %v", l)
	}
	if !strings.Contains(h["top"].(string), "fairshare.exe") || h["harness"] != "cmd.exe" || h["peak_cores"].(float64) < 4 {
		t.Errorf("heavy lane event: %v", h)
	}
}

// The same chats without holding back: the light chat must lose out, or the test above proves nothing.
func TestWithoutBalanceALightChatLosesOut(t *testing.T) {
	h, l := laneRun2(t, false)
	if r := perThreadRatio(h, l); r > 1.2 {
		t.Errorf("without balance each light-chat thread still got %.1fx a heavy-chat thread's CPU, so the PC was not contended (want under 1.2x)", r)
	}
	if h["holds"].(float64) != 0 {
		t.Errorf("held back without --balance: %v", h)
	}
}

// Work that is already inside another job when adopt finds it (here `fairshare run`'s, like the
// Store PowerShell's or an exec's) cannot join the chat's job, so it gets a side job; throttle then
// caps the chat across all its jobs. Spin 8 threads for 12 s, started before adopt: about 96 CPU-s
// uncapped, about 45 capped at 2 cores from 3.5 s in.
func TestThrottleCapsAChatAcrossItsJobs(t *testing.T) {
	bin := fairshareBin(t)
	dir := t.TempDir()
	env := append(os.Environ(), "FAIRSHARE_EVENTS="+filepath.Join(dir, "events.jsonl"))
	c := exec.Command("cmd", "/c", fmt.Sprintf("%s run -- %s spin 8 12", bin, bin))
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Process.Kill() })
	time.Sleep(2500 * time.Millisecond) // the spin is in run's job before adopt looks
	adopt := exec.Command(bin, "adopt","--for", "12s", "--pid", fmt.Sprint(c.Process.Pid))
	adopt.Env = env
	if err := adopt.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	th := exec.Command(bin, "throttle", fmt.Sprint(c.Process.Pid), fmt.Sprint(200/float64(runtime.NumCPU())))
	th.Env = env
	if out, err := th.CombinedOutput(); err != nil || !strings.Contains(string(out), "capped") {
		t.Fatal(err, string(out))
	}
	time.Sleep(4 * time.Second)
	ls := exec.Command(bin, "throttle")
	ls.Env = env
	list, _ := ls.CombinedOutput()
	adopt.Wait()
	l := laneEvents(t, filepath.Join(dir, "events.jsonl"))[float64(c.Process.Pid)]
	t.Logf("lane: %v\nlist: %s", l, list)
	if l == nil || l["side_jobs"].(float64) < 1 || l["outside"].(float64) != 0 {
		t.Fatalf("work inside another job did not get a side job: %v", l)
	}
	// the jobs count the whole run: 3.5 s at 8 cores, then 8.5 s at 2, about 45 s
	if cpu := l["cpu_ms"].(float64); cpu > 60000 || cpu < 25000 {
		t.Errorf("throttled chat used %.0f ms CPU, want 25000-60000 (uncapped about 96000)", cpu)
	}
	if !strings.Contains(string(list), "capped at") || !strings.Contains(string(list), fmt.Sprint(c.Process.Pid)) {
		t.Errorf("throttle list does not show the cap: %s", list)
	}
}

// A chat's first command runs inside a job that kills its processes when it closes (fairshare
// run's, like exec's); a second command starts after it with no job. When the first run ends,
// the second must live on: the chat's job must not have been nested inside the first run's.
func TestOneRunEndingDoesNotKillTheChatsOtherWork(t *testing.T) {
	bin := fairshareBin(t)
	c := exec.Command("cmd", "/c", fmt.Sprintf("start /b %s run -- %s spin 1 4 & ping -n 2 127.0.0.1 >nul & ping -n 8 127.0.0.1 >nul", bin, bin))
	t0 := time.Now()
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Process.Kill() })
	adopt := exec.Command(bin, "adopt","--for", "12s", "--pid", fmt.Sprint(c.Process.Pid))
	adopt.Env = append(os.Environ(), "FAIRSHARE_EVENTS="+filepath.Join(t.TempDir(), "events.jsonl"))
	time.Sleep(500 * time.Millisecond) // the run and its job exist; the first ping too
	if err := adopt.Start(); err != nil {
		t.Fatal(err)
	}
	defer adopt.Wait()
	c.Wait()
	// the run ends at about 4 s; the second ping, started at about 1 s, needs about 7 s
	if d := time.Since(t0); d < 7500*time.Millisecond {
		t.Errorf("the chat's second command died with the first run: the chat finished after %s, want about 8.5s", d.Round(100*time.Millisecond))
	}
}

func laneEvents(t *testing.T, file string) map[float64]map[string]any {
	lanes := map[float64]map[string]any{}
	b, _ := os.ReadFile(file)
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var e map[string]any
		json.Unmarshal([]byte(line), &e)
		if e["ev"] == "lane" {
			lanes[e["pid"].(float64)] = e
		}
	}
	return lanes
}

const laneSecs = 7

// perThreadRatio is the CPU one light-chat thread got over what one heavy-chat thread got.
func perThreadRatio(heavy, light map[string]any) float64 {
	return (light["cpu_ms"].(float64) / 2) / (heavy["cpu_ms"].(float64) / float64(2*runtime.NumCPU()))
}

func laneRun2(t *testing.T, balance bool) (heavyLane, lightLane map[string]any) {
	bin := fairshareBin(t)
	const secs = laneSecs
	chat := func(threads int) *exec.Cmd {
		c := exec.Command("cmd", "/c", fmt.Sprintf("ping -n 2 127.0.0.1 >nul & %s spin %d %d", bin, threads, secs))
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Process.Kill() })
		return c
	}
	heavy, light := chat(2*runtime.NumCPU()), chat(2)
	events := filepath.Join(t.TempDir(), "events.jsonl")
	args := []string{"adopt","--for", "11s", "--pid", fmt.Sprintf("%d,%d", heavy.Process.Pid, light.Process.Pid)}
	if balance {
		args = append(args, "--balance")
	}
	adopt := exec.Command(bin, args...)
	adopt.Env = append(os.Environ(), "FAIRSHARE_EVENTS="+events)
	if out, err := adopt.CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	lanes := laneEvents(t, events)
	h, l := lanes[float64(heavy.Process.Pid)], lanes[float64(light.Process.Pid)]
	if h == nil || l == nil {
		t.Fatalf("missing lane events: %v", lanes)
	}
	t.Logf("heavy: cpu %v ms, held %v ms; light: cpu %v ms of %d wanted", h["cpu_ms"], h["held_ms"], l["cpu_ms"], 2*secs*1000)
	// Both chats run at idle priority, so a PC busy with other work gives them too little to
	// compare (2026-11-03: 3 of 24 cores, while other chats ran tests). That is no answer either way.
	if got, pc := h["cpu_ms"].(float64)+l["cpu_ms"].(float64), float64(runtime.NumCPU()*secs*1000); got < pc/3 {
		t.Skipf("the PC was too busy to measure sharing: these chats got %.0f%% of it (need a third); run again when it is quieter", 100*got/pc)
	}
	return h, l
}
