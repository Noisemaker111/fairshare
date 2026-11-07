package main

import (
	"bufio"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type leaseHelper struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	held  chan struct{}
}

// startLease runs `fairshare lease boat` in a child, as a repo's boat tool does.
func startLease(t *testing.T, label string) *leaseHelper {
	t.Helper()
	return startLeaseIn(t, "TestBoatLeasesCapMachinesAcrossProcesses", label)
}

func startLeaseIn(t *testing.T, test, label string) *leaseHelper {
	t.Helper()
	child := exec.Command(os.Args[0], "-test.run=^"+test+"$")
	child.Env = append(os.Environ(), "FAIRSHARE_TEST_LEASE="+label)
	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	h := &leaseHelper{cmd: child, stdin: stdin, held: make(chan struct{})}
	go func() {
		s := bufio.NewScanner(stdout)
		for s.Scan() {
			if s.Text() == "held" {
				close(h.held)
			}
		}
	}()
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	return h
}

// fakeBoatList makes `boat list` show these machine ids, and returns a function that changes them.
func fakeBoatList(t *testing.T, ids string) func(string) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "boat-list")
	set := func(ids string) {
		if err := os.WriteFile(file, []byte(ids), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	set(ids)
	t.Setenv("BOAT_EXE", os.Args[0])
	t.Setenv("FAIRSHARE_TEST_BOAT_LIST", file)
	return set
}

func holds(h *leaseHelper, within time.Duration) bool {
	select {
	case <-h.held:
		return true
	case <-time.After(within):
		return false
	}
}

// Leases started at the same moment from separate processes never exceed BOAT_MAX_MACHINES; one
// frees when its owner closes stdin or is killed, and the heavy-job slots are a separate pool.
func TestBoatLeasesCapMachinesAcrossProcesses(t *testing.T) {
	if label := os.Getenv("FAIRSHARE_TEST_LEASE"); label != "" {
		os.Exit(cmdLease([]string{"boat", "--label", label}))
	}
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("BOAT_MAX_MACHINES", "2")
	t.Setenv("FAIRSHARE_HEAVY_SLOTS", "1")
	t.Setenv(heldEnv, "")
	fakeBoatList(t, "")

	a, b, c := startLease(t, "gate a"), startLease(t, "gate b"), startLease(t, "gate c")
	got := 0
	var waiting *leaseHelper
	for _, h := range []*leaseHelper{a, b, c} {
		if holds(h, 4*time.Second) {
			got++
		} else {
			waiting = h
		}
	}
	if got != 2 || waiting == nil {
		t.Fatalf("%d of 3 simultaneous leases were held; the cap is 2", got)
	}
	held := []*leaseHelper{}
	for _, h := range []*leaseHelper{a, b, c} {
		if h != waiting {
			held = append(held, h)
		}
	}
	if names := describeHolders(boatPool().holders()); !strings.Contains(names, "gate") {
		t.Error("the holders should be readable by name:", names)
	}

	// The boat pool never takes from the heavy-job slots.
	heavy, err := acquireSlot("go test ./...", "C:/repo", "", time.Second, nil)
	if err != nil {
		t.Fatal("two boat leases blocked a heavy-job slot:", err)
	}
	heavy.Close()

	// The caller finishing (stdin closed) frees its lease for the one waiting.
	held[0].stdin.Close()
	if !holds(waiting, 5*time.Second) {
		t.Fatal("a lease whose caller closed stdin was not passed on")
	}
	if err := held[0].cmd.Wait(); err != nil {
		t.Fatal("lease should exit 0 after stdin closes:", err)
	}

	// A killed owner never keeps its lease.
	d := startLease(t, "gate d")
	if holds(d, 2*time.Second) {
		t.Fatal("a third lease was held while two owners were alive")
	}
	_ = held[1].cmd.Process.Kill()
	if !holds(d, 5*time.Second) {
		t.Fatal("a killed owner kept its lease")
	}
}

// A machine boat lists that no lease names (its run was killed while boat went on, or it was started
// outside fairshare) takes a place in the cap until it ends, and is never deleted. A machine a holder
// named is not counted twice.
func TestBoatLeaseCountsMachinesNoLeaseNames(t *testing.T) {
	if label := os.Getenv("FAIRSHARE_TEST_LEASE"); label != "" {
		os.Exit(cmdLease([]string{"boat", "--label", label}))
	}
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("BOAT_MAX_MACHINES", "2")
	t.Setenv(heldEnv, "")
	setList := fakeBoatList(t, "bx_orphan1")

	a := startLeaseIn(t, "TestBoatLeaseCountsMachinesNoLeaseNames", "gate a")
	if !holds(a, 4*time.Second) {
		t.Fatal("one leftover machine and no holders: a lease should be held")
	}
	// a names its machine, which boat now lists beside the leftover one.
	if _, err := io.WriteString(a.stdin, "machine bx_mine0001\n"); err != nil {
		t.Fatal(err)
	}
	setList("bx_orphan1 bx_mine0001")
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(describeHoldersJSON(t), "bx_mine0001") {
		if time.Now().After(deadline) {
			t.Fatal("the holder's machine was not written into its lease")
		}
		time.Sleep(50 * time.Millisecond)
	}

	b := startLeaseIn(t, "TestBoatLeaseCountsMachinesNoLeaseNames", "gate b")
	if holds(b, 3*time.Second) {
		t.Fatal("a second lease was held while one holder and one leftover machine filled the cap of 2")
	}
	// The leftover machine ends by itself; a's named machine is not counted a second time.
	setList("bx_mine0001")
	if !holds(b, 20*time.Second) {
		t.Fatal("the lease stayed blocked after the leftover machine ended")
	}
	if b, _ := os.ReadFile(os.Getenv("FAIRSHARE_TEST_BOAT_LIST")); !strings.Contains(string(b), "bx_mine0001") {
		t.Fatal("the lease changed boat's machines")
	}
}

func describeHoldersJSON(t *testing.T) string {
	t.Helper()
	var all []string
	for _, h := range boatPool().holders() {
		all = append(all, h.Label+"="+h.Machine)
	}
	return strings.Join(all, " ")
}

// `fairshare lease boat -- <command>` runs it holding a lease, passes its output and exit code on,
// and names the lease's machine from the first id the command prints.
func TestBoatLeaseRunsACommand(t *testing.T) {
	if os.Getenv("FAIRSHARE_TEST_LEASE_CMD") != "" {
		// The command is this binary acting as `boat list`, so it prints an id as `boat run` does.
		os.Exit(cmdLease([]string{"boat", "--", os.Args[0], "list", "--json"}))
	}
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv(heldEnv, "")
	events := filepath.Join(t.TempDir(), "events.jsonl")
	t.Setenv("FAIRSHARE_EVENTS", events)
	fakeBoatList(t, "bx_ran0001")
	child := exec.Command(os.Args[0], "-test.run=^TestBoatLeaseRunsACommand$")
	child.Env = append(os.Environ(), "FAIRSHARE_TEST_LEASE_CMD=1")
	out, err := child.Output()
	if err != nil {
		t.Fatal("lease with a command:", err, string(out))
	}
	if !strings.Contains(string(out), `"bx_ran0001"`) {
		t.Fatalf("the command's output did not come through: %q", out)
	}
	logged, _ := os.ReadFile(events)
	if !strings.Contains(string(logged), `"machine":"bx_ran0001"`) {
		t.Fatalf("the lease did not name the machine the command printed: %s", logged)
	}
}

// Two runs waiting with one place free (the cap is 2 and one machine runs without a lease): exactly
// one starts, at once, and the other starts when the machine ends. Counting a fellow waiter as a
// machine left both waiting while the place stayed free, until every unleased machine had ended.
func TestBoatLeaseWaitersShareTheLastPlace(t *testing.T) {
	if label := os.Getenv("FAIRSHARE_TEST_LEASE"); label != "" {
		os.Exit(cmdLease([]string{"boat", "--label", label}))
	}
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("BOAT_MAX_MACHINES", "2")
	t.Setenv(heldEnv, "")
	setList := fakeBoatList(t, "bx_other01")

	a := startLeaseIn(t, "TestBoatLeaseWaitersShareTheLastPlace", "gate a")
	b := startLeaseIn(t, "TestBoatLeaseWaitersShareTheLastPlace", "gate b")
	held, waiting := 0, []*leaseHelper{}
	for _, h := range []*leaseHelper{a, b} {
		if holds(h, 6*time.Second) {
			held++
		} else {
			waiting = append(waiting, h)
		}
	}
	if held != 1 {
		t.Fatalf("%d of 2 waiters started with one place free; want exactly 1", held)
	}
	setList("")
	if !holds(waiting[0], 20*time.Second) {
		t.Fatal("the second waiter did not start after the unleased machine ended")
	}
}
