package main

import (
	"os/exec"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// A suspended chat looks hung to whoever is using it, so freezing is opt-in. With memory faked to
// nothing free, the default policies (run's and adopt's) must never suspend a process; the same
// setup with --freeze must, or the check proves nothing.
func TestDefaultConfigNeverSuspendsAProcess(t *testing.T) {
	real := freezeMemStatus
	freezeMemStatus = func() (uint64, uint64, uint64) { return 0, 0, 100 }
	defer func() { freezeMemStatus = real }()

	for _, c := range []struct {
		name    string
		p       Policy
		suspend bool
	}{
		{"run default", DefaultPolicy(), false},
		{"adopt default", adoptPolicy(false, 0, 1), false},
		{"adopt --freeze", adoptPolicy(true, 0, 1), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			freezes, suspended := suspensionsUnder(t, c.p)
			if c.suspend && (freezes == 0 || suspended == 0) {
				t.Fatalf("--freeze with no memory free froze nothing (freezes %d, suspended threads seen %d), so this check cannot see a freeze", freezes, suspended)
			}
			if !c.suspend && (freezes != 0 || suspended != 0) {
				t.Fatalf("default config suspended work: %d freezes, %d suspended threads seen", freezes, suspended)
			}
		})
	}
}

// suspensionsUnder puts three running programs in three chats' lanes under p, watches them for
// 2 s, and returns the governor's freeze count and the most suspended threads seen at once.
func suspensionsUnder(t *testing.T, p Policy) (int64, int) {
	gov, err := NewGovernor(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	pids := map[uint32]bool{}
	for range 3 {
		c := exec.Command("ping", "-n", "30", "127.0.0.1")
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Process.Kill(); c.Wait() })
		pid := uint32(c.Process.Pid)
		h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_SET_INFORMATION|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
		if err != nil {
			t.Fatal(err)
		}
		defer windows.CloseHandle(h)
		u, err := gov.NewUnit(true, "")
		if err != nil {
			t.Fatal(err)
		}
		l := newLane(gov, 0, 0, "test", u)
		defer l.close(gov, "test over") // thaws anything frozen
		if how := l.place(gov, pid, h); how == "outside" {
			t.Fatalf("ping %d stayed outside the lane", pid)
		}
		pids[pid] = true
		time.Sleep(50 * time.Millisecond) // distinct ages, so there is a youngest
	}
	most := 0
	for end := time.Now().Add(2 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if st, ok := threadStates(pids); ok {
			most = max(most, st.Suspended)
		}
	}
	return gov.Freezes.Load(), most
}
