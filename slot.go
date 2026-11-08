package main

// Heavy-job slots: at most a few whole test suites, builds or checks run on this PC at once,
// across every chat and repo. Without them, six chats each start a full suite, the CPU sits at
// 100%, and tests fail on their own time limits instead of on bugs (2026-11-03: every session was
// cut off at 02:01 that way, after an evening of agents rerunning "flaky" tests). Idle priority
// alone does not help: heavy jobs at idle priority still share the CPU with each other.
//
// Only a command on the heavy list (heavy.go) waits for a slot; any other command runs at once.
// Nothing is judged from a command's name: every run `exec` and `slot` wrap is measured, and the
// list learns a command the first time one run of it is heavy.
//
// A slot is a lock file held open for writing. Windows closes it when the holder exits, however
// it exits, so a crashed or killed run never keeps a slot. The file holds who has it, so a run that
// waits can say what it waits for.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Noisemaker111/fairshare/internal/windows"
)

// heldEnv marks work that already holds a slot, so what it starts never waits for a second one.
const heldEnv = "FAIRSHARE_HEAVY_HELD"

var (
	discoveryRun = regexp.MustCompile(`^(?:bun|npm|pnpm)\s+run\s+(?:verify(?::smoke)?\s+--plan|smoke:ci\s+--list)(?:\s|$)`)
	leadAssign   = regexp.MustCompile(`^\w+=`)
)

func slotsDir() string { return filepath.Join(eventsDir(), "slots") }

// fixedSlots is FAIRSHARE_HEAVY_SLOTS: a fixed count of heavy-job slots in place of the room rule
// (room.go), or 0 when unset.
func fixedSlots() int {
	if n, err := strconv.Atoi(os.Getenv("FAIRSHARE_HEAVY_SLOTS")); err == nil && n > 0 {
		return n
	}
	return 0
}

type slotHolder struct {
	PID   int    `json:"pid"`
	Cmd   string `json:"cmd"`
	Cwd   string `json:"cwd"`
	Label string `json:"label,omitempty"`
	Since string `json:"since"`
	// Machine is the boat machine a boat lease holder started, once boat reported it.
	Machine string `json:"machine,omitempty"`
	// Started is set once a boat lease holder was let through to start its machine.
	Started bool `json:"started,omitempty"`
}

// heavySlot is a held slot; release it with Close (exiting does the same).
type heavySlot struct {
	f      *os.File
	Waited time.Duration
	holder slotHolder
	Beside int    // heavy jobs that held slots when this one got its own (counted under the room rule)
	Why    string // the room rule's last reason to wait, if it waited for room
}

func (s *heavySlot) Close() {
	if s != nil && s.f != nil {
		s.f.Close()
		s.f = nil
	}
}

var errBusy = errors.New("no heavy-job slot came free")

// slotPool is one set of slots with its own queue: the heavy-job slots, or the boat machines
// (boat_lease.go). Each pool has its own folder, so its queue and its lock files never mix.
type slotPool struct {
	name  string // lock files are <name>-<i>.lock
	dir   string
	count int // the most holders at once
	// room, when set, is asked before a free lock file is taken: whether one more holder fits
	// beside held, and if not, why. It is called about once a second while the waiter is first.
	room func(held []slotHolder) (bool, string)
}

func heavyPool() slotPool {
	if n := fixedSlots(); n > 0 {
		return slotPool{name: "heavy", dir: slotsDir(), count: n}
	}
	return slotPool{name: "heavy", dir: slotsDir(), count: maxHeavySlots}
}

// try takes slot i if nobody holds it. Others may still read it to see who does.
func (p slotPool) try(i int) *os.File {
	path := filepath.Join(p.dir, fmt.Sprintf("%s-%d.lock", p.name, i))
	ptr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil
	}
	h, err := windows.CreateFile(ptr, windows.GENERIC_WRITE, windows.FILE_SHARE_READ, nil, windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil
	}
	return os.NewFile(uintptr(h), path)
}

// holders reads who holds each slot that is taken now.
func (p slotPool) holders() []slotHolder {
	var out []slotHolder
	for i := range p.count {
		path := filepath.Join(p.dir, fmt.Sprintf("%s-%d.lock", p.name, i))
		if f := p.try(i); f != nil { // free: it was only left over
			f.Close()
			continue
		}
		ptr, err := windows.UTF16PtrFromString(path)
		if err != nil {
			continue
		}
		h, err := windows.CreateFile(ptr, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
		if err != nil {
			continue
		}
		f := os.NewFile(uintptr(h), path)
		var who slotHolder
		buf := make([]byte, 4096)
		n, _ := f.Read(buf)
		f.Close()
		if json.Unmarshal(buf[:n], &who) == nil {
			out = append(out, who)
		}
	}
	return out
}

func slotHolders() []slotHolder { return heavyPool().holders() }

func describeHolders(hs []slotHolder) string {
	var parts []string
	for _, h := range hs {
		age := ""
		if t, err := time.Parse(time.RFC3339, h.Since); err == nil {
			age = ", " + time.Since(t).Round(time.Second).String()
		}
		parts = append(parts, fmt.Sprintf("`%s` in %s%s", clip(h.Cmd, 60), filepath.Base(h.Cwd), age))
	}
	if len(parts) == 0 {
		return "none readable"
	}
	return strings.Join(parts, "; ")
}

// acquireSlot waits for a heavy-job slot, up to maxWait (0: no limit): one of the first two, or
// past them one the PC has room for (room.go), judged by need, what the command used on its last
// run. note is told who holds the slots and why it waits, once, when it has to wait. Work that
// already holds a slot gets an empty one at once. Callers ask only for a command on the heavy list.
func acquireSlot(cmd, cwd, label string, need heavyNeed, maxWait time.Duration, note func(string)) (*heavySlot, error) {
	if os.Getenv(heldEnv) != "" {
		return &heavySlot{}, nil
	}
	p := heavyPool()
	if fixedSlots() == 0 {
		var w loadWatch
		p.room = func(held []slotHolder) (bool, string) {
			if len(held) < minHeavySlots {
				return true, ""
			}
			load, ready := w.look()
			if !ready {
				return false, measuringLoad
			}
			return heavyRoom(len(held), newestHolder(held), load, need)
		}
	}
	return p.acquire(cmd, cwd, label, maxWait, note)
}

// measuringLoad is the reason a waiter gives before it has looked at the PC's load long enough.
const measuringLoad = "measuring the PC's load"

// newestHolder is how long ago the most recent of held got its slot.
func newestHolder(held []slotHolder) time.Duration {
	newest := time.Duration(1<<63 - 1)
	for _, h := range held {
		if t, err := time.Parse(time.RFC3339, h.Since); err == nil {
			newest = min(newest, time.Since(t))
		}
	}
	return newest
}

// acquire waits its turn in the pool's queue for a free slot, up to maxWait (0: no limit).
func (p slotPool) acquire(cmd, cwd, label string, maxWait time.Duration, note func(string)) (*heavySlot, error) {
	if err := os.MkdirAll(p.dir, 0o755); err != nil {
		return nil, err
	}
	start := time.Now()
	ticket, err := newSlotTicket(p.dir, start, maxWait)
	if err != nil {
		return nil, err
	}
	defer func() {
		if ticket.file != nil {
			if gate, err := lockSlotQueue(p.dir, time.Now(), 0); err == nil {
				_ = ticket.remove()
				gate.Close()
			} else {
				_ = ticket.file.Close()
				ticket.file = nil
			}
		}
	}()
	told := false
	why, waitedFor := "", ""
	for {
		gate, err := lockSlotQueue(p.dir, start, maxWait)
		if err != nil {
			return nil, err
		}
		live, err := liveSlotTickets(p.dir)
		if err != nil {
			gate.Close()
			return nil, err
		}
		first := len(live) > 0 && live[0] == ticket.name
		var held []slotHolder
		fits := true
		if first && p.room != nil {
			held = p.holders()
			fits, why = p.room(held)
			if !fits && why != measuringLoad {
				waitedFor = why
			}
		}
		if first && fits {
			for i := range p.count {
				f := p.try(i)
				if f == nil {
					continue
				}
				f.Truncate(0)
				holder := slotHolder{PID: os.Getpid(), Cmd: cmd, Cwd: cwd, Label: label, Since: time.Now().Format(time.RFC3339)}
				b, _ := json.Marshal(holder)
				f.WriteAt(b, 0)
				if err := ticket.remove(); err != nil {
					f.Close()
					gate.Close()
					return nil, err
				}
				gate.Close()
				return &heavySlot{f: f, Waited: time.Since(start), holder: holder, Beside: len(held), Why: waitedFor}, nil
			}
		}
		gate.Close()
		if !told && note != nil && why != measuringLoad {
			text := describeHolders(p.holders())
			if why != "" {
				text += ". It waits because " + why
			}
			note(text)
			told = true
		}
		if maxWait > 0 && time.Since(start) >= maxWait {
			if waitedFor != "" {
				return nil, fmt.Errorf("%w: %s", errBusy, waitedFor)
			}
			return nil, errBusy
		}
		time.Sleep(time.Second)
	}
}

// slotEvent adds to a run's event how its slot was granted under the room rule.
func slotEvent(ev map[string]any, s *heavySlot) {
	if s.Beside > 0 {
		ev["slots_beside"] = s.Beside
	}
	if s.Why != "" {
		ev["waited_for"] = clip(s.Why, 200)
	}
}

// waitNote is what a run on the heavy list tells the agent while it waits for a slot.
func waitNote(e heavyEntry, cmd, holders string) string {
	return fmt.Sprintf("[fairshare] waiting its turn for a heavy-job slot: %s. Current holders: %s.%s", heavyNote(e), holders, boatHint(cmd))
}

// `fairshare slot [--wait <ms>] <command> [args...]` runs a command with the agent's own output,
// redirects and exit code untouched, and measures it for the heavy list. A command on the list
// first waits for a heavy-job slot; any other starts at once. The hook puts it in front of runs in
// the background, runs that write to a file, piped test or build runs, and listed commands.
func cmdSlot(args []string) int {
	var maxWait time.Duration // --wait <ms>: give up then; none waits as long as it takes
	if len(args) >= 2 && args[0] == "--wait" {
		ms, err := strconv.Atoi(args[1])
		if err != nil || ms < 0 {
			fmt.Fprintln(os.Stderr, "fairshare slot: --wait needs milliseconds")
			return 2
		}
		maxWait, args = time.Duration(ms)*time.Millisecond, args[2:]
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: fairshare slot [--wait <ms>] <command> [args...]")
		return 2
	}
	signal.Ignore(os.Interrupt) // the command gets Ctrl+C itself; we wait for it and pass on its code
	endWithCaller("slot")
	// Everything the command starts is in a job Windows kills when this process ends. The job
	// contains this process, so its handle is left for the OS to close at exit: closing it here
	// would kill this process before it could return the child's status. It also counts the
	// command's CPU for the heavy list.
	job, jobErr := killOnExitJob()
	text := strings.Join(args, " ")
	cwd, _ := os.Getwd()
	s := &heavySlot{}
	entry, listed := heavyListed(cwd, text)
	if listed {
		var err error
		s, err = acquireSlot(text, cwd, "", needOf(entry), maxWait, func(holders string) { fmt.Fprintln(os.Stderr, waitNote(entry, text, holders)) })
		if err != nil {
			if !errors.Is(err, errBusy) {
				fmt.Fprintf(os.Stderr, "[fairshare] not started: cannot use the heavy-job queue: %v\n", err)
				logEvent("slot", map[string]any{"cmd": clip(text, 300), "cwd": cwd, "stopped": "queue-error"})
				return 1
			}
			fmt.Fprintf(os.Stderr, "[fairshare] not started: %v, in %s. Run it with run_in_background (it then waits as long as it takes), or narrow it to the files you changed.%s\n", err, maxWait.Round(time.Second), boatHint(text))
			logEvent("slot", map[string]any{"cmd": clip(text, 300), "cwd": cwd, "stopped": "no-slot", "waited_ms": maxWait.Milliseconds()})
			return 124
		}
		defer s.Close()
		if s.Waited > time.Second {
			fmt.Fprintf(os.Stderr, "[fairshare] got a slot after %s\n", s.Waited.Round(time.Second))
		}
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if listed {
		cmd.Env = append(os.Environ(), heldEnv+"=1")
	}
	started := time.Now()
	cpu0 := selfCPU()
	code := 0
	if err := cmd.Run(); err != nil {
		code = 127
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			fmt.Fprintln(os.Stderr, "fairshare slot:", err)
		}
	}
	wall := time.Since(started)
	ev := map[string]any{"cmd": clip(text, 300), "cwd": cwd, "exit": code, "wall_ms": wall.Milliseconds(), "listed": listed}
	if listed {
		ev["waited_ms"] = s.Waited.Milliseconds()
		slotEvent(ev, s)
	}
	if jobErr == nil {
		st := jobStats(job)
		cpu := max(0, st.CPUSeconds-(selfCPU()-cpu0))
		ev["cpu_ms"], ev["peak_mb"] = int64(cpu*1000), round1(st.PeakMB)
		if recordHeavy(heavyRun{Dir: cwd, Cmd: text, CPUSeconds: cpu, WallSeconds: wall.Seconds(), PeakMB: st.PeakMB, At: started}) {
			ev["listed_now"] = true
		}
	} else {
		ev["job_error"] = jobErr.Error()
	}
	logEvent("slot", ev)
	return code
}
