//go:build windows

package main

// The meter: one small process that looks at the PC every few seconds and keeps the reading every
// other command answers from. Windows keeps no load average, so something has to. A look is two
// system calls (every processor's times, every process and thread) plus memory, disk and what is
// in the foreground; it changes nothing on the PC. Of another process it only reads: the folder
// it works in, and its processor time, through a handle kept from the first look that sees it so
// that what it used in its last seconds is still there to read once it has ended.
//
// It never runs per command. fairshare's first version put a hook in front of every agent command,
// which cost 130-160 ms a call and broke commands, and was removed on 2026-11-08. This one is asked
// only when a thread is about to start.

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

var (
	ntdll    = syscall.NewLazyDLL("ntdll.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	user32   = syscall.NewLazyDLL("user32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
	dwmapi   = syscall.NewLazyDLL("dwmapi.dll")

	procNtQuerySystemInformation     = ntdll.NewProc("NtQuerySystemInformation")
	procGlobalMemoryStatusEx         = kernel32.NewProc("GlobalMemoryStatusEx")
	procGetDiskFreeSpaceExW          = kernel32.NewProc("GetDiskFreeSpaceExW")
	procGetTickCount                 = kernel32.NewProc("GetTickCount")
	procCreateMutexW                 = kernel32.NewProc("CreateMutexW")
	procOpenMutexW                   = kernel32.NewProc("OpenMutexW")
	procProcessIdToSessionId         = kernel32.NewProc("ProcessIdToSessionId")
	procReadProcessMemory            = kernel32.NewProc("ReadProcessMemory")
	procNtQueryInformationProcess    = ntdll.NewProc("NtQueryInformationProcess")
	procGetLogicalProcessorInfo      = kernel32.NewProc("GetLogicalProcessorInformation")
	procGetLastInputInfo             = user32.NewProc("GetLastInputInfo")
	procGetForegroundWindow          = user32.NewProc("GetForegroundWindow")
	procGetWindowRect                = user32.NewProc("GetWindowRect")
	procMonitorFromWindow            = user32.NewProc("MonitorFromWindow")
	procGetMonitorInfoW              = user32.NewProc("GetMonitorInfoW")
	procGetClassNameW                = user32.NewProc("GetClassNameW")
	procGetWindowThreadProcessId     = user32.NewProc("GetWindowThreadProcessId")
	procIsZoomed                     = user32.NewProc("IsZoomed")
	procSHQueryUserNotificationState = shell32.NewProc("SHQueryUserNotificationState")
	procDwmFlush                     = dwmapi.NewProc("DwmFlush")
)

const (
	systemProcessInformation              = 5
	systemProcessorPerformanceInformation = 8
	processorEntrySize                    = 48    // SYSTEM_PROCESSOR_PERFORMANCE_INFORMATION
	procEntryHeader                       = 0x100 // SYSTEM_PROCESS_INFORMATION on 64-bit Windows
	threadEntrySize                       = 0x50  // SYSTEM_THREAD_INFORMATION
	statusInfoLengthMismatch              = 0xC0000004
	// belowNormalBase is the base priority of the below-normal class. Owl holds every process of
	// agent work at it, so "base priority at or under this" is agent work, by Owl's own sorting.
	belowNormalBase = 6
	// idleBase is the base priority of the idle class, where Owl holds agent work that steps
	// aside for an urgent session.
	idleBase    = 4
	shortWindow = time.Minute
	longWindow  = 10 * time.Minute
)

// processorTimes returns each logical processor's idle and total (kernel plus user) time, in
// 100 ns units since the PC started, and the part of the total spent in interrupts, which is
// no process's. Windows counts idle and interrupt time inside kernel time.
func processorTimes() (idle, total, irq []uint64, err error) {
	buf := make([]byte, processorEntrySize*256)
	var got uint32
	st, _, _ := procNtQuerySystemInformation.Call(systemProcessorPerformanceInformation, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), uintptr(unsafe.Pointer(&got)))
	if st != 0 {
		return nil, nil, nil, fmt.Errorf("NtQuerySystemInformation(processor times) status %#x", st)
	}
	n := int(got) / processorEntrySize
	idle, total, irq = make([]uint64, n), make([]uint64, n), make([]uint64, n)
	for i := range n {
		o := i * processorEntrySize
		idle[i] = binary.LittleEndian.Uint64(buf[o:])
		total[i] = binary.LittleEndian.Uint64(buf[o+8:]) + binary.LittleEndian.Uint64(buf[o+16:])
		irq[i] = binary.LittleEndian.Uint64(buf[o+24:]) + binary.LittleEndian.Uint64(buf[o+32:]) // DPC and interrupt time
	}
	return idle, total, irq, nil
}

// procLook is one process in a look.
type procLook struct {
	pid     uint32
	parent  uint32
	created uint64
	cpu     float64 // user plus kernel seconds so far
	name    string
	base    int32 // base priority
	private float64
	session uint32
	ready   int // threads that want a processor and have none
}

var snapBuf []byte

// processes lists every process on the PC with one system call.
func processes() ([]procLook, error) {
	if snapBuf == nil {
		snapBuf = make([]byte, 2<<20)
	}
	for {
		var need uint32
		st, _, _ := procNtQuerySystemInformation.Call(systemProcessInformation, uintptr(unsafe.Pointer(&snapBuf[0])), uintptr(len(snapBuf)), uintptr(unsafe.Pointer(&need)))
		if st == statusInfoLengthMismatch {
			snapBuf = make([]byte, max(int(need)+1<<16, 2*len(snapBuf)))
			continue
		}
		if st != 0 {
			return nil, fmt.Errorf("NtQuerySystemInformation(processes) status %#x", st)
		}
		break
	}
	b := snapBuf
	var out []procLook
	for off := 0; off+procEntryHeader <= len(b); {
		next := binary.LittleEndian.Uint32(b[off:])
		threads := int(binary.LittleEndian.Uint32(b[off+4:]))
		p := procLook{
			pid:     uint32(binary.LittleEndian.Uint64(b[off+80:])),
			parent:  uint32(binary.LittleEndian.Uint64(b[off+88:])),
			created: binary.LittleEndian.Uint64(b[off+32:]),
			cpu:     float64(binary.LittleEndian.Uint64(b[off+40:])+binary.LittleEndian.Uint64(b[off+48:])) / 1e7,
			base:    int32(binary.LittleEndian.Uint32(b[off+72:])),
			private: float64(binary.LittleEndian.Uint64(b[off+200:])),
			session: binary.LittleEndian.Uint32(b[off+100:]),
		}
		// The image name is a UNICODE_STRING whose buffer points into this same snapshot.
		n := int(binary.LittleEndian.Uint16(b[off+56:]))
		at := int(uintptr(binary.LittleEndian.Uint64(b[off+64:])) - uintptr(unsafe.Pointer(&b[0])))
		if n > 0 && at >= 0 && at+n <= len(b) {
			u := make([]uint16, n/2)
			for j := range u {
				u[j] = binary.LittleEndian.Uint16(b[at+2*j:])
			}
			p.name = strings.ToLower(syscall.UTF16ToString(u))
		}
		for i := range threads {
			t := off + procEntryHeader + i*threadEntrySize
			if t+threadEntrySize > len(b) {
				break
			}
			// KTHREAD_STATE: 1 ready, 3 standby, 7 deferred ready.
			switch binary.LittleEndian.Uint32(b[t+68:]) {
			case 1, 3, 7:
				p.ready++
			}
		}
		out = append(out, p)
		if next == 0 {
			break
		}
		off += int(next)
	}
	return out, nil
}

// memAvailGB is the memory a new program could still take: free physical memory, or free commit
// (memory plus page file) when that is less.
func memStatus() (totalGB, availGB float64) {
	var m struct {
		Length               uint32
		MemoryLoad           uint32
		TotalPhys, AvailPhys uint64
		TotalPageFile        uint64
		AvailPageFile        uint64
		TotalVirtual         uint64
		AvailVirtual         uint64
		AvailExtendedVirtual uint64
	}
	m.Length = uint32(unsafe.Sizeof(m))
	procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m)))
	return float64(m.TotalPhys) / (1 << 30), float64(min(m.AvailPhys, m.AvailPageFile)) / (1 << 30)
}

func diskFreeGB(path string) (float64, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var free uint64
	if r, _, e := procGetDiskFreeSpaceExW.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&free)), 0, 0); r == 0 {
		return 0, e
	}
	return float64(free) / (1 << 30), nil
}

// idleInput is how long ago the keyboard or mouse was last used.
func idleInput() time.Duration {
	var li struct{ size, tick uint32 }
	li.size = 8
	if r, _, _ := procGetLastInputInfo.Call(uintptr(unsafe.Pointer(&li))); r == 0 {
		return 0
	}
	now, _, _ := procGetTickCount.Call()
	return time.Duration(uint32(now)-li.tick) * time.Millisecond
}

// fullScreenApp names what fills the screen in the foreground: a game in exclusive full screen, a
// full-screen app or presentation (as Windows reports it for notifications), or a borderless
// window covering its whole monitor, which is how most games run now. "" when nothing does.
func fullScreenApp(names map[uint32]string) string {
	hwnd, _, _ := procGetForegroundWindow.Call()
	name := func() string {
		var pid uint32
		procGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
		if n := names[pid]; n != "" {
			return n
		}
		return "a full-screen app"
	}
	var state int32
	if r, _, _ := procSHQueryUserNotificationState.Call(uintptr(unsafe.Pointer(&state))); r == 0 {
		switch state {
		case 2, 3, 4, 7: // QUNS_BUSY, QUNS_RUNNING_D3D_FULL_SCREEN, QUNS_PRESENTATION_MODE, QUNS_APP
			return name()
		}
	}
	if hwnd == 0 {
		return ""
	}
	cls := make([]uint16, 64)
	n, _, _ := procGetClassNameW.Call(hwnd, uintptr(unsafe.Pointer(&cls[0])), uintptr(len(cls)))
	switch syscall.UTF16ToString(cls[:n]) {
	case "Progman", "WorkerW", "Shell_TrayWnd": // the desktop and taskbar
		return ""
	}
	// A maximized window (a browser, an editor) covers the whole monitor when the taskbar hides
	// itself; a borderless game is not maximized.
	if z, _, _ := procIsZoomed.Call(hwnd); z != 0 {
		return ""
	}
	type rect struct{ L, T, R, B int32 }
	var w rect
	if r, _, _ := procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&w))); r == 0 {
		return ""
	}
	mon, _, _ := procMonitorFromWindow.Call(hwnd, 2) // MONITOR_DEFAULTTONEAREST
	mi := struct {
		Size      uint32
		Mon, Work rect
		Flags     uint32
	}{Size: 40}
	if r, _, _ := procGetMonitorInfoW.Call(mon, uintptr(unsafe.Pointer(&mi))); r == 0 {
		return ""
	}
	if w.L <= mi.Mon.L && w.T <= mi.Mon.T && w.R >= mi.Mon.R && w.B >= mi.Mon.B {
		return name()
	}
	return ""
}

// owlReserve is how many of the first logical processors Owl holds agent work off: its
// reserve_processors (8 unless changed), 0 when Owl is not on this PC or its keeper is off.
func owlReserve() int {
	b, err := os.ReadFile(filepath.Join(os.Getenv("LOCALAPPDATA"), "Owl", "agents.json"))
	if err != nil {
		return 0
	}
	var s struct {
		Enabled           *bool `json:"enabled"`
		ReserveProcessors *int  `json:"reserve_processors"`
	}
	if json.Unmarshal(b, &s) != nil {
		return 8
	}
	if s.Enabled != nil && !*s.Enabled {
		return 0
	}
	if s.ReserveProcessors != nil {
		return *s.ReserveProcessors
	}
	return 8
}

func hideWindow(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true} }

// logicalPerCore is the logical processors of one physical core (2 with SMT on).
func logicalPerCore(logical int) int {
	const entry = 32 // SYSTEM_LOGICAL_PROCESSOR_INFORMATION on 64-bit Windows
	buf := make([]byte, entry*512)
	size := uint32(len(buf))
	if r, _, _ := procGetLogicalProcessorInfo.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size))); r == 0 {
		return 1
	}
	cores := 0
	for o := 0; o+entry <= int(size); o += entry {
		if binary.LittleEndian.Uint32(buf[o+8:]) == 0 { // RelationProcessorCore
			cores++
		}
	}
	if cores == 0 || logical%cores != 0 {
		return 1
	}
	return logical / cores
}

// mySession is the Windows session this meter runs in: the user's own, since the task starts it
// there.
func mySession() uint32 {
	var id uint32
	procProcessIdToSessionId.Call(uintptr(os.Getpid()), uintptr(unsafe.Pointer(&id)))
	return id
}

func owlExe(s Settings) string {
	if s.OwlPath != "" {
		return s.OwlPath
	}
	return filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "Owl", "v2", "owl.exe")
}

// applyReserve tells Owl how many processors to hold agent work off, for a few minutes at a time:
// the meter says it again every minute, and if the meter ever stops, Owl puts its own number back.
func applyReserve(s Settings, n int) error {
	return owlAgents(s, "set", "reserve_processors", strconv.Itoa(n), "--for", "3m")
}

// tellYield tells Owl to hold every agent session that started before since at idle priority,
// for three minutes; the meter says it again every minute while the urgent hold stands.
func tellYield(s Settings, since time.Time) error {
	return owlAgents(s, "set", "yield_before", strconv.FormatInt(since.Unix(), 10), "--for", "3m")
}

// endYield gives the held sessions back their below-normal priority now, rather than when the
// last three minutes Owl was told run out.
func endYield(s Settings) error {
	return owlAgents(s, "unset", "yield_before")
}

// owlAgents runs one `owl agents` command as fairshare, on Owl's real home.
func owlAgents(s Settings, args ...string) error {
	what := "owl agents " + strings.Join(args, " ")
	if os.Getenv("FAIRSHARE_RESERVE") == "measure" {
		return fmt.Errorf("FAIRSHARE_RESERVE=measure: %s worked out but not told", what)
	}
	exe := owlExe(s)
	if _, err := os.Stat(exe); err != nil {
		return fmt.Errorf("Owl is not at %s, so nothing holds agent work", exe)
	}
	cmd := exec.Command(exe, append(append([]string{"agents"}, args...), "--real-home", "--json")...)
	cmd.Env = append(os.Environ(), "OWL_CALLER=fairshare")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	done := make(chan error, 1)
	var out []byte
	go func() {
		var err error
		out, err = cmd.CombinedOutput()
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("%s: %v: %s", what, err, strings.TrimSpace(string(out[:min(len(out), 300)])))
		}
		// Owl answers a setting it does not have with an error in its JSON and exit 0 (2026-11-11:
		// yield_before, on an Owl from before it had one), so the exit code alone says nothing.
		var said struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(jsonPart(out), &said) == nil && said.Error != "" {
			return fmt.Errorf("%s: Owl refused: %s", what, said.Error)
		}
		return nil
	case <-time.After(15 * time.Second):
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		return fmt.Errorf("%s did not answer in 15 s", what)
	}
}

// processDir is the current folder of another process, read from its process parameters the way
// Task Manager reads a command line; it changes nothing. "" when Windows refuses.
func processDir(pid uint32) string {
	const queryLimited, vmRead = 0x1000, 0x10
	h, err := syscall.OpenProcess(queryLimited|vmRead, false, pid)
	if err != nil {
		return ""
	}
	defer syscall.CloseHandle(h)
	read := func(at uintptr, into unsafe.Pointer, size uintptr) bool {
		var got uintptr
		r, _, _ := procReadProcessMemory.Call(uintptr(h), at, uintptr(into), size, uintptr(unsafe.Pointer(&got)))
		return r != 0 && got == size
	}
	var basic [6]uintptr // PROCESS_BASIC_INFORMATION: PebBaseAddress is the second word
	if st, _, _ := procNtQueryInformationProcess.Call(uintptr(h), 0, uintptr(unsafe.Pointer(&basic[0])), unsafe.Sizeof(basic), 0); st != 0 || basic[1] == 0 {
		return ""
	}
	var params uintptr // PEB.ProcessParameters at 0x20
	if !read(basic[1]+0x20, unsafe.Pointer(&params), unsafe.Sizeof(params)) || params == 0 {
		return ""
	}
	var dir struct { // RTL_USER_PROCESS_PARAMETERS.CurrentDirectory.DosPath at 0x38
		Length, MaxLength uint16
		_                 uint32
		Buffer            uintptr
	}
	if !read(params+0x38, unsafe.Pointer(&dir), unsafe.Sizeof(dir)) || dir.Length == 0 || dir.Length > 4096 || dir.Buffer == 0 {
		return ""
	}
	u := make([]uint16, dir.Length/2)
	if !read(dir.Buffer, unsafe.Pointer(&u[0]), uintptr(dir.Length)) {
		return ""
	}
	return syscall.UTF16ToString(u)
}

// frameWatch times Windows' window manager from the meter's own process: DwmFlush returns once
// the next frame of the desktop is drawn, which takes about one refresh of the screen, and as
// long as the window manager is stalled when it is. It asks nothing of any other program.
//
// Why: processors, memory and disk can all have room while the desktop cannot be used (room.go,
// desktopFree). This is the measurement that shows it, in the reading and the history.
type frameWatch struct {
	mu    sync.Mutex
	ms    []float64 // the frames timed since the last take
	began time.Time // when the frame now awaited was asked for; zero between frames
}

// watch times one frame five times a second, for ever. It ends at once on a Windows with no
// window manager to ask.
func (f *frameWatch) watch() {
	if procDwmFlush.Find() != nil {
		return
	}
	for {
		f.mu.Lock()
		f.began = time.Now()
		f.mu.Unlock()
		hr, _, _ := procDwmFlush.Call()
		f.mu.Lock()
		// A failure (nothing is being drawn: the screen is locked) is not a frame.
		if took := time.Since(f.began); hr == 0 && len(f.ms) < 4096 {
			f.ms = append(f.ms, took.Seconds()*1000)
		}
		f.began = time.Time{}
		f.mu.Unlock()
		time.Sleep(200 * time.Millisecond)
	}
}

// take is the frames timed since the last take: how long one usually took (the median), the
// slowest, and how many. A frame still awaited counts as taking what it has taken so far, so a
// stall that outlasts a look is in that look.
func (f *frameWatch) take(now time.Time) (usual, worst float64, n int) {
	f.mu.Lock()
	ms := f.ms
	f.ms = nil
	if !f.began.IsZero() {
		worst = now.Sub(f.began).Seconds() * 1000
	}
	f.mu.Unlock()
	if len(ms) == 0 {
		return 0, 0, 0
	}
	sort.Float64s(ms)
	return ms[len(ms)/2], max(worst, ms[len(ms)-1]), len(ms)
}

// heldProc is what the meter keeps of a process from the first look that sees it: the process as
// the latest look saw it, and a handle that can do nothing but read its times. When the process
// ends, what it used after the last look that saw it is read from the handle.
type heldProc struct {
	h    syscall.Handle // 0 when Windows refused one
	seen procLook
}

// endedProc is a process that ended since the look before.
type endedProc struct {
	seen procLook
	used float64 // processor-seconds after the last look that saw it
	at   time.Time
}

func ftime(f syscall.Filetime) uint64 { return uint64(f.HighDateTime)<<32 | uint64(f.LowDateTime) }

// keep notes a process this look saw, and the first time opens the handle its last seconds will
// be read from. The handle is for the process the look saw and no other: process ids are reused.
func (m *meter) keep(p procLook) {
	k := procKey{p.pid, p.created}
	if e, ok := m.held[k]; ok {
		e.seen = p
		return
	}
	if m.held == nil {
		m.held = map[procKey]*heldProc{}
	}
	e := &heldProc{seen: p}
	const queryLimited = 0x1000
	if h, err := syscall.OpenProcess(queryLimited, false, p.pid); err == nil {
		var began, end, kernel, user syscall.Filetime
		if syscall.GetProcessTimes(h, &began, &end, &kernel, &user) == nil && ftime(began) == p.created {
			e.h = h
		} else {
			syscall.CloseHandle(h)
		}
	}
	m.held[k] = e
}

// ended is the processes that ended since the look before, each with what it used after that
// look, and lets go of them. alive is the processes of this look.
func (m *meter) ended(alive map[procKey]float64) []endedProc {
	var out []endedProc
	for k, e := range m.held {
		if _, ok := alive[k]; ok {
			continue
		}
		if e.h != 0 {
			var began, end, kernel, user syscall.Filetime
			if syscall.GetProcessTimes(e.h, &began, &end, &kernel, &user) == nil {
				if used := float64(ftime(kernel)+ftime(user))/1e7 - e.seen.cpu; used > 0 {
					out = append(out, endedProc{seen: e.seen, used: used, at: filetime(ftime(end))})
				}
			}
			syscall.CloseHandle(e.h)
		}
		delete(m.held, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].seen.pid < out[j].seen.pid })
	return out
}

type meter struct {
	prevIdle, prevTotal []uint64
	prevIrq             []uint64
	held                map[procKey]*heldProc
	prevCPU             map[procKey]float64
	prevAt              time.Time
	win                 window
	lastHistory         time.Time
	rv                  reserver
	applied             int // the reserve Owl was last told
	told                bool
	appliedAt           time.Time
	applyErr            string
	session             uint32
	perCore             int
	lastState           string
	// The urgent hold Owl was last told, and when; yieldErr is what went wrong telling it.
	yieldSince, yieldAt time.Time
	yieldErr            string
	// Which work uses what (work.go), and what was learned from it (learn.go).
	track tracker
	known learned
	// How long the window manager takes to draw a frame.
	frames frameWatch
}

// filetime turns a Windows time (100 ns units since 1601) into a time.
func filetime(v uint64) time.Time {
	const toUnix = 116444736000000000
	if v < toUnix {
		return time.Time{}
	}
	return time.Unix(0, int64(v-toUnix)*100)
}

// whereWindows says the folder a process runs in. The thread it was started for is not read on
// Windows, so a process with nothing above it is placed by its folder alone.
func whereWindows(pid uint32, _ uint64) (dir, thread string) { return processDir(pid), "" }

// look measures the PC once and returns the reading as of now. The first look only sets the
// counters every later look is measured against, and returns ok false.
func (m *meter) look(s Settings, now time.Time) (Reading, bool, error) {
	idle, total, irq, err := processorTimes()
	if err != nil {
		return Reading{}, false, err
	}
	procs, err := processes()
	if err != nil {
		return Reading{}, false, err
	}
	if m.perCore == 0 {
		m.perCore, m.session = logicalPerCore(len(idle)), mySession()
		m.rv.apps = loadApps()
	}
	// The split of this look's processor time uses the reserve that was in force during it.
	reserved := owlReserve()
	if s.ReservedProcessors != nil {
		reserved = *s.ReservedProcessors
	} else if m.told && m.applyErr == "" {
		reserved = m.applied
	}
	reserved = max(0, min(reserved, len(idle)-1))

	// The processors in use since the look before: on the agent processors, on the reserved ones,
	// and in all as the processor-seconds some process used (interrupts are nobody's).
	first := m.prevAt.IsZero() || len(m.prevIdle) != len(idle)
	seconds := now.Sub(m.prevAt).Seconds()
	var busy, userBusy, spent float64
	if !first && seconds > 0 {
		for i := range idle {
			dt := float64(total[i] - m.prevTotal[i])
			if dt <= 0 {
				continue
			}
			rest := float64(idle[i] - m.prevIdle[i])
			used := max(0, min(1, 1-rest/dt))
			if i < reserved {
				userBusy += used
			} else {
				busy += used
			}
			spent += max(0, dt-rest-float64(irq[i]-m.prevIrq[i])) / 1e7
		}
	}

	// Every process of this look with what it used since the look before, then the processes that
	// ended since then with what they used after it.
	cpuNow := make(map[procKey]float64, len(procs))
	names := make(map[uint32]string, len(procs))
	seen := make([]procLook, 0, len(procs)+16)
	nodes := make([]procNode, 0, len(procs)+16)
	node := func(p procLook, used float64) procNode {
		return procNode{pid: p.pid, parent: p.parent, created: p.created, name: p.name, cores: used, memGB: p.private / (1 << 30),
			work: p.base <= belowNormalBase && p.pid != 4, started: filetime(p.created), total: p.cpu}
	}
	for _, p := range procs {
		if p.pid == 0 { // the idle process: its threads are "ready" whenever a processor is busy
			continue
		}
		names[p.pid] = p.name
		key := procKey{p.pid, p.created}
		cpuNow[key] = p.cpu
		// What it used since the last look; a process started since then used all it has.
		n := node(p, 0)
		if before, known := m.prevCPU[key]; known && p.cpu > before {
			n.cores = p.cpu - before
		} else if !known && m.prevCPU != nil {
			n.cores, n.fresh = p.cpu, true
		}
		seen, nodes = append(seen, p), append(nodes, n)
	}
	for _, e := range m.ended(cpuNow) {
		n := node(e.seen, e.used)
		n.gone, n.ended, n.memGB = true, e.at, 0
		seen, nodes = append(seen, e.seen), append(nodes, n)
	}
	for _, p := range procs {
		if p.pid != 0 {
			m.keep(p)
		}
	}
	// What no process accounts for was used by processes no look saw (work.go, creditUnseen).
	if !first && seconds > 0 {
		var counted float64
		for _, n := range nodes {
			counted += n.cores
		}
		creditUnseen(nodes, spent-counted)
	}

	programs, others := map[string]float64{}, map[string]float64{}
	mem, otherMem := map[string]float64{}, map[string]float64{}
	count, otherCount := map[string]int{}, map[string]int{}
	var waiting, workMem, workCPU, idleCPU, userCPU, userWaiting, gameCPU, otherCPU float64
	game := ""
	for i, n := range nodes {
		p, used, live := seen[i], n.cores, !n.gone
		if live {
			waiting += float64(p.ready)
		}
		if p.base > belowNormalBase || p.pid == 4 {
			if p.session == m.session && p.session != 0 {
				userCPU += used
				if live {
					userWaiting += float64(p.ready)
				}
				if m.rv.isGame(p.name, s) {
					gameCPU += used
					if game == "" {
						game = p.name
					}
				}
			}
			others[p.name] += used
			otherCPU += used
			if live {
				otherMem[p.name] += p.private / (1 << 30)
				otherCount[p.name]++
			}
			continue
		}
		if live {
			workMem += p.private
			mem[p.name] += p.private / (1 << 30)
			count[p.name]++
		}
		programs[p.name] += used
		workCPU += used
		if p.base <= idleBase {
			idleCPU += used
		}
	}

	m.prevIdle, m.prevTotal, m.prevIrq, m.prevCPU = idle, total, irq, cpuNow
	m.prevAt = now
	if first || seconds <= 0 {
		return Reading{}, false, nil
	}

	waiting = waitingWhenFull(waiting, busy, float64(len(idle)-reserved))
	userWaiting = waitingWhenFull(userWaiting, userBusy, float64(reserved))
	memTotal, memAvail := memStatus()
	work := m.measure(s, now, seconds, nodes, names)
	holding, hold := m.urgent(s, now, work)
	firm := firmCores(busy, workCPU/seconds, idleCPU/seconds, holding)
	idleFor := idleInput()
	presence := Presence{State: "active", IdleMinutes: round2(idleFor.Minutes())}
	if app := fullScreenApp(names); app != "" {
		presence.State, presence.App = "full-screen", app
	} else if idleFor.Minutes() >= s.AwayMinutes {
		presence.State = "away"
	}
	// Frames are timed for the person looking at them: with nobody there the screen may be off,
	// and a frame nobody draws is not a slow one.
	frameMs, frameWorst, frames := m.frames.take(now)
	if presence.State == "away" {
		frameMs, frameWorst, frames = 0, 0, 0
	}
	m.win.add(sample{at: now, seconds: seconds, busy: busy, userBusy: userBusy, waiting: waiting, firm: firm, idleCores: idleCPU / seconds,
		memAvailGB: memAvail, workCores: workCPU / seconds, programs: programs, others: others, otherCores: otherCPU / seconds, hasOthers: true,
		frameMs: frameMs, frameWorst: frameWorst, frames: frames}, longWindow)

	r := Reading{OS: "windows", At: now.UTC(), Processors: len(idle), Reserved: reserved, Capacity: float64(len(idle) - reserved),
		MemTotalGB: round2(memTotal), WorkMemGB: round2(workMem / (1 << 30))}
	r.Work = work
	r.OtherCores = round2(m.win.othersMean(now, shortWindow))
	r.FrameMs, r.FrameWorstMs = m.win.frameTimes(now, shortWindow)
	r.Machine, _ = os.Hostname()
	var progCPU, otherProgCPU map[string]float64
	var progSeconds float64
	r.BusyShort, r.WaitingShort, r.UserBusy, r.WorkCores, r.MemAvailGB, progCPU, otherProgCPU, progSeconds = m.win.means(now, shortWindow)
	r.ShortSeconds = progSeconds
	r.BusyLong, r.WaitingLong, _, _, _, _, _, r.LongSeconds = m.win.means(now, longWindow)
	r.DemandShort, r.DemandLong = r.BusyShort, r.BusyLong
	r.FirmShort, r.IdleCores = m.win.firmMeans(now, shortWindow)
	r.FirmLong, _ = m.win.firmMeans(now, longWindow)
	if holding {
		r.Urgent = &hold
		if m.yieldErr != "" {
			r.Notes = append(r.Notes, "the urgent hold is recorded but Owl is not holding anything back for it: "+m.yieldErr)
		}
	}
	for _, f := range []*float64{&r.BusyShort, &r.BusyLong, &r.WaitingShort, &r.WaitingLong, &r.DemandShort, &r.DemandLong, &r.FirmShort, &r.FirmLong, &r.IdleCores, &r.UserBusy, &r.WorkCores, &r.MemAvailGB, &r.ShortSeconds, &r.LongSeconds} {
		*f = round2(*f)
	}
	r.Programs = topPrograms(progCPU, progSeconds, mem, count, 8)
	r.Others = topPrograms(otherProgCPU, progSeconds, otherMem, otherCount, 5)

	r.DiskPath = s.DiskPath
	if r.DiskPath == "" {
		r.DiskPath, _ = os.UserHomeDir()
	}
	if free, err := diskFreeGB(r.DiskPath); err != nil {
		r.Notes = append(r.Notes, "disk not read: "+err.Error())
	} else {
		r.DiskFreeGB = round2(free)
	}

	r.Presence = presence
	m.lastState = r.Presence.State

	r.UserCores = round2(userCPU / seconds)
	if s.ReservedProcessors != nil {
		r.ReserveWhy = "reserved_processors in settings.json pins it"
		return r, true, nil
	}
	// Only the user's own programs size the reserve. Windows' services (search indexing and
	// Defender read every file a build writes, 4.4 processors at their peak on 2026-11-10) run at
	// normal priority on any processor, so they need none kept; counting them made the reserve 8
	// again with the user's programs at 0.3. The user's threads waiting is the check on this.
	// Measured again on 2026-11-11 under a full agent load: 2 kept or 8 kept, windows answered in
	// 0.1 ms and the window manager's frames differed by 0 to 15 ms, with no stall either way.
	if app := strings.ToLower(r.Presence.App); game == "" && r.Presence.State == "full-screen" && app != "" && !notGame(app, s) {
		game, gameCPU = app, others[app]
	}
	want, why := m.rv.want(now, game, gameCPU/seconds, userWaiting, r.Presence, s, len(idle), m.perCore)
	m.rv.save()
	// Tell Owl when the number changes, and again every minute so it does not run out.
	if !m.told || want != m.applied || now.Sub(m.appliedAt) >= time.Minute {
		if err := applyReserve(s, want); err != nil {
			m.applyErr = err.Error()
		} else {
			m.applyErr = ""
		}
		m.applied, m.appliedAt, m.told = want, now, true
	}
	if m.applyErr != "" {
		r.Notes = append(r.Notes, fmt.Sprintf("the reserve would be %d (%s) but was not applied: %s", want, why, m.applyErr))
		r.ReserveWhy = "Owl's own setting; " + m.applyErr
		return r, true, nil
	}
	r.Reserved, r.Capacity, r.ReserveWhy = want, float64(len(idle)-want), why
	// Looks from before the reserve changed were split at the old number.
	r.UserBusy = min(r.UserBusy, float64(want))
	return r, true, nil
}

// urgent keeps the urgent hold: it ends the hold once it no longer stands (urgentHold.live), and
// while it stands tells Owl again every minute, so Owl's three minutes never run out under it.
// work is the work open in this look.
func (m *meter) urgent(s Settings, now time.Time, work []LiveWork) (bool, urgentHold) {
	hold, ok := loadUrgent()
	if !ok {
		if !m.yieldSince.IsZero() {
			endYield(s)
			m.yieldSince = time.Time{}
		}
		return false, hold
	}
	if !hold.live(now, hold.runs(work), s) {
		os.Remove(urgentFile())
		if err := endYield(s); err != nil {
			fmt.Fprintln(os.Stderr, "degraded:", err)
		}
		appendLine("decisions.jsonl", map[string]any{"at": now.UTC(), "event": "urgent ended", "since": hold.Since, "labels": hold.Labels, "repos": hold.Repos}, 8<<20)
		m.yieldSince = time.Time{}
		return false, hold
	}
	if !hold.Since.Equal(m.yieldSince) || now.Sub(m.yieldAt) >= time.Minute {
		m.yieldErr = ""
		if err := tellYield(s, hold.Since); err != nil {
			m.yieldErr = err.Error()
			fmt.Fprintln(os.Stderr, "degraded:", err)
		}
		m.yieldSince, m.yieldAt = hold.Since, now
	}
	return true, hold
}

// measure puts this look's processes under their work, records what was learned from the pieces
// that finished a minute or ended, and returns the work open now.
func (m *meter) measure(s Settings, now time.Time, seconds float64, nodes []procNode, names map[uint32]string) []LiveWork {
	if m.known == nil {
		m.known = loadLearned()
	}
	alive := func(pid uint32) bool { return strings.HasPrefix(names[pid], "fairshare") }
	if learnt := m.track.look(now, seconds, nodes, readMarks(alive), whereWindows, s); len(learnt) > 0 {
		m.known = learn(learnt, now, s)
	}
	return m.track.live(now, m.known, s)
}

// historyLine is what the meter keeps of each minute.
type historyLine struct {
	At        time.Time `json:"at"`
	Capacity  float64   `json:"capacity"`
	Busy      float64   `json:"busy"`
	Firm      float64   `json:"firm"`
	Waiting   float64   `json:"waiting"`
	UserBusy  float64   `json:"user_busy"`
	Reserved  int       `json:"reserved"`
	WorkCores float64   `json:"work_cores"`
	Others    float64   `json:"others"`
	// FrameMs is how long the window manager usually took to draw a frame in the minute, and
	// FrameWorstMs the slowest; both 0 when nobody was at the PC.
	FrameMs      float64   `json:"frame_ms,omitempty"`
	FrameWorstMs float64   `json:"frame_worst_ms,omitempty"`
	MemAvailGB   float64   `json:"mem_avail_gb"`
	DiskFreeGB   float64   `json:"disk_free_gb"`
	Presence     string    `json:"presence"`
	App          string    `json:"app,omitempty"`
	Programs     []Program `json:"programs,omitempty"`
}

func meterMutexName() string {
	h := fnv.New32a()
	h.Write([]byte(strings.ToLower(filepath.Clean(homeDir()))))
	return fmt.Sprintf(`Local\fairshare-meter-%08x`, h.Sum32())
}

func stopFile() string { return filepath.Join(homeDir(), "meter.stop") }

// holdMeterMutex makes this the one meter for its home. ok is false when another holds it.
func holdMeterMutex() (ok bool, err error) {
	name, err := syscall.UTF16PtrFromString(meterMutexName())
	if err != nil {
		return false, err
	}
	h, _, e := procCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(name)))
	if h == 0 {
		return false, e
	}
	return !errors.Is(e, syscall.ERROR_ALREADY_EXISTS), nil
}

// runMeter looks at the PC every sample_seconds until meter.stop appears in its home.
func runMeter() error {
	if err := os.MkdirAll(homeDir(), 0o755); err != nil {
		return err
	}
	ok, err := holdMeterMutex()
	if err != nil {
		return err
	}
	if !ok {
		return nil // the meter already runs; the scheduled task starts this every minute to find out
	}
	os.Remove(stopFile())
	var m meter
	go m.frames.watch()
	if lines, err := readHistory(time.Now().Add(-longWindow)); err == nil {
		m.win.seed(lines, time.Now(), longWindow)
	}
	failures := 0
	for {
		s, err := loadSettings()
		if err != nil {
			fmt.Fprintln(os.Stderr, "degraded:", err)
		}
		now := time.Now()
		r, got, err := m.look(s, now)
		switch {
		case err != nil:
			failures++
			fmt.Fprintln(os.Stderr, "degraded:", err)
			if failures >= 5 {
				return err
			}
		case got:
			failures = 0
			if err := writeReading(r); err != nil {
				fmt.Fprintln(os.Stderr, "degraded: reading not written:", err)
			}
			go moveLine(r, s, now)
			if now.Sub(m.lastHistory) >= time.Minute {
				m.lastHistory = now
				appendLine("history.jsonl", historyLine{At: r.At, Capacity: r.Capacity, Busy: r.BusyShort, Firm: r.FirmShort, Waiting: r.WaitingShort,
					UserBusy: r.UserBusy, Reserved: r.Reserved, WorkCores: r.WorkCores, Others: r.OtherCores, FrameMs: r.FrameMs, FrameWorstMs: r.FrameWorstMs, MemAvailGB: r.MemAvailGB, DiskFreeGB: r.DiskFreeGB,
					Presence: r.Presence.State, App: r.Presence.App, Programs: r.Programs[:min(len(r.Programs), 5)]}, 16<<20)
			}
		}
		for wait := time.Duration(s.SampleSeconds * float64(time.Second)); wait > 0; wait -= 250 * time.Millisecond {
			if _, err := os.Stat(stopFile()); err == nil {
				os.Remove(stopFile())
				return nil
			}
			if m.lastState == "away" && idleInput() < 2*time.Second {
				break // someone is back: look now, so the reserve is theirs within the second
			}
			time.Sleep(min(wait, 250*time.Millisecond))
		}
	}
}

// meterRunning reports whether a meter holds this home's mutex.
func meterRunning() bool {
	name, err := syscall.UTF16PtrFromString(meterMutexName())
	if err != nil {
		return false
	}
	const synchronize = 0x00100000
	h, _, _ := procOpenMutexW.Call(synchronize, 0, uintptr(unsafe.Pointer(name)))
	if h == 0 {
		return false
	}
	syscall.CloseHandle(syscall.Handle(h))
	return true
}

// currentReading is the meter's latest reading. A reading older than a few looks means the meter
// is not running, and that is an error rather than a stale answer.
func currentReading(s Settings) (Reading, error) {
	r, err := readReadingFile(stateFile())
	if err != nil {
		return r, fmt.Errorf("the meter has written no reading (%v); run: fairshare install", err)
	}
	limit := time.Duration(max(3*s.SampleSeconds, 20) * float64(time.Second))
	if age := time.Since(r.At); age > limit {
		return r, fmt.Errorf("the meter's reading is %.0f s old, so the meter is not running; run: fairshare install", age.Seconds())
	}
	return r, nil
}
