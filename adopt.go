package main

import (
	"cmp"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/Noisemaker111/fairshare/internal/windows"
)

// adopt makes every agent session's work fair without changing the agents: it watches for
// processes started under a chat (chat.go: each session T3 Code starts, whatever the provider, or
// an agent harness started from a terminal) and puts them in that chat's lane (lane.go), low-priority
// jobs for that one chat. Whatever a process starts afterwards is in the lane automatically. The
// chat's own process stays at normal priority, so the agent's own loop stays quick.
//
// `fairshare install` runs one adopt for the whole PC from a scheduled task, started at install
// and again within a minute of logon or of it ending; a second copy sees the mutex and exits.
//
// Lanes share the CPU by chat, not by thread: when one chat runs 48 busy threads and another 2,
// Windows alone gives the second about a third of what it asks for (fairshare bench lanes). So
// twice a second adopt measures each lane, and while a chat with unmet demand gets less than an
// equal share, every chat well over an equal share has its threads dropped to the lowest priority.
// Each lane writes one "lane" event when its chat ends: what it cost and what held it up.

func creationTime(pid uint32) uint64 {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return 0
	}
	defer windows.CloseHandle(h)
	var c, e, k, u windows.Filetime
	if windows.GetProcessTimes(h, &c, &e, &k, &u) != nil {
		return 0
	}
	return ftTicks(c)
}

func nameSet(csv string) map[string]bool {
	out := map[string]bool{}
	for _, n := range strings.Split(csv, ",") {
		if n = strings.ToLower(strings.TrimSpace(n)); n != "" {
			out[n] = true
		}
	}
	return out
}

// laneName names the job that holds one chat's work; the start time keeps a reused process ID
// from opening an old chat's lane.
func laneName(harness uint32, created uint64) string {
	return fmt.Sprintf(`Local\fairshare-lane-%d-%d`, harness, created)
}

const (
	adoptMutex     = `Local\fairshare-adopt`
	adoptStopEvent = `Local\fairshare-adopt-stop`
)

// adoptPolicy is what adopt applies to every chat's work: lower priority, and suspension only
// when freezing was asked for.
func adoptPolicy(freeze bool, ioPri, memPri int) Policy {
	p := DefaultPolicy()
	p.IOPriority, p.MemPriority = ioPri, memPri
	if freeze {
		p = p.withFreezing(freezeBelowMB, resumeAboveMB)
	}
	return p
}

func cmdAdopt(args []string) int {
	fs := flag.NewFlagSet("adopt", flag.ExitOnError)
	hosts := fs.String("host", defaultHosts, "name prefixes of programs whose every child is one chat, whatever its provider")
	harness := fs.String("harness", defaultHarness, "agent harness programs that are a chat when started outside a host (a terminal)")
	leave := fs.String("leave", "conhost.exe,openconsole.exe", "programs never adopted")
	every := fs.Duration("every", 250*time.Millisecond, "how often to look for new processes")
	stopAfter := fs.Duration("for", 0, "stop after this long (0 = until stopped)")
	ioPri := fs.Int("io", 0, "disk priority: 0 very low, 1 low, 2 normal")
	memPri := fs.Int("mempri", 1, "memory priority: 1 very low .. 5 normal")
	freeze := fs.Bool("freeze", false, "suspend chats' work while memory is short (off: work is only lowered in priority)")
	balance := fs.Bool("balance", false, "also hold back a chat that takes more than its share, by dropping its threads to idle priority")
	feedback := fs.Bool("feedback", false, "suspend the youngest work while the foreground is held up (not yet proven at scale)")
	verbose := fs.Bool("v", false, "print every process adopted")
	onlyPIDs := fs.String("pid", "", "comma-separated harness process IDs: watch only these sessions instead of every harness by name")
	fs.Parse(args)
	rule := newChatRule(*hosts, *harness)
	rule.only = map[uint32]bool{}
	for _, s := range strings.Split(*onlyPIDs, ",") {
		var pid uint32
		if _, err := fmt.Sscan(strings.TrimSpace(s), &pid); err == nil {
			rule.only[pid] = true
		}
	}
	// One adopt watches the whole PC; a second one exits. A --pid adopt watches only its sessions.
	var stopEvent windows.Handle
	if len(rule.only) == 0 {
		mutex, err := windows.CreateMutex(nil, false, windows.StringToUTF16Ptr(adoptMutex))
		if err == windows.ERROR_ALREADY_EXISTS {
			fmt.Fprintln(os.Stderr, "fairshare adopt: already running")
			return 0
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "fairshare adopt:", err)
			return 1
		}
		defer windows.CloseHandle(mutex)
		if stopEvent, err = windows.CreateEvent(nil, 1, 0, windows.StringToUTF16Ptr(adoptStopEvent)); err != nil {
			fmt.Fprintln(os.Stderr, "fairshare adopt:", err)
			return 1
		}
		defer windows.CloseHandle(stopEvent)
	}

	p := adoptPolicy(*freeze, *ioPri, *memPri)
	var sensor *WakeSensor
	if *feedback {
		sensor = NewWakeSensor()
	}
	gov, err := NewGovernor(p, sensor)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	// if this process is killed while it holds work frozen, a guard process thaws that work
	gov.FrozenFile = filepath.Join(filepath.Dir(runsDir()), fmt.Sprintf("adopt-%d.frozen", os.Getpid()))
	os.Remove(gov.FrozenFile)
	exe, _ := os.Executable()
	guard := exec.Command(exe, "thaw-guard", fmt.Sprint(os.Getpid()), gov.FrozenFile)
	guard.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP}
	if err := guard.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "fairshare adopt: no thaw guard, so do not kill this process while work is frozen:", err)
	}
	leaveNames := nameSet(*leave)
	lanes := map[uint32]*lane{}
	placed := map[uint32]bool{}
	self := windows.GetCurrentProcessId()
	placements := map[string]int{}
	var nLanes, nHolds, ticks int
	start := time.Now()
	lastBalance, lastStatus := start, time.Time{} // status on the first look, then every 5 s
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	if len(rule.only) > 0 {
		fmt.Fprintf(os.Stderr, "fairshare adopt: watching sessions %s\n", *onlyPIDs)
	} else {
		fmt.Fprintf(os.Stderr, "fairshare adopt: watching every child of %s, and %s started elsewhere\n", *hosts, *harness)
	}
	closeLane := func(h uint32, why string, procs map[uint32]procInfo) {
		l := lanes[h]
		nHolds += l.holds
		l.close(gov, why, procs)
		delete(lanes, h)
		if *verbose {
			fmt.Fprintf(os.Stderr, "  %6.1fs lane %d %s closed: %s\n", time.Since(start).Seconds(), h, l.name, why)
		}
	}
	laneList := func() []*lane {
		ls := make([]*lane, 0, len(lanes))
		for _, l := range lanes {
			ls = append(ls, l)
		}
		return ls
	}

	for *stopAfter == 0 || time.Since(start) < *stopAfter {
		select {
		case <-stop:
			*stopAfter = time.Nanosecond
			continue
		default:
		}
		if stopEvent != 0 {
			if ev, _ := windows.WaitForSingleObject(stopEvent, 0); ev == windows.WAIT_OBJECT_0 {
				break // `fairshare install` is replacing this copy
			}
		}
		ticks++
		// one snapshot per look; twice a second it also measures every lane
		var procs map[uint32]procInfo
		var bal *laneBalance
		if time.Since(lastBalance) >= 500*time.Millisecond && len(lanes) > 0 {
			bal = startBalance(laneList())
			procs = bal.procs
		} else {
			procs = snapshot()
		}
		for pid := range placed {
			if _, ok := procs[pid]; !ok {
				delete(placed, pid)
			}
		}
		for h, l := range lanes {
			if p, ok := procs[h]; !ok || p.created != l.created {
				closeLane(h, "chat ended", procs) // and everything left in its lane ends
				continue
			}
			for pid := range l.outside {
				if _, ok := procs[pid]; !ok {
					delete(l.outside, pid)
				}
			}
		}
		// new processes oldest first, so a parent is placed before its children
		var fresh []uint32
		for pid, info := range procs {
			if !placed[pid] && !leaveNames[info.name] && pid != 0 && pid != self && !info.unstarted {
				fresh = append(fresh, pid)
			}
		}
		slices.SortFunc(fresh, func(a, b uint32) int { return cmp.Compare(procs[a].created, procs[b].created) })
		for _, pid := range fresh {
			info := procs[pid]
			chat := rule.of(pid, inSnapshot(procs))
			if chat == 0 {
				continue
			}
			l := lanes[chat]
			if l == nil {
				created := procs[chat].created
				u, err := gov.NewUnit(false, laneName(chat, created))
				if err != nil {
					fmt.Fprintln(os.Stderr, "fairshare adopt:", err)
					continue
				}
				l = newLane(gov, chat, created, procs[chat].name, u)
				lanes[chat] = l
				nLanes++
			}
			placed[pid] = true
			h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_SET_INFORMATION|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
			if err != nil {
				continue // gone already, or not ours
			}
			how := "already in it"
			if !l.jobs.has(h) {
				how = l.place(gov, pid, h)
			}
			windows.CloseHandle(h)
			placements[strings.SplitN(how, ":", 2)[0]]++
			if *verbose && how != "already in it" {
				fmt.Fprintf(os.Stderr, "  %6.1fs lane %d %s %d %s\n", time.Since(start).Seconds(), chat, how, pid, info.name)
			}
		}
		if bal != nil {
			if bal.finish(time.Since(lastBalance).Seconds(), *balance, readCaps()) {
				lastStatus = time.Time{} // show a new throttle at once
			}
			lastBalance = time.Now()
		}
		if time.Since(lastStatus) >= 5*time.Second {
			writeStatus(laneList())
			lastStatus = time.Now()
		}
		time.Sleep(*every)
	}
	for h := range lanes {
		closeLane(h, "adopt stopped", nil) // adopted work survives adopt stopping
	}
	writeStatus(nil)
	var c, e, k, u windows.Filetime
	windows.GetProcessTimes(windows.CurrentProcess(), &c, &e, &k, &u)
	selfCPU := float64(ftTicks(k)+ftTicks(u)) / 1e7
	logEvent("adopt", map[string]any{"wall_ms": time.Since(start).Milliseconds(), "self_cpu_ms": int(selfCPU * 1000), "ticks": ticks,
		"lanes": nLanes, "joined": placements["joined"], "side_jobs": placements["side job"], "inherited": placements["already in it"],
		"outside": placements["outside"], "holds": nHolds, "tuned": gov.Tuned.Load(), "freezes": gov.Freezes.Load()})
	fmt.Fprintf(os.Stderr, "fairshare adopt: %d chats; processes: %d joined their chat's lane, %d needed a side job, %d were already in one, %d refused every job (tuned directly); %d times a chat was held back, %d freezes; adopt itself used %.1f s CPU\n",
		nLanes, placements["joined"], placements["side job"], placements["already in it"], placements["outside"], nHolds, gov.Freezes.Load(), selfCPU)
	return 0
}
