package main

// `fairshare bench lanes` measures whether per-chat lanes share the CPU by chat rather than by
// thread: a heavy lane (many busy threads) and a light lane (a few) run at once, each in its own
// job, under each way of holding the heavy lane back, in an order that runs back the other way.
// `fairshare spin <threads> <seconds>` is the busy work it uses.

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"github.com/Noisemaker111/fairshare/internal/windows"
)

func cmdSpin(args []string) int {
	threads, secs := 1, 1.0
	if len(args) > 0 {
		threads, _ = strconv.Atoi(args[0])
	}
	if len(args) > 1 {
		secs, _ = strconv.ParseFloat(args[1], 64)
	}
	runtime.GOMAXPROCS(threads)
	stop := time.Now().Add(time.Duration(secs * float64(time.Second)))
	done := make(chan struct{})
	for range threads {
		go func() {
			x := uint64(1)
			for time.Now().Before(stop) {
				for i := 0; i < 1<<16; i++ {
					x = x*6364136223846793005 + 1442695040888963407
				}
			}
			if x == 42 {
				fmt.Print("")
			}
			done <- struct{}{}
		}()
	}
	for range threads {
		<-done
	}
	return 0
}

// laneRun starts one busy lane in its own kill-on-close job at idle priority, as adopt runs agent
// work, and returns the job. mode is how the heavy lane is held back: plain (not at all),
// weighted (equal CPU weight per lane), below (the light lane runs at below-normal instead) or
// threads (the heavy lane's threads are dropped to the lowest priority, see demoteThreads).
func laneRun(threads int, secs float64, mode string, heavy bool) (windows.Handle, *exec.Cmd, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, nil, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE | windows.JOB_OBJECT_LIMIT_PRIORITY_CLASS
	info.BasicLimitInformation.PriorityClass = windows.IDLE_PRIORITY_CLASS
	if mode == "below" && !heavy {
		info.BasicLimitInformation.PriorityClass = windows.BELOW_NORMAL_PRIORITY_CLASS
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return 0, nil, err
	}
	if mode == "weighted" {
		err = setCPUWeight(job, 5)
	}
	if err != nil {
		return 0, nil, fmt.Errorf("%s: %w", mode, err)
	}
	exe, _ := os.Executable()
	cmd := exec.Command(exe, "spin", strconv.Itoa(threads), strconv.FormatFloat(secs, 'f', 1, 64))
	if err := cmd.Start(); err != nil {
		return 0, nil, err
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err == nil {
		err = windows.AssignProcessToJobObject(job, h)
		windows.CloseHandle(h)
	}
	return job, cmd, err
}

var openJobObject = kernel32.NewProc("OpenJobObjectW")

const (
	jobObjectQuery         = 0x4
	jobObjectSetAttributes = 0x10
)

// openLane opens the lane adopt keeps for the chat whose harness is pid, or returns 0.
func openLane(pid uint32) windows.Handle {
	name, _ := windows.UTF16PtrFromString(laneName(pid, creationTime(pid)))
	h, _, _ := openJobObject.Call(jobObjectQuery|jobObjectSetAttributes, 0, uintptr(unsafe.Pointer(name)))
	return windows.Handle(h)
}

// cmdThrottle caps one chat's CPU, or lists the chats adopt holds. adopt owns every job setting:
// this leaves a file adopt reads twice a second and applies across all of the chat's jobs.
func cmdThrottle(args []string) int {
	procs := snapshot()
	if len(args) == 0 {
		var st struct {
			At    string       `json:"at"`
			Lanes []laneStatus `json:"lanes"`
		}
		b, err := os.ReadFile(statusFile())
		if err != nil || json.Unmarshal(b, &st) != nil {
			fmt.Println("no chats: fairshare adopt is not running")
			return 0
		}
		at, _ := time.Parse(time.RFC3339, st.At)
		if time.Since(at) > 30*time.Second {
			fmt.Printf("no chats: fairshare adopt last wrote its status %s ago, so it is not running\n", time.Since(at).Round(time.Second))
			return 0
		}
		fmt.Println("chats adopt holds (fairshare throttle <pid> <percent of the PC|off>):")
		slices.SortFunc(st.Lanes, func(a, b laneStatus) int { return int(100 * (b.Cores - a.Cores)) })
		for _, l := range st.Lanes {
			state := ""
			if l.Held {
				state += "  held back"
			}
			if l.CapPct > 0 {
				state += fmt.Sprintf("  capped at %.0f%%", l.CapPct)
			}
			fmt.Printf("  %6d %-12s %5.1f cores now  %8.1f s CPU over %5.1f min  %4d processes in %d jobs (%d outside)  peak %5.0f MB%s\n",
				l.PID, l.Harness, l.Cores, l.CPUS, l.AliveMin, l.Procs, l.Jobs, l.Outside, l.PeakMB, state)
		}
		return 0
	}
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: fairshare throttle [<pid> <percent of the whole PC|off>]  (pid: the chat's harness or anything under it)")
		return 2
	}
	pid64, err := strconv.ParseUint(args[0], 10, 32)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fairshare throttle: not a process ID:", args[0])
		return 2
	}
	pct := 0.0
	if args[1] != "off" {
		if pct, err = strconv.ParseFloat(strings.TrimSuffix(args[1], "%"), 64); err != nil || pct <= 0 || pct > 100 {
			fmt.Fprintln(os.Stderr, "fairshare throttle: percent must be 1-100 or off:", args[1])
			return 2
		}
	}
	var job windows.Handle
	chat := uint32(pid64)
	for depth := 0; depth < 64 && job == 0; depth++ {
		if job = openLane(chat); job != 0 {
			break
		}
		next, ok := procs[chat]
		if !ok || next.ppid == chat {
			break
		}
		chat = next.ppid
	}
	if job == 0 {
		fmt.Fprintf(os.Stderr, "fairshare throttle: %d is not in any chat's lane (is fairshare adopt running?)\n", pid64)
		return 1
	}
	windows.CloseHandle(job)
	file := filepath.Join(throttleDir(), fmt.Sprintf("%d-%d", chat, creationTime(chat)))
	if pct == 0 {
		os.Remove(file)
	} else {
		os.MkdirAll(throttleDir(), 0o755)
		if err := os.WriteFile(file, []byte(strconv.FormatFloat(pct, 'f', -1, 64)), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "fairshare throttle:", err)
			return 1
		}
	}
	logEvent("throttle", map[string]any{"pid": chat, "harness": procs[chat].name, "pct": pct})
	if pct == 0 {
		fmt.Printf("chat %d (%s): cap removed\n", chat, procs[chat].name)
	} else {
		fmt.Printf("chat %d (%s): capped at %.0f%% of the PC, within a second\n", chat, procs[chat].name, pct)
	}
	return 0
}

func benchLanes(args []string) int {
	fs := flag.NewFlagSet("bench lanes", flag.ExitOnError)
	heavy := fs.Int("heavy", 2*runtime.NumCPU(), "busy threads in the heavy lane")
	light := fs.Int("light", 2, "busy threads in the light lane")
	secs := fs.Float64("secs", 6, "seconds per slice")
	fs.Parse(args)
	if _, err := killOnExitJob(); err != nil {
		fmt.Fprintln(os.Stderr, "fairshare bench lanes:", err)
		return 1
	}
	fmt.Printf("%d logical CPUs; heavy lane %d threads, light lane %d threads, %.0f s slices\n", runtime.NumCPU(), *heavy, *light, *secs)
	for _, mode := range []string{"plain", "weighted", "below", "threads", "threads", "below", "weighted", "plain"} {
		hj, hc, err := laneRun(*heavy, *secs, mode, true)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		lj, lc, err := laneRun(*light, *secs, mode, false)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		done := make(chan struct{})
		go func() { hc.Wait(); lc.Wait(); close(done) }()
		lowered := map[uint32]bool{}
		for waiting := true; waiting; {
			if mode == "threads" {
				demoteThreads(map[uint32]bool{uint32(hc.Process.Pid): true}, lowered)
			}
			select {
			case <-done:
				waiting = false
			case <-time.After(250 * time.Millisecond):
			}
		}
		h, l := jobStats(hj).CPUSeconds, jobStats(lj).CPUSeconds
		windows.CloseHandle(hj)
		windows.CloseHandle(lj)
		fmt.Printf("%-8s heavy %5.1f cores  light %4.2f of %d cores it wanted (%3.0f%%)\n", mode, h / *secs, l / *secs, *light, 100*l / *secs/float64(*light))
	}
	return 0
}
