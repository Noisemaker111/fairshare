package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Noisemaker111/fairshare/internal/windows"
)

func fileTimeNow() uint64 { return uint64(time.Now().UnixNano()/100 + 116444736000000000) }

// The child side of TestStorePwshStartsUnderTwoPlacers: at idle priority, as adopt runs a chat's
// shell, start the Store PowerShell FAIRSHARE_SPAWN_N times and print each failure.
func TestSpawnRaceStarter(t *testing.T) {
	if os.Getenv("FAIRSHARE_SPAWN_STARTER") == "" {
		t.Skip()
	}
	windows.SetPriorityClass(windows.CurrentProcess(), windows.IDLE_PRIORITY_CLASS)
	n, _ := strconv.Atoi(os.Getenv("FAIRSHARE_SPAWN_N"))
	fails := 0
	for i := 0; i < n; i++ {
		a := fileTimeNow()
		if out, err := exec.Command("pwsh", "-NoProfile", "-Command", "exit 0").CombinedOutput(); err != nil {
			fails++
			fmt.Printf("fail %d %d %d: %v %s\n", i, a, fileTimeNow(), err, strings.TrimSpace(string(out)))
		}
	}
	fmt.Printf("done %d failed %d\n", n, fails)
}

type placement struct {
	sweeper   int
	pid       uint32
	created   uint64
	ageMs     float64
	unstarted bool
	inJob     bool
	result    string
}

// The reproduction of "Access is denied" on starting the Store PowerShell (see unstarted). Two
// placers, each with a lane like adopt's, put every process a child starts into a job, while the
// child starts pwsh again and again. It starts 150 PowerShells, so it runs only when asked:
//
//	FAIRSHARE_SPAWN_REPRO=thread go test -run TestStorePwshStartsUnderTwoPlacers -v .
//
// thread is fairshare's rule (wait until the first thread runs) and must fail no start. none
// (place at once) shows the failure, about 3% of starts; time (the 1 s wait used before
// 2026-11-08) fails none but governs almost no pwsh. FAIRSHARE_SPAWN_BURN=n adds n busy threads
// above the child's priority.
func TestStorePwshStartsUnderTwoPlacers(t *testing.T) {
	rule := os.Getenv("FAIRSHARE_SPAWN_REPRO")
	if rule == "" {
		t.Skip("set FAIRSHARE_SPAWN_REPRO=thread, none or time")
	}
	var stop atomic.Bool
	burn, _ := strconv.Atoi(os.Getenv("FAIRSHARE_SPAWN_BURN"))
	for i := 0; i < burn; i++ {
		go func() {
			runtime.LockOSThread()
			setThreadPriority.Call(uintptr(windows.Handle(^uintptr(1))), 2)
			x := 0
			for !stop.Load() {
				x++
			}
		}()
	}
	defer stop.Store(true)
	n := 150
	if v, err := strconv.Atoi(os.Getenv("FAIRSHARE_SPAWN_N")); err == nil {
		n = v
	}
	sp := exec.Command(os.Args[0], "-test.run=^TestSpawnRaceStarter$", "-test.v")
	sp.Env = append(os.Environ(), "FAIRSHARE_SPAWN_STARTER=1", "FAIRSHARE_SPAWN_N="+strconv.Itoa(n))
	out, _ := sp.StdoutPipe()
	if err := sp.Start(); err != nil {
		t.Fatal(err)
	}
	root := uint32(sp.Process.Pid)
	var mu sync.Mutex
	var events []placement
	var wg sync.WaitGroup
	for w := 0; w < 2; w++ {
		p := DefaultPolicy()
		p.FreezeBelowMB = 0
		gov, err := NewGovernor(p, nil)
		if err != nil {
			t.Fatal(err)
		}
		u, err := gov.NewUnit(false, "")
		if err != nil {
			t.Fatal(err)
		}
		l := newLane(gov, 0, 0, "spawn race", u)
		defer l.close(gov, "over", nil)
		wg.Add(1)
		go func() {
			defer wg.Done()
			runtime.LockOSThread()
			setThreadPriority.Call(uintptr(windows.Handle(^uintptr(1))), 15) // adopt is not starved
			done := map[uint32]bool{}
			for !stop.Load() {
				procs := snapshot()
				for _, pid := range descendants(procs, root) {
					info := procs[pid]
					if done[pid] {
						continue
					}
					switch rule {
					case "time": // the 1 s wait from PR #11
						if fileTimeNow() < info.created+1e7 {
							continue
						}
					case "thread":
						if info.unstarted {
							continue
						}
					}
					done[pid] = true
					h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_SET_INFORMATION|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
					if err != nil {
						continue
					}
					e := placement{sweeper: w, pid: pid, created: info.created, ageMs: float64(fileTimeNow()-info.created) / 1e4, unstarted: info.unstarted, inJob: inJob(h, 0)}
					if l.jobs.has(h) {
						e.result = "already"
					} else {
						e.result = l.place(gov, pid, h)
					}
					windows.CloseHandle(h)
					mu.Lock()
					events = append(events, e)
					mu.Unlock()
				}
				time.Sleep(2 * time.Millisecond)
			}
		}()
	}
	type span struct{ a, b uint64 }
	var fails []span
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		l := sc.Text()
		if strings.HasPrefix(l, "fail") {
			var i int
			var s span
			fmt.Sscanf(l, "fail %d %d %d", &i, &s.a, &s.b)
			fails = append(fails, s)
			t.Log(l)
		}
		if strings.HasPrefix(l, "done") {
			t.Log(l)
		}
	}
	sp.Wait()
	stop.Store(true)
	wg.Wait()
	counts := map[string]int{}
	governed := map[uint32]bool{}
	for _, e := range events {
		key := fmt.Sprintf("unstarted=%v injob=%v %s", e.unstarted, e.inJob, strings.SplitN(e.result, ":", 2)[0])
		counts[key]++
		governed[e.pid] = true
	}
	for k, v := range counts {
		t.Logf("%4d  %s", v, k)
	}
	t.Logf("pwsh processes a sweeper got to: %d", len(governed))
	if rule == "thread" && len(fails) > 0 {
		defer t.Errorf("%d of %d Store PowerShell starts failed", len(fails), n)
	}
	for _, f := range fails {
		for _, e := range events {
			if e.created >= f.a && e.created <= f.b {
				t.Logf("failed start: sweeper %d pid %d age %.1fms unstarted=%v injob=%v -> %s", e.sweeper, e.pid, e.ageMs, e.unstarted, e.inJob, e.result)
			}
		}
	}
}
