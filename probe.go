package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"runtime"
	"slices"
	"sync"
	"time"
	"unsafe"

	"github.com/Noisemaker111/fairshare/internal/windows"
)

// The probe stands in for a game in the foreground: every 16.7 ms (60 fps) four threads each do a
// fixed slice of CPU work, and the frame counts as late if it is not done before the next one is
// due. A fifth thread sleeps 1 ms at a time and records how late it wakes, which is how late a key
// press or mouse move would be handled. It runs at normal priority and is not the foreground
// window, so it gets no foreground boost: a real game is better off than this.

const frameBudget = time.Second / 60

type ProbeResult struct {
	Work      int     `json:"work"`
	Frames    int     `json:"frames"`
	Late      int     `json:"late"`
	LatePct   float64 `json:"late_pct"`
	FrameP50  float64 `json:"frame_p50_ms"`
	FrameP99  float64 `json:"frame_p99_ms"`
	FrameMax  float64 `json:"frame_max_ms"`
	WakeP50   float64 `json:"wake_p50_ms"`
	WakeP99   float64 `json:"wake_p99_ms"`
	WakeMax   float64 `json:"wake_max_ms"`
	DurationS float64 `json:"duration_s"`
}

var sink uint64

var (
	queryPerformanceCounter   = kernel32.NewProc("QueryPerformanceCounter")
	queryPerformanceFrequency = kernel32.NewProc("QueryPerformanceFrequency")
	qpcFreq                   = func() float64 {
		var f int64
		queryPerformanceFrequency.Call(uintptr(unsafe.Pointer(&f)))
		return float64(f)
	}()
)

// nowMs reads the high-resolution counter. Go's own clock on Windows moves in ~0.5 ms steps,
// too coarse to time a 2.5 ms slice of work or a 1 ms sleep.
func nowMs() float64 {
	var c int64
	queryPerformanceCounter.Call(uintptr(unsafe.Pointer(&c)))
	return float64(c) * 1000 / qpcFreq
}

func busy(iters int, buf []uint64) {
	x := uint64(88172645463325252)
	for i := range iters {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		buf[(x>>3)%uint64(len(buf))] += x + uint64(i)
	}
	sink += x
}

// calibrate finds the iteration count that takes target on an unloaded core (best of many tries,
// so other load on the PC only makes it more conservative).
func calibrate(target time.Duration) int {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	buf := make([]uint64, 8192)
	iters := 100000
	want := float64(target) / 1e6
	for range 4 {
		best := 1e9
		for range 15 {
			t := nowMs()
			busy(iters, buf)
			best = min(best, nowMs()-t)
		}
		iters = int(float64(iters) * want / max(best, 0.001))
	}
	return iters
}

func percentile(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	return s[min(len(s)-1, int(p*float64(len(s))))]
}

func summarize(frames, wakes []float64, work int, secs float64) ProbeResult {
	late := 0
	for _, f := range frames {
		if f > float64(frameBudget)/1e6 {
			late++
		}
	}
	return ProbeResult{
		Work: work, Frames: len(frames), Late: late, LatePct: 100 * float64(late) / float64(max(1, len(frames))),
		FrameP50: percentile(frames, .5), FrameP99: percentile(frames, .99), FrameMax: percentile(frames, 1),
		WakeP50: percentile(wakes, .5), WakeP99: percentile(wakes, .99), WakeMax: percentile(wakes, 1),
		DurationS: secs,
	}
}

func runProbe(dur time.Duration, work int) ProbeResult {
	if work <= 0 {
		work = calibrate(2500 * time.Microsecond)
	}
	frames, wakes, secs := probeRaw(dur, work)
	return summarize(frames, wakes, work, secs)
}

// probeRaw returns every frame time and every wake delay, in ms.
func probeRaw(dur time.Duration, work int) (frames, wakes []float64, secs float64) {
	const threads = 4
	if work <= 0 {
		work = calibrate(2500 * time.Microsecond)
	}
	start := make([]chan struct{}, threads)
	var done sync.WaitGroup
	for i := range threads {
		start[i] = make(chan struct{})
		go func(c chan struct{}) {
			runtime.LockOSThread()
			buf := make([]uint64, 8192)
			for range c {
				busy(work, buf)
				done.Done()
			}
		}(start[i])
	}
	stop := make(chan struct{})
	var wakeDone sync.WaitGroup
	wakeDone.Add(1)
	go func() {
		defer wakeDone.Done()
		runtime.LockOSThread()
		for {
			select {
			case <-stop:
				return
			default:
			}
			t := nowMs()
			time.Sleep(time.Millisecond)
			wakes = append(wakes, nowMs()-t-1)
		}
	}()
	budget := float64(frameBudget) / 1e6
	begin := nowMs()
	next := begin
	for nowMs()-begin < float64(dur)/1e6 {
		next += budget
		if d := next - nowMs(); d > 0 {
			time.Sleep(time.Duration(d * 1e6))
		}
		done.Add(threads)
		for _, c := range start {
			c <- struct{}{}
		}
		done.Wait()
		ft := nowMs() - next
		frames = append(frames, ft)
		if ft > budget {
			next = nowMs() // a late frame pushes the next one back, as in a game
		}
	}
	close(stop)
	wakeDone.Wait()
	for _, c := range start {
		close(c)
	}
	return frames, wakes, (nowMs() - begin) / 1000
}

func cmdProbe(args []string) int {
	fs := flag.NewFlagSet("probe", flag.ExitOnError)
	secs := fs.Float64("secs", 10, "how long to run")
	work := fs.Int("work", 0, "iterations per thread per frame (0 = calibrate to 2.5 ms now)")
	fs.Parse(args)
	r := runProbe(time.Duration(*secs*float64(time.Second)), *work)
	json.NewEncoder(os.Stdout).Encode(r)
	return 0
}

func (r ProbeResult) String() string {
	return fmt.Sprintf("late frames %.1f%% (p99 %.1f ms, max %.0f ms), input wake p99 %.2f ms max %.0f ms", r.LatePct, r.FrameP99, r.FrameMax, r.WakeP99, r.WakeMax)
}

var getSystemTimes = kernel32.NewProc("GetSystemTimes")

// cpuSampler reports whole-PC CPU use between calls.
type cpuSampler struct{ idle, total uint64 }

func (c *cpuSampler) sample() float64 {
	var idle, kern, user windows.Filetime
	getSystemTimes.Call(uintptr(unsafe.Pointer(&idle)), uintptr(unsafe.Pointer(&kern)), uintptr(unsafe.Pointer(&user)))
	i := uint64(idle.HighDateTime)<<32 | uint64(idle.LowDateTime)
	t := (uint64(kern.HighDateTime)<<32 | uint64(kern.LowDateTime)) + (uint64(user.HighDateTime)<<32 | uint64(user.LowDateTime))
	di, dt := i-c.idle, t-c.total
	c.idle, c.total = i, t
	if dt == 0 {
		return 0
	}
	return 100 * (1 - float64(di)/float64(dt))
}
