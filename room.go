package main

// Room for one more heavy job. Two heavy-job slots are always there; past two, a slot is granted
// only while the PC has room for the job that asks: nothing full-screen in the foreground (a game,
// a video), the cores that job used on its last run still idle, and its last run's peak memory
// still free with a reserve to spare. A fixed two left heavy commands waiting 13 hours in the week
// to 2026-11-08 while the PC sat at about a third of its CPU; the room rule keeps the protection
// that count was for (six full suites at once made tests fail on their own time limits) by
// measuring the load instead of assuming it.
//
// The numbers below come from `fairshare bench slots` (benchslots.go) on this PC; see README.

import (
	"fmt"
	"runtime"
	"time"
	"unsafe"

	"github.com/Noisemaker111/fairshare/internal/windows"
)

const (
	// minHeavySlots always run, whatever the load: the old fixed count.
	minHeavySlots = 2
	// maxHeavySlots is the most heavy jobs at once, however idle the PC.
	maxHeavySlots = 4
	// spareCores stay idle after the new job's cores are counted, for the foreground and the
	// chats' own light work.
	spareCores = 4
	// spareMemMB stays free after the new job's peak memory is counted.
	spareMemMB = 8 << 10
	// settleTime is how long a newly granted job counts as not yet loading the PC: a suite
	// spends its first seconds starting, so the CPU it will use does not show yet.
	settleTime = 30 * time.Second
	// cpuWindow is how many one-second CPU samples a waiter averages before it may take a slot
	// past the minimum.
	cpuWindow = 5
)

// pcLoad is what the room rule sees of the PC now.
type pcLoad struct {
	CPUs       int
	CPUPct     float64 // whole PC, over the last few seconds
	AvailMB    uint64  // free physical memory
	FullScreen string  // what fills the screen in the foreground, or ""
}

// heavyNeed is what the job that asks used on its last run.
type heavyNeed struct {
	Cores  float64 // average cores: CPU-seconds over wall seconds
	PeakMB float64
}

func needOf(e heavyEntry) heavyNeed {
	n := heavyNeed{PeakMB: e.PeakMB}
	if e.WallSeconds > 0 {
		n.Cores = e.CPUSeconds / e.WallSeconds
	}
	return n
}

// heavyRoom reports whether one more heavy job may start beside held others, and if not, why.
// newest is how long ago the most recent holder was granted its slot.
func heavyRoom(held int, newest time.Duration, load pcLoad, need heavyNeed) (bool, string) {
	if held < minHeavySlots {
		return true, ""
	}
	if held >= maxHeavySlots {
		return false, fmt.Sprintf("%d heavy jobs already run, the most at once", held)
	}
	if load.FullScreen != "" {
		return false, load.FullScreen + " is full screen in the foreground"
	}
	if newest < settleTime {
		return false, fmt.Sprintf("the newest heavy job started %s ago and its load does not show yet", newest.Round(time.Second))
	}
	idle := float64(load.CPUs) * (1 - load.CPUPct/100)
	if idle-need.Cores < spareCores {
		return false, fmt.Sprintf("the PC is at %.0f%% CPU (%.0f of %d cores idle) and this command used %.1f cores on its last run", load.CPUPct, idle, load.CPUs, need.Cores)
	}
	if float64(load.AvailMB)-need.PeakMB < spareMemMB {
		return false, fmt.Sprintf("%.1f GB of memory is free and this command peaked at %.1f GB", float64(load.AvailMB)/1024, need.PeakMB/1024)
	}
	return true, ""
}

// loadWatch averages whole-PC CPU over the waiter's recent one-second looks.
type loadWatch struct {
	cs      cpuSampler
	primed  bool
	samples []float64
}

// look takes one sample; call it about once a second. ready is false until cpuWindow samples.
func (w *loadWatch) look() (load pcLoad, ready bool) {
	pct := w.cs.sample()
	if w.primed {
		w.samples = append(w.samples, pct)
		if len(w.samples) > cpuWindow {
			w.samples = w.samples[1:]
		}
	}
	w.primed = true
	var sum float64
	for _, x := range w.samples {
		sum += x
	}
	if len(w.samples) > 0 {
		load.CPUPct = sum / float64(len(w.samples))
	}
	load.CPUs = runtime.NumCPU()
	load.AvailMB, _, _ = MemStatus()
	load.FullScreen = fullScreenApp()
	return load, len(w.samples) >= cpuWindow
}

var (
	shell32                      = windows.NewLazySystemDLL("shell32.dll")
	shQueryUserNotificationState = shell32.NewProc("SHQueryUserNotificationState")
	getForegroundWindow          = user32.NewProc("GetForegroundWindow")
	getWindowRect                = user32.NewProc("GetWindowRect")
	monitorFromWindow            = user32.NewProc("MonitorFromWindow")
	getMonitorInfoW              = user32.NewProc("GetMonitorInfoW")
	getClassNameW                = user32.NewProc("GetClassNameW")
	getWindowThreadProcessId     = user32.NewProc("GetWindowThreadProcessId")
)

// fullScreenApp names what fills the screen in the foreground: a game in exclusive full screen,
// a full-screen app or presentation (as Windows reports it for notifications), or a borderless
// window covering its whole monitor, which is how most games run now. "" when nothing does.
func fullScreenApp() string {
	var state int32
	if r, _, _ := shQueryUserNotificationState.Call(uintptr(unsafe.Pointer(&state))); r == 0 {
		switch state {
		case 2, 3, 4, 7: // QUNS_BUSY, QUNS_RUNNING_D3D_FULL_SCREEN, QUNS_PRESENTATION_MODE, QUNS_APP
			return foregroundName()
		}
	}
	hwnd, _, _ := getForegroundWindow.Call()
	if hwnd == 0 {
		return ""
	}
	cls := make([]uint16, 64)
	n, _, _ := getClassNameW.Call(hwnd, uintptr(unsafe.Pointer(&cls[0])), uintptr(len(cls)))
	switch windows.UTF16ToString(cls[:n]) {
	case "Progman", "WorkerW", "Shell_TrayWnd": // the desktop and taskbar
		return ""
	}
	type rect struct{ L, T, R, B int32 }
	var w rect
	if r, _, _ := getWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&w))); r == 0 {
		return ""
	}
	mon, _, _ := monitorFromWindow.Call(hwnd, 2) // MONITOR_DEFAULTTONEAREST
	mi := struct {
		Size      uint32
		Mon, Work rect
		Flags     uint32
	}{Size: 40}
	if r, _, _ := getMonitorInfoW.Call(mon, uintptr(unsafe.Pointer(&mi))); r == 0 {
		return ""
	}
	if w.L <= mi.Mon.L && w.T <= mi.Mon.T && w.R >= mi.Mon.R && w.B >= mi.Mon.B {
		return foregroundName()
	}
	return ""
}

// foregroundName is the foreground window's program, such as "7DaysToDie.exe".
func foregroundName() string {
	hwnd, _, _ := getForegroundWindow.Call()
	var pid uint32
	getWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	if p, ok := liveProc(pid); ok && p.name != "" {
		return p.name
	}
	return "a full-screen app"
}
