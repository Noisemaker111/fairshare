//go:build windows

package main

// The meter: one small process that looks at the PC every few seconds and keeps the reading every
// other command answers from. Windows keeps no load average, so something has to. A look is two
// system calls (every processor's times, every process and thread) plus memory, disk and what is
// in the foreground; it changes nothing on the PC and touches no other process.
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
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

var (
	ntdll    = syscall.NewLazyDLL("ntdll.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	user32   = syscall.NewLazyDLL("user32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")

	procNtQuerySystemInformation     = ntdll.NewProc("NtQuerySystemInformation")
	procGlobalMemoryStatusEx         = kernel32.NewProc("GlobalMemoryStatusEx")
	procGetDiskFreeSpaceExW          = kernel32.NewProc("GetDiskFreeSpaceExW")
	procGetTickCount                 = kernel32.NewProc("GetTickCount")
	procCreateMutexW                 = kernel32.NewProc("CreateMutexW")
	procOpenMutexW                   = kernel32.NewProc("OpenMutexW")
	procProcessIdToSessionId         = kernel32.NewProc("ProcessIdToSessionId")
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
	shortWindow     = time.Minute
	longWindow      = 10 * time.Minute
)

// processorTimes returns each logical processor's idle and total (kernel plus user) time, in
// 100 ns units since the PC started. Windows counts idle time inside kernel time.
func processorTimes() (idle, total []uint64, err error) {
	buf := make([]byte, processorEntrySize*256)
	var got uint32
	st, _, _ := procNtQuerySystemInformation.Call(systemProcessorPerformanceInformation, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), uintptr(unsafe.Pointer(&got)))
	if st != 0 {
		return nil, nil, fmt.Errorf("NtQuerySystemInformation(processor times) status %#x", st)
	}
	n := int(got) / processorEntrySize
	idle, total = make([]uint64, n), make([]uint64, n)
	for i := range n {
		o := i * processorEntrySize
		idle[i] = binary.LittleEndian.Uint64(buf[o:])
		total[i] = binary.LittleEndian.Uint64(buf[o+8:]) + binary.LittleEndian.Uint64(buf[o+16:])
	}
	return idle, total, nil
}

// procLook is one process in a look.
type procLook struct {
	pid     uint32
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
	if os.Getenv("FAIRSHARE_RESERVE") == "measure" {
		return errors.New("FAIRSHARE_RESERVE=measure: worked out but not applied")
	}
	exe := owlExe(s)
	if _, err := os.Stat(exe); err != nil {
		return fmt.Errorf("Owl is not at %s, so nothing holds agent work off the reserve", exe)
	}
	cmd := exec.Command(exe, "agents", "set", "reserve_processors", strconv.Itoa(n), "--for", "3m", "--real-home", "--json")
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
			return fmt.Errorf("owl agents set reserve_processors %d: %v: %s", n, err, strings.TrimSpace(string(out[:min(len(out), 300)])))
		}
		return nil
	case <-time.After(15 * time.Second):
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		return fmt.Errorf("owl agents set reserve_processors %d did not answer in 15 s", n)
	}
}

type procKey struct {
	pid     uint32
	created uint64
}

type meter struct {
	prevIdle, prevTotal []uint64
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
}

// look measures the PC once and returns the reading as of now. The first look only sets the
// counters every later look is measured against, and returns ok false.
func (m *meter) look(s Settings, now time.Time) (Reading, bool, error) {
	idle, total, err := processorTimes()
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

	cpuNow := make(map[procKey]float64, len(procs))
	names := make(map[uint32]string, len(procs))
	programs, others := map[string]float64{}, map[string]float64{}
	mem, otherMem := map[string]float64{}, map[string]float64{}
	count, otherCount := map[string]int{}, map[string]int{}
	var waiting, workMem, workCPU, userCPU, userWaiting float64
	for _, p := range procs {
		if p.pid == 0 { // the idle process: its threads are "ready" whenever a processor is busy
			continue
		}
		names[p.pid] = p.name
		waiting += float64(p.ready)
		key := procKey{p.pid, p.created}
		cpuNow[key] = p.cpu
		// What it used since the last look; a process started since then used all it has.
		var used float64
		if before, seen := m.prevCPU[key]; seen && p.cpu > before {
			used = p.cpu - before
		} else if !seen && m.prevCPU != nil {
			used = p.cpu
		}
		if p.base > belowNormalBase || p.pid == 4 {
			if p.session == m.session && p.session != 0 {
				userCPU += used
				userWaiting += float64(p.ready)
			}
			others[p.name] += used
			otherMem[p.name] += p.private / (1 << 30)
			otherCount[p.name]++
			continue
		}
		workMem += p.private
		mem[p.name] += p.private / (1 << 30)
		count[p.name]++
		programs[p.name] += used
		workCPU += used
	}

	first := m.prevAt.IsZero() || len(m.prevIdle) != len(idle)
	seconds := now.Sub(m.prevAt).Seconds()
	var busy, userBusy float64
	if !first && seconds > 0 {
		for i := range idle {
			dt := float64(total[i] - m.prevTotal[i])
			if dt <= 0 {
				continue
			}
			used := 1 - float64(idle[i]-m.prevIdle[i])/dt
			used = max(0, min(1, used))
			if i < reserved {
				userBusy += used
			} else {
				busy += used
			}
		}
	}
	m.prevIdle, m.prevTotal, m.prevCPU = idle, total, cpuNow
	m.prevAt = now
	if first || seconds <= 0 {
		return Reading{}, false, nil
	}

	waiting = waitingWhenFull(waiting, busy, float64(len(idle)-reserved))
	userWaiting = waitingWhenFull(userWaiting, userBusy, float64(reserved))
	memTotal, memAvail := memStatus()
	m.win.add(sample{at: now, seconds: seconds, busy: busy, userBusy: userBusy, waiting: waiting,
		memAvailGB: memAvail, workCores: workCPU / seconds, programs: programs, others: others}, longWindow)

	r := Reading{OS: "windows", At: now.UTC(), Processors: len(idle), Reserved: reserved, Capacity: float64(len(idle) - reserved),
		MemTotalGB: round2(memTotal), WorkMemGB: round2(workMem / (1 << 30))}
	r.Machine, _ = os.Hostname()
	var progCPU, otherCPU map[string]float64
	var progSeconds float64
	r.BusyShort, r.WaitingShort, r.UserBusy, r.WorkCores, r.MemAvailGB, progCPU, otherCPU, progSeconds = m.win.means(now, shortWindow)
	r.ShortSeconds = progSeconds
	r.BusyLong, r.WaitingLong, _, _, _, _, _, r.LongSeconds = m.win.means(now, longWindow)
	r.DemandShort, r.DemandLong = r.BusyShort, r.BusyLong
	for _, f := range []*float64{&r.BusyShort, &r.BusyLong, &r.WaitingShort, &r.WaitingLong, &r.DemandShort, &r.DemandLong, &r.UserBusy, &r.WorkCores, &r.MemAvailGB, &r.ShortSeconds, &r.LongSeconds} {
		*f = round2(*f)
	}
	r.Programs = topPrograms(progCPU, progSeconds, mem, count, 8)
	r.Others = topPrograms(otherCPU, progSeconds, otherMem, otherCount, 5)

	r.DiskPath = s.DiskPath
	if r.DiskPath == "" {
		r.DiskPath, _ = os.UserHomeDir()
	}
	if free, err := diskFreeGB(r.DiskPath); err != nil {
		r.Notes = append(r.Notes, "disk not read: "+err.Error())
	} else {
		r.DiskFreeGB = round2(free)
	}

	idleFor := idleInput()
	r.Presence = Presence{State: "active", IdleMinutes: round2(idleFor.Minutes())}
	if app := fullScreenApp(names); app != "" {
		r.Presence.State, r.Presence.App = "full-screen", app
	} else if idleFor.Minutes() >= s.AwayMinutes {
		r.Presence.State = "away"
	}
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
	want, why := m.rv.want(now, userCPU/seconds, userWaiting, r.Presence, s, len(idle), m.perCore)
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

// historyLine is what the meter keeps of each minute.
type historyLine struct {
	At         time.Time `json:"at"`
	Capacity   float64   `json:"capacity"`
	Busy       float64   `json:"busy"`
	Waiting    float64   `json:"waiting"`
	UserBusy   float64   `json:"user_busy"`
	Reserved   int       `json:"reserved"`
	WorkCores  float64   `json:"work_cores"`
	MemAvailGB float64   `json:"mem_avail_gb"`
	DiskFreeGB float64   `json:"disk_free_gb"`
	Presence   string    `json:"presence"`
	App        string    `json:"app,omitempty"`
	Programs   []Program `json:"programs,omitempty"`
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
				appendLine("history.jsonl", historyLine{At: r.At, Capacity: r.Capacity, Busy: r.BusyShort, Waiting: r.WaitingShort,
					UserBusy: r.UserBusy, Reserved: r.Reserved, WorkCores: r.WorkCores, MemAvailGB: r.MemAvailGB, DiskFreeGB: r.DiskFreeGB,
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
