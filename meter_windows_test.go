//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ownCPU is the processor-seconds this process has used.
func ownCPU() float64 {
	h, _ := syscall.GetCurrentProcess()
	var began, end, kernel, user syscall.Filetime
	syscall.GetProcessTimes(h, &began, &end, &kernel, &user)
	return float64(ftime(kernel)+ftime(user)) / 1e7
}

// A process that burns half a processor-second and ends, between two looks: the meter kept a
// handle on it from the look that saw it, so what it used after that look is still read.
func TestWhatAProcessUsedAfterTheLastLookIsReadOnceItHasEnded(t *testing.T) {
	if os.Getenv("FAIRSHARE_TEST_BURN") != "" {
		for ownCPU() < 0.6 {
		}
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestWhatAProcessUsedAfterTheLastLookIsReadOnceItHasEnded$")
	cmd.Env = append(os.Environ(), "FAIRSHARE_TEST_BURN=1")
	began := time.Now()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := uint32(cmd.Process.Pid)
	var m meter
	var seen *procLook
	for end := time.Now().Add(10 * time.Second); seen == nil && time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		procs, err := processes()
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range procs {
			if p.pid == pid {
				seen = &p
			}
			if p.pid != 0 {
				m.keep(p)
			}
		}
	}
	if seen == nil {
		t.Fatal("the process was never in a look")
	}
	if seen.cpu >= 0.55 {
		t.Skipf("the look caught the process with %.2f of its 0.6 processor-seconds used: nothing left to read", seen.cpu)
	}
	cmd.Wait()
	procs, err := processes()
	if err != nil {
		t.Fatal(err)
	}
	alive := map[procKey]float64{}
	for _, p := range procs {
		alive[procKey{p.pid, p.created}] = p.cpu
	}
	var got *endedProc
	for _, e := range m.ended(alive) {
		if e.seen.pid == pid {
			got = &e
		}
	}
	if got == nil {
		t.Fatalf("the process ended with %.2f processor-seconds seen and nothing was read of the rest", seen.cpu)
	}
	if total := seen.cpu + got.used; total < 0.6 || total > 1.5 {
		t.Fatalf("seen %.2f and read %.2f after it ended: %.2f processor-seconds, want the 0.6 it burned", seen.cpu, got.used, total)
	}
	if got.at.Before(began) || got.at.After(time.Now().Add(time.Second)) {
		t.Fatalf("it ended at %v, outside this test (%v to now)", got.at, began)
	}
	if _, still := m.held[procKey{pid, seen.created}]; still {
		t.Fatal("the meter still holds a process that ended")
	}
	if again := m.ended(alive); len(again) != 0 {
		t.Fatalf("a process that ended was counted twice: %+v", again)
	}
}

// Owl answers a setting it does not have with exit 0 and an error in its JSON. On 2026-11-11
// that was read as told, and an urgent hold stood for 45 minutes with nothing held.
func TestOwlRefusingASettingIsNotOwlTold(t *testing.T) {
	t.Setenv("FAIRSHARE_HOME", t.TempDir())
	t.Setenv("FAIRSHARE_RESERVE", "")
	dir := t.TempDir()
	refuses, takes := filepath.Join(dir, "refuses.cmd"), filepath.Join(dir, "takes.cmd")
	os.WriteFile(refuses, []byte("@echo {\"error\":\"yield_before is not a setting; the keys are enabled, reserve_processors\",\"usage\":true}\r\n"), 0o755)
	os.WriteFile(takes, []byte("@echo {\"key\":\"yield_before\",\"value\":\"1794421362\"}\r\n"), 0o755)
	err := tellYield(Settings{OwlPath: refuses}, time.Now())
	if err == nil || !strings.Contains(err.Error(), "Owl refused: yield_before is not a setting") {
		t.Fatalf("an Owl that refused the hold was read as told: %v", err)
	}
	if err := tellYield(Settings{OwlPath: takes}, time.Now()); err != nil {
		t.Fatalf("an Owl that took the hold was read as refusing: %v", err)
	}
	if err := applyReserve(Settings{OwlPath: refuses}, 2); err == nil {
		t.Fatal("an Owl that refused the reserve was read as told")
	}
}

func TestFramesAreTheUsualOneAndTheSlowest(t *testing.T) {
	now := time.Now()
	f := frameWatch{ms: []float64{20, 23, 21, 1900, 22}}
	if usual, worst, n := f.take(now); usual != 22 || worst != 1900 || n != 5 {
		t.Fatalf("usual %.0f worst %.0f of %d, want 22, 1900 of 5", usual, worst, n)
	}
	if _, _, n := f.take(now); n != 0 {
		t.Fatalf("frames were counted twice: %d", n)
	}
	// A frame that has been awaited for three seconds is a three-second frame already.
	f = frameWatch{ms: []float64{20}, began: now.Add(-3 * time.Second)}
	if _, worst, _ := f.take(now); worst != 3000 {
		t.Fatalf("a frame awaited for 3 s counts as %.0f ms", worst)
	}
	var w window
	w.add(sample{at: now.Add(-50 * time.Second), seconds: 5, frameMs: 20, frameWorst: 40, frames: 20}, longWindow)
	w.add(sample{at: now.Add(-45 * time.Second), seconds: 5, frameMs: 30, frameWorst: 1900, frames: 5}, longWindow)
	w.add(sample{at: now.Add(-40 * time.Second), seconds: 5}, longWindow) // nobody at the PC: not timed
	if usual, worst := w.frameTimes(now, shortWindow); usual != 22 || worst != 1900 {
		t.Fatalf("over the minute: usual %.0f worst %.0f, want 22 and 1900", usual, worst)
	}
	if usual, worst := w.frameTimes(now, 30*time.Second); usual != 0 || worst != 0 {
		t.Fatalf("with no frame timed: %.0f %.0f", usual, worst)
	}
}

// A real look at this PC, with the reserve worked out and not applied: a test never changes what
// Owl holds on the machine it runs on.
func TestALookMeasuresThisPC(t *testing.T) {
	t.Setenv("FAIRSHARE_HOME", t.TempDir())
	t.Setenv("FAIRSHARE_RESERVE", "measure")
	s := defaultSettings()
	var m meter
	if _, got, err := m.look(s, time.Now()); err != nil || got {
		t.Fatalf("the first look only sets the counters: got %v, err %v", got, err)
	}
	// Burn one processor so there is something to see.
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
		}
	}()
	time.Sleep(1200 * time.Millisecond)
	r, got, err := m.look(s, time.Now())
	close(stop)
	if err != nil || !got {
		t.Fatalf("second look: got %v, err %v", got, err)
	}
	if r.Processors < 1 || r.Capacity < 1 || r.Capacity > float64(r.Processors) {
		t.Fatalf("processors %d, capacity %.0f", r.Processors, r.Capacity)
	}
	if total := r.BusyShort + r.UserBusy; total < 0.5 || total > float64(r.Processors)+0.01 {
		t.Fatalf("%.2f processors busy on a PC of %d while this test burns one", total, r.Processors)
	}
	if r.MemTotalGB < 1 || r.MemAvailGB <= 0 || r.MemAvailGB > r.MemTotalGB {
		t.Fatalf("memory %.1f of %.1f GB", r.MemAvailGB, r.MemTotalGB)
	}
	if r.DiskFreeGB <= 0 {
		t.Fatalf("disk free %.1f GB on %s (%v)", r.DiskFreeGB, r.DiskPath, r.Notes)
	}
	if r.Presence.State == "" || r.Presence.State == "none" {
		t.Fatalf("presence %+v", r.Presence)
	}
	if m.perCore < 1 || m.perCore > 8 {
		t.Fatalf("%d logical processors a core", m.perCore)
	}
	// This test's own processes are found among the programs or the others.
	me := 0.0
	for _, p := range append(r.Programs, r.Others...) {
		if p.Name == "fairshare.test.exe" {
			me = p.Cores
		}
	}
	if me < 0.5 {
		t.Fatalf("this test burned a processor and the look saw %.2f for it: %+v %+v", me, r.Programs, r.Others)
	}
	if _, err := os.Stat(appsFile()); err == nil && r.Presence.State != "full-screen" {
		t.Fatal("apps.json written with nothing full screen")
	}
}

// An install puts the new program in place while the old one is running from the same path, as
// it is whenever a pool run carries a gate.
func TestANewProgramReplacesOneThatIsRunning(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "fairshare.exe")
	ping, err := os.ReadFile(filepath.Join(os.Getenv("SystemRoot"), "System32", "PING.EXE"))
	if err != nil {
		t.Skip("no ping.exe to run:", err)
	}
	if err := os.WriteFile(exe, ping, 0o755); err != nil {
		t.Fatal(err)
	}
	running := exec.Command(exe, "-n", "30", "127.0.0.1")
	if err := running.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { running.Process.Kill(); running.Wait() }()
	if err := os.WriteFile(exe, []byte("over"), 0o755); err == nil {
		t.Fatal("Windows let a running program be written over: the case this guards is not the case here")
	}
	if err := os.WriteFile(exe+".new", []byte("the new program"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := swapIn(exe); err != nil {
		t.Fatalf("the new program did not go in while the old one ran: %v", err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "the new program" {
		t.Fatalf("the installed program is %q", got)
	}
	if _, err := os.Stat(exe + ".new"); !os.IsNotExist(err) {
		t.Fatal("the new program was left beside it as well")
	}
	olds, _ := filepath.Glob(filepath.Join(dir, "fairshare.exe.*.old"))
	if len(olds) != 1 {
		t.Fatalf("the running program was not kept aside: %v", olds)
	}
	running.Process.Kill()
	running.Wait()
	os.WriteFile(exe+".new", []byte("the next program"), 0o755)
	if err := swapIn(exe); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(olds[0]); !os.IsNotExist(err) {
		t.Fatal("an old program nothing runs any more was not removed at the next install")
	}
}
