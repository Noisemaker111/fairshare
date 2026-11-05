package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Noisemaker111/fairshare/internal/windows"
)

// endWithCaller ends this process when any process that started it ends, and with this process
// every job it holds, so Windows kills the command and everything it started.
//
// An agent harness cancels a background command by killing the process tree under its shell, by
// Windows parent links. Git Bash breaks those links: to run `bash script.sh` it forks a subshell
// that execs the script as a new Windows process and exits, so the script, and fairshare under
// it, have a parent that no longer exists. The tree kill never reached them and fairshare's job
// kept the command running (a headed browser stayed open on the user's desktop after its task was
// stopped). Git Bash's own process table still has the link, so where the Windows chain breaks
// at a Git Bash process, fairshare reads that table once to find the caller.
//
// The callers are found once, at start, and waited on as events, with no polling. The walk stops
// below explorer.exe, which outlives every chat.
func endWithCaller(verb string) {
	callers := findCallers()
	if len(callers) == 0 {
		return
	}
	handles := make([]windows.Handle, len(callers))
	for i, c := range callers {
		handles[i] = c.h
	}
	go func() {
		ev, err := windows.WaitForMultipleObjects(handles, false, windows.INFINITE)
		if err != nil {
			return
		}
		i := int(ev - windows.WAIT_OBJECT_0)
		if i < 0 || i >= len(callers) {
			return
		}
		fmt.Fprintf(os.Stderr, "[fairshare] %s ended, so this %s and everything it started were ended\n", callers[i].name, verb)
		logEvent("caller-ended", map[string]any{"verb": verb, "caller": callers[i].name})
		os.Exit(130) // closing this process's job handles kills what they hold
	}()
}

type caller struct {
	h    windows.Handle
	name string
}

// findCallers walks this process's ancestors with liveProc (one process at a time, not a whole-PC
// list) and opens each for waiting.
func findCallers() []caller {
	var out []caller
	seen := map[uint32]bool{}
	chain := []uint32{windows.GetCurrentProcessId()} // this process and the ancestors found
	usedMsys := false
	for len(out) < windows.MAXIMUM_WAIT_OBJECTS-1 {
		next, ok := livingParent(chain[len(chain)-1])
		if !ok && !usedMsys {
			usedMsys = true // one read of Git Bash's table covers every break above
			next, ok = msysParent(chain)
		}
		if !ok || seen[next] {
			break
		}
		p, ok := liveProc(next)
		if !ok || p.name == "explorer.exe" {
			break
		}
		seen[next] = true
		if h, ok := openCaller(next, p.created); ok {
			out = append(out, caller{h, fmt.Sprintf("%s (%d)", p.name, next)})
		}
		chain = append(chain, next)
	}
	return out
}

// livingParent is pid's Windows parent while that parent is still the process that started it.
func livingParent(pid uint32) (uint32, bool) {
	c, ok := liveProc(pid)
	if !ok || c.ppid == pid || c.ppid == 0 {
		return 0, false
	}
	if par, ok := liveProc(c.ppid); !ok || par.created > c.created {
		return 0, false // the parent is gone, or its ID now names a younger process
	}
	return c.ppid, true
}

// openCaller opens pid for waiting if it is still the process liveProc saw.
func openCaller(pid uint32, created uint64) (windows.Handle, bool) {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return 0, false
	}
	var c, e, k, u windows.Filetime
	if windows.GetProcessTimes(h, &c, &e, &k, &u) != nil || ftTicks(c) != created {
		windows.CloseHandle(h)
		return 0, false
	}
	return h, true
}

// msysParent finds the caller of a chain whose Windows links broke at a Git Bash process: Git
// Bash's ps maps each of its process IDs to a Windows one, and an exec keeps the ID, so the
// ancestor nearest the break that ps lists has the real caller as its parent there.
func msysParent(chain []uint32) (uint32, bool) {
	ps := ""
	for _, pid := range chain {
		if p := msysTool(pid, "ps.exe"); p != "" {
			ps = p
			break
		}
	}
	if ps == "" {
		return 0, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ps, "-a")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	raw, err := cmd.Output()
	if err != nil {
		return 0, false
	}
	parentOf, winOf := parseMsysPS(raw)
	for i := len(chain) - 1; i >= 0; i-- {
		ppid, ok := parentOf[chain[i]]
		if !ok {
			continue
		}
		win, ok := winOf[ppid]
		if !ok {
			return 0, false
		}
		p, ok1 := liveProc(win)
		c, ok2 := liveProc(chain[i])
		if ok1 && ok2 && p.created <= c.created {
			return win, true
		}
		return 0, false
	}
	return 0, false
}

// parseMsysPS reads `ps -a` from Git Bash: for each Windows PID, the Git Bash ID of its parent,
// and for each Git Bash ID, its Windows PID.
func parseMsysPS(raw []byte) (parentOf map[uint32]uint64, winOf map[uint64]uint32) {
	parentOf, winOf = map[uint32]uint64{}, map[uint64]uint32{}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		f := strings.Fields(strings.TrimLeft(sc.Text(), " SIO")) // a status letter can lead the row
		if len(f) < 4 {
			continue
		}
		pid, err1 := strconv.ParseUint(f[0], 10, 64)
		ppid, err2 := strconv.ParseUint(f[1], 10, 64)
		win, err3 := strconv.ParseUint(f[3], 10, 32)
		if err1 != nil || err2 != nil || err3 != nil {
			continue // the header
		}
		parentOf[uint32(win)] = ppid
		winOf[pid] = uint32(win)
	}
	return parentOf, winOf
}

// msysTool is the path of tool beside pid's program when that program is Git Bash's (its folder
// holds msys-2.0.dll), or "".
func msysTool(pid uint32, tool string) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, 32768)
	n := uint32(len(buf))
	if windows.QueryFullProcessImageName(h, 0, &buf[0], &n) != nil {
		return ""
	}
	dir := filepath.Dir(windows.UTF16ToString(buf[:n]))
	if _, err := os.Stat(filepath.Join(dir, "msys-2.0.dll")); err != nil {
		return ""
	}
	p := filepath.Join(dir, tool)
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}
