package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/Noisemaker111/fairshare/internal/windows"
)

var (
	user32         = windows.NewLazySystemDLL("user32.dll")
	createDesktopW = user32.NewProc("CreateDesktopW")
	closeDesktop   = user32.NewProc("CloseDesktop")
)

// spawn starts cmdline with no window. With console, the process gets its own hidden console
// host (conhost.exe), the way agent harnesses start shells; without it, it gets none. desktop
// names the desktop it is created on ("" = the current one).
func spawn(cmdline string, console bool, desktop string) (windows.Handle, uint32, error) {
	si := new(windows.StartupInfo)
	si.Cb = uint32(unsafe.Sizeof(*si))
	if desktop != "" {
		si.Desktop, _ = windows.UTF16PtrFromString(`WinSta0\` + desktop)
	}
	flags := uint32(windows.DETACHED_PROCESS)
	if console {
		flags = windows.CREATE_NO_WINDOW
	}
	pi := new(windows.ProcessInformation)
	cl, _ := windows.UTF16PtrFromString(cmdline)
	if err := windows.CreateProcess(nil, cl, nil, nil, false, flags, nil, nil, si, pi); err != nil {
		return 0, 0, err
	}
	windows.CloseHandle(pi.Thread)
	return pi.Process, pi.ProcessId, nil
}

func exitCodeAfter(h windows.Handle, wait time.Duration) (uint32, bool) {
	ev, _ := windows.WaitForSingleObject(h, uint32(wait.Milliseconds()))
	if ev != windows.WAIT_OBJECT_0 {
		return 0, false
	}
	var code uint32
	windows.GetExitCodeProcess(h, &code)
	return code, true
}

func cmdCeiling(args []string) int {
	fs := flag.NewFlagSet("ceiling", flag.ExitOnError)
	what := fs.String("what", "idle", "idle (hold N idle processes) or spawn (start-and-exit rate)")
	proc := fs.String("proc", "tiny", "idle: tiny (a minimal Go process), node or cmd; spawn: the command line to start")
	n := fs.Int("n", 500, "idle: how many to hold at most")
	step := fs.Int("step", 100, "idle: report after every this many")
	console := fs.Bool("console", true, "give each process its own hidden console host, as agent shells get")
	ownDesktop := fs.Bool("own-desktop", true, "start them on a separate hidden desktop so Jon's desktop is never affected")
	par := fs.Int("par", 8, "spawn: how many launchers at once")
	secs := fs.Float64("secs", 10, "spawn: how long")
	stopFreeMB := fs.Uint64("stop-free-mb", 8192, "idle: stop before free RAM or free commit falls below this")
	fs.Parse(args)

	if _, err := killOnExitJob(); err != nil {
		fmt.Fprintln(os.Stderr, "job:", err)
		return 1
	}
	desktop := ""
	if *ownDesktop {
		desktop = fmt.Sprintf("fairshare-ceiling-%d", os.Getpid())
		name, _ := windows.UTF16PtrFromString(desktop)
		d, _, err := createDesktopW.Call(uintptr(unsafe.Pointer(name)), 0, 0, 0, uintptr(windows.GENERIC_ALL), 0)
		if d == 0 {
			fmt.Fprintln(os.Stderr, "create desktop:", err)
			return 1
		}
		defer closeDesktop.Call(d)
	}
	exe, _ := os.Executable()

	if *what == "spawn" {
		var count, fails atomic.Int64
		var mu sync.Mutex
		var lat []float64
		var firstErr string
		deadline := time.Now().Add(time.Duration(*secs * float64(time.Second)))
		var wg sync.WaitGroup
		for range *par {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for time.Now().Before(deadline) {
					t := time.Now()
					h, _, err := spawn(*proc, *console, desktop)
					if err == nil {
						code, done := exitCodeAfter(h, 30*time.Second)
						windows.CloseHandle(h)
						if !done || code != 0 {
							err = fmt.Errorf("exit 0x%X", code)
						}
					}
					mu.Lock()
					if err != nil {
						fails.Add(1)
						if firstErr == "" {
							firstErr = err.Error()
						}
					} else {
						count.Add(1)
						lat = append(lat, float64(time.Since(t))/1e6)
					}
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		fmt.Printf("spawn %q par=%d: %.1f starts/s, start-to-exit p50 %.0f ms p99 %.0f ms, %d failed %s\n",
			*proc, *par, float64(count.Load())/(*secs), percentile(lat, .5), percentile(lat, .99), fails.Load(), firstErr)
		return 0
	}

	cmdline := fmt.Sprintf(`"%s" sleep`, exe)
	switch *proc {
	case "node":
		cmdline = `node -e "setInterval(()=>{},1e9)"`
	case "cmd": // waits on its console for input forever: the lightest console process there is
		cmdline = `cmd /d /k`
	}
	phys0, commit0, _ := MemStatus()
	var handles []windows.Handle
	var spawnMs []float64
	fmt.Printf("holding up to %d idle %s processes (console host each: %v, own desktop: %v); free RAM %d MB, free commit %d MB\n", *n, *proc, *console, *ownDesktop, phys0, commit0)
	for i := 1; i <= *n; i++ {
		t := time.Now()
		h, _, err := spawn(cmdline, *console, desktop)
		if err != nil {
			fmt.Printf("  #%d: start failed: %v\n", i, err)
			break
		}
		spawnMs = append(spawnMs, float64(time.Since(t))/1e6)
		handles = append(handles, h)
		if i%*step == 0 || i == *n {
			time.Sleep(1500 * time.Millisecond) // let them finish starting before counting
			dead := 0
			var codes []string
			for _, h := range handles {
				if code, done := exitCodeAfter(h, 0); done {
					dead++
					if len(codes) < 3 {
						codes = append(codes, fmt.Sprintf("0x%X", code))
					}
				}
			}
			phys, commit, _ := MemStatus()
			probe := "ok"
			if ph, _, err := spawn(`cmd /d /c exit 0`, true, desktop); err != nil {
				probe = err.Error()
			} else if code, done := exitCodeAfter(ph, 10*time.Second); !done || code != 0 {
				probe = fmt.Sprintf("new console process failed 0x%X", code)
				windows.CloseHandle(ph)
			} else {
				windows.CloseHandle(ph)
			}
			fmt.Printf("  %5d held: %d died (%s), RAM used +%d MB (%.1f MB each), commit +%d MB (%.1f MB each), start p50 %.0f ms max %.0f ms, new console process: %s\n",
				i, dead, strings.Join(codes, ","), phys0-phys, float64(phys0-phys)/float64(i), commit0-commit, float64(commit0-commit)/float64(i), percentile(spawnMs, .5), percentile(spawnMs, 1), probe)
			if dead > 0 || probe != "ok" {
				fmt.Println("  stopping: processes are failing")
				break
			}
			if phys < *stopFreeMB || commit < *stopFreeMB {
				fmt.Println("  stopping: memory safety margin reached")
				break
			}
		}
	}
	return 0
}

func init() {
	if len(os.Args) == 2 && os.Args[1] == "sleep" {
		syscall.WaitForSingleObject(syscall.Handle(windows.CurrentProcess()), syscall.INFINITE)
		os.Exit(0)
	}
}
