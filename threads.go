package main

// What the threads of a set of processes are doing right now, from one system-wide snapshot
// (NtQuerySystemInformation). It tells a command that waits on something (every thread blocked)
// from one that only waits its turn for the CPU (threads ready) or that fairshare froze. The same
// snapshot lists every thread's base priority, which demoteThreads uses to hold back a chat that
// takes more than its share.

import (
	"encoding/binary"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	ntQuerySystemInformation = ntdll.NewProc("NtQuerySystemInformation")
	setThreadPriority        = kernel32.NewProc("SetThreadPriority")
)

const (
	systemProcessInformation = 5
	procEntryHeader          = 0x100 // SYSTEM_PROCESS_INFORMATION on 64-bit Windows
	threadEntrySize          = 0x50  // SYSTEM_THREAD_INFORMATION
	statusInfoLengthMismatch = 0xC0000004
	threadSetLimitedInfo     = 0x0400
	threadPriorityIdle       = -15 // priority 1 in any class below real-time
	threadPriorityNormal     = 0
)

// ThreadStates counts threads by what they are doing.
type ThreadStates struct {
	Running, Ready, Waiting, Suspended int
}

type threadInfo struct {
	pid, tid, base, state, reason uint32
}

// procEntry is one process in the snapshot: CPU is its user+kernel seconds so far.
type procEntry struct {
	pid, ppid uint32
	created   uint64
	cpu       float64
	name      string
}

// procInfo is what fairshare keeps about each process between looks.
type procInfo struct {
	ppid    uint32
	name    string
	created uint64 // start time; with the ID it names one process even after the ID is reused
}

var snapBuf = make([]byte, 1<<20)

// snapshot lists every process on the PC (half the cost of a Toolhelp snapshot, which makes this
// same query and then copies it).
func snapshot() map[uint32]procInfo {
	out := map[uint32]procInfo{}
	scan(func(e procEntry) bool {
		out[e.pid] = procInfo{ppid: e.ppid, name: e.name, created: e.created}
		return false
	}, nil)
	return out
}

// eachThread calls f for every thread of the processes wanted says yes to.
func eachThread(wanted func(pid uint32) bool, f func(threadInfo)) bool {
	return scan(func(e procEntry) bool { return wanted(e.pid) }, f)
}

// scan reads one snapshot of every process and thread on the PC. It calls onProc for every
// process, and f for each thread of the processes onProc returns true for.
func scan(onProc func(procEntry) bool, f func(threadInfo)) bool {
	for {
		var need uint32
		st, _, _ := ntQuerySystemInformation.Call(systemProcessInformation, uintptr(unsafe.Pointer(&snapBuf[0])), uintptr(len(snapBuf)), uintptr(unsafe.Pointer(&need)))
		if st == statusInfoLengthMismatch {
			snapBuf = make([]byte, max(int(need)+1<<16, 2*len(snapBuf)))
			continue
		}
		if st != 0 {
			return false
		}
		break
	}
	b := snapBuf
	for off := 0; off+procEntryHeader <= len(b); {
		next := binary.LittleEndian.Uint32(b[off:])
		threads := int(binary.LittleEndian.Uint32(b[off+4:]))
		pid := uint32(binary.LittleEndian.Uint64(b[off+80:]))
		e := procEntry{
			pid:     pid,
			ppid:    uint32(binary.LittleEndian.Uint64(b[off+88:])),
			created: binary.LittleEndian.Uint64(b[off+32:]),
			cpu:     float64(binary.LittleEndian.Uint64(b[off+40:])+binary.LittleEndian.Uint64(b[off+48:])) / 1e7,
		}
		// the image name is a UNICODE_STRING whose buffer points into this same snapshot
		n := int(binary.LittleEndian.Uint16(b[off+56:]))
		at := int(uintptr(binary.LittleEndian.Uint64(b[off+64:])) - uintptr(unsafe.Pointer(&b[0])))
		if n > 0 && at >= 0 && at+n <= len(b) {
			u := make([]uint16, n/2)
			for j := range u {
				u[j] = binary.LittleEndian.Uint16(b[at+2*j:])
			}
			e.name = strings.ToLower(windows.UTF16ToString(u))
		}
		if onProc(e) && f != nil {
			for i := 0; i < threads; i++ {
				t := off + procEntryHeader + i*threadEntrySize
				if t+threadEntrySize > len(b) {
					break
				}
				f(threadInfo{
					pid:    pid,
					tid:    uint32(binary.LittleEndian.Uint64(b[t+48:])),
					base:   binary.LittleEndian.Uint32(b[t+60:]),
					state:  binary.LittleEndian.Uint32(b[t+68:]),
					reason: binary.LittleEndian.Uint32(b[t+72:]),
				})
			}
		}
		if next == 0 {
			break
		}
		off += int(next)
	}
	return true
}

func (s *ThreadStates) add(t threadInfo) {
	switch {
	case t.state == 2: // running
		s.Running++
	case t.state == 1 || t.state == 3 || t.state == 7: // ready, standby, deferred ready: wants a CPU
		s.Ready++
	case t.state == 5 && t.reason == 5: // waiting because suspended
		s.Suspended++
	case t.state == 5:
		s.Waiting++
	}
}

// threadStates looks at every thread of the given processes.
func threadStates(pids map[uint32]bool) (ThreadStates, bool) {
	var s ThreadStates
	ok := eachThread(func(pid uint32) bool { return pids[pid] }, s.add)
	return s, ok
}

// demoteThreads drops every thread of pids to the lowest priority, from a snapshot of its own.
func demoteThreads(pids map[uint32]bool, lowered map[uint32]bool) (changed int) {
	var ts []threadInfo
	eachThread(func(pid uint32) bool { return pids[pid] }, func(t threadInfo) { ts = append(ts, t) })
	return lowerThreads(ts, lowered)
}

// lowerThreads drops each thread to the lowest priority its process class allows, and records
// each one it changed in lowered so restoreThreads can put it back. Threads already that low are
// left alone, so a steady held-back chat costs nothing beyond the snapshot.
func lowerThreads(ts []threadInfo, lowered map[uint32]bool) (changed int) {
	for _, t := range ts {
		if t.base > 1 && setThreadPri(t.tid, threadPriorityIdle) {
			lowered[t.tid] = true
			changed++
		}
	}
	return changed
}

// restoreThreads puts back the threads lowerThreads lowered that are still in pids (a thread ID
// Windows reused in some other process is never touched).
func restoreThreads(pids map[uint32]bool, lowered map[uint32]bool) {
	eachThread(func(pid uint32) bool { return pids[pid] }, func(t threadInfo) {
		if lowered[t.tid] {
			setThreadPri(t.tid, threadPriorityNormal)
		}
	})
	clear(lowered)
}

func setThreadPri(tid uint32, pri int32) bool {
	h, err := windows.OpenThread(threadSetLimitedInfo, false, tid)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	r, _, _ := setThreadPriority.Call(uintptr(h), uintptr(pri))
	return r != 0
}
