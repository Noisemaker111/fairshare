package main

// Boat machines: at most BOAT_MAX_MACHINES (2) boat.dev machines run at once from this PC, across
// every repo and process (Jon, 2026-11-07). Boat itself allows 100 on the paid plan, so the cap is
// kept here. Reading `boat list` and then starting is not enough: a machine is not in the list for
// the seconds it takes to create, so two starts in that window both get in. On 2026-11-07 at 18:57
// three ran at once: two appforgutters landing gates and one jgengine verify-fast.
//
// Whatever starts a machine first holds a lease from `fairshare lease boat` for that machine's whole
// life. A lease is a slot in its own pool (slot.go): a lock file Windows closes when the holder
// exits, however it exits, so a crashed owner never keeps one, and waiters are let in in the order
// they came. A holder names its machine once boat reports it ("machine <id>" on stdin, or read from
// the command's output), and that name is written into its lease file.
//
// A machine can outlive its lease: the run that held it was killed while `boat run` went on, or a
// machine was started without a lease. So the lease is handed over only once the other holders plus
// the listed machines no lease names are fewer than the cap. Such machines are counted, never
// deleted: they may be someone's work.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

func boatMachines() int {
	if n, err := strconv.Atoi(os.Getenv("BOAT_MAX_MACHINES")); err == nil && n > 0 {
		return n
	}
	return 2
}

func boatPool() slotPool {
	return slotPool{name: "boat", dir: filepath.Join(slotsDir(), "boat"), count: boatMachines()}
}

var machineID = regexp.MustCompile(`\bbx_[a-z0-9]+\b`)

// boatExe is the boat CLI: BOAT_EXE, else on PATH, else where its installer puts it.
func boatExe() string {
	if p := os.Getenv("BOAT_EXE"); p != "" {
		return p
	}
	if p, err := exec.LookPath("boat"); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ascii", "bin", "boat.exe")
}

// listedMachines is the ids `boat list` shows now.
func listedMachines() ([]string, error) {
	cmd := exec.Command(boatExe(), "list", "--json")
	cmd.Stdin = nil
	done := make(chan struct{})
	var out []byte
	var err error
	go func() { out, err = cmd.Output(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-done
		return nil, fmt.Errorf("boat list did not answer in 60 s")
	}
	if err != nil {
		return nil, fmt.Errorf("boat list failed: %v %s", err, strings.TrimSpace(clip(string(out), 200)))
	}
	var parsed struct {
		Sandboxes []struct {
			ID string `json:"id"`
		} `json:"sandboxes"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, fmt.Errorf("boat list printed no JSON: %s", clip(string(out), 200))
	}
	ids := make([]string, 0, len(parsed.Sandboxes))
	for _, s := range parsed.Sandboxes {
		ids = append(ids, s.ID)
	}
	return ids, nil
}

// boatRoom says whether one more machine fits: the other lease holders (each owns at most one
// machine, created or on its way) plus listed machines no lease names must stay under the cap.
func boatRoom(self int, holders []slotHolder, listed []string, cap int) (ok bool, unowned []string) {
	named := map[string]bool{}
	others := 0
	for _, h := range holders {
		if h.PID == self {
			continue
		}
		others++
		if h.Machine != "" {
			named[h.Machine] = true
		}
	}
	for _, id := range listed {
		if !named[id] {
			unowned = append(unowned, id)
		}
	}
	return others+len(unowned) < cap, unowned
}

// waitForBoatRoom keeps the lease until a machine fits. Another holder waiting here at the same
// time counts this one, so neither starts while the count is full.
func waitForBoatRoom(pool slotPool) {
	told := ""
	for {
		listed, err := listedMachines()
		if err != nil {
			// The holders alone still bound this PC's own starts; the caller's own boat calls will
			// meet the same problem and say so.
			fmt.Fprintf(os.Stderr, "[fairshare] could not count boat's machines (%v); going by the leases alone.\n", err)
			return
		}
		ok, unowned := boatRoom(os.Getpid(), pool.holders(), listed, pool.count)
		if ok {
			return
		}
		why := strings.Join(unowned, ", ")
		if why != told {
			fmt.Fprintf(os.Stderr, "[fairshare] waiting for a boat machine: %d already run without a lease here (%s), left by a run that was stopped or started outside fairshare; it waits for them to end, it never deletes them.\n", len(unowned), why)
			told = why
		}
		time.Sleep(15 * time.Second)
	}
}

// boatLease is a held boat lease that can record its machine.
type boatLease struct {
	mu     sync.Mutex
	slot   *heavySlot
	holder slotHolder
}

func (l *boatLease) name(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.holder.Machine == id || l.slot == nil || l.slot.f == nil {
		return
	}
	l.holder.Machine = id
	b, _ := json.Marshal(l.holder)
	_ = l.slot.f.Truncate(0)
	_, _ = l.slot.f.WriteAt(b, 0)
}

// machineNamer passes output through and names the lease's machine from the first id in it.
type machineNamer struct {
	lease *boatLease
	w     io.Writer
	seen  bool
	tail  string
}

func (m *machineNamer) Write(p []byte) (int, error) {
	if !m.seen {
		m.tail = clip(m.tail+string(p), 4096)
		if id := machineID.FindString(m.tail); id != "" {
			m.seen = true
			m.lease.name(id)
		}
	}
	return m.w.Write(p)
}

// `fairshare lease boat [--label <what>] [-- <command> [args...]]` waits its turn for one of the
// boat machine leases and for room among the machines boat lists.
//
// With no command it prints "held" on its own line to stdout and keeps the lease until stdin closes
// (the caller is done, or died and Windows closed the pipe) or the process that started it ends. A
// line "machine <id>" on stdin names the caller's machine. The caller starts its machine only after
// reading "held", and closes stdin once the machine is deleted.
//
// With a command it runs it holding the lease, with its output and exit code untouched, and names
// the machine from the first id in its output: `fairshare lease boat -- boat run ...`.
func cmdLease(args []string) int {
	if len(args) == 0 || args[0] != "boat" {
		fmt.Fprintln(os.Stderr, "usage: fairshare lease boat [--label <what>] [-- <command> [args...]]")
		return 2
	}
	label := ""
	var command []string
	for i := 1; i < len(args); i++ {
		switch {
		case args[i] == "--label" && i+1 < len(args):
			label = args[i+1]
			i++
		case args[i] == "--":
			command = args[i+1:]
			i = len(args)
		default:
			fmt.Fprintf(os.Stderr, "fairshare lease: unknown argument %q\n", args[i])
			return 2
		}
	}
	if label == "" && len(command) > 0 {
		label = clip(strings.Join(command, " "), 120)
	}
	if command != nil && len(command) == 0 {
		fmt.Fprintln(os.Stderr, "fairshare lease: nothing after --")
		return 2
	}
	if len(command) > 0 {
		signal.Ignore(os.Interrupt) // the command gets Ctrl+C itself; we wait for it and pass on its code
	}
	endWithCaller("lease")
	cwd, _ := os.Getwd()
	what := label
	if what == "" {
		what = "a boat machine"
	}
	pool := boatPool()
	slot, err := pool.acquire(what, cwd, label, 0, func(holders string) {
		fmt.Fprintf(os.Stderr, "[fairshare] waiting its turn for a boat machine: this PC runs at most %d at once (BOAT_MAX_MACHINES). Current holders: %s.\n", pool.count, holders)
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "[fairshare] cannot use the boat lease queue: %v\n", err)
		logEvent("lease", map[string]any{"pool": "boat", "label": label, "cwd": cwd, "error": err.Error()})
		return 1
	}
	defer slot.Close()
	waitForBoatRoom(pool)
	lease := &boatLease{slot: slot, holder: slot.holder}
	waited := slot.Waited
	logEvent("lease", map[string]any{"pool": "boat", "label": label, "cwd": cwd, "waited_ms": waited.Milliseconds()})
	held := time.Now()
	code := 0
	if len(command) > 0 {
		cmd := exec.Command(command[0], command[1:]...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = &machineNamer{lease: lease, w: os.Stdout}
		cmd.Stderr = &machineNamer{lease: lease, w: os.Stderr}
		if err := cmd.Run(); err != nil {
			code = 127
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			} else {
				fmt.Fprintln(os.Stderr, "fairshare lease:", err)
			}
		}
	} else {
		if _, err := os.Stdout.WriteString("held\n"); err != nil {
			return 1 // nobody is reading: the caller is gone
		}
		lines := bufio.NewScanner(os.Stdin)
		for lines.Scan() { // ends at EOF: the caller closed stdin or exited
			if id, ok := strings.CutPrefix(strings.TrimSpace(lines.Text()), "machine "); ok && machineID.MatchString(id) {
				lease.name(machineID.FindString(id))
			}
		}
	}
	logEvent("lease-end", map[string]any{"pool": "boat", "label": label, "cwd": cwd, "machine": lease.holder.Machine, "held_ms": time.Since(held).Milliseconds(), "exit": code})
	return code
}
