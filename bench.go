package main

import (
	_ "embed"
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
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

//go:embed burn.js
var burnJS []byte

// BenchResult compares the foreground probe in slices with the jobs running ("on") and frozen
// ("off"), in the order on, off, off, on, ... so that other load on the PC drifting up or down
// during the run hits both sides equally.
type BenchResult struct {
	When         string      `json:"when"`
	Mode         string      `json:"mode"`
	Jobs         int         `json:"jobs"`
	Units        int         `json:"units"`
	Threads      int         `json:"threads"`
	MemMB        int         `json:"mem_mb"`
	IOKB         int         `json:"io_kb"`
	SpawnEvery   int         `json:"spawn_every"`
	Files        int         `json:"files"`
	Kind         string      `json:"kind"`
	Baseline     ProbeResult `json:"baseline"`
	On           ProbeResult `json:"on"`
	Off          ProbeResult `json:"off"`
	OnOtherCPU   float64     `json:"on_other_cpu_pct"`
	OffOtherCPU  float64     `json:"off_other_cpu_pct"`
	OnMineCPU    float64     `json:"on_mine_cpu_pct"`
	WorkS        float64     `json:"work_s"`
	JobP50S      float64     `json:"job_p50_s"`
	JobMaxS      float64     `json:"job_max_s"`
	Failed       int         `json:"failed"`
	TimedOut     bool        `json:"timed_out"`
	UnitsPerSec  float64     `json:"units_per_s"`
	CPUAvgPct    float64     `json:"cpu_avg_pct"`
	MinFreeMB    uint64      `json:"min_free_ram_mb"`
	MinCommitMB  uint64      `json:"min_free_commit_mb"`
	FirstErr     string      `json:"first_error,omitempty"`
}

// killOnExitJob puts this process in a job that Windows kills, with every descendant, when this
// process ends, however it ends. Nothing a bench starts can outlive it.
func killOnExitJob() (windows.Handle, error) {
	job, err := killOnCloseJob()
	if err != nil {
		return 0, err
	}
	return job, windows.AssignProcessToJobObject(job, windows.CurrentProcess())
}

// killOnCloseJob makes an empty job whose processes Windows kills when its last handle closes.
func killOnCloseJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return 0, err
	}
	return job, nil
}

// treeCPU reads the CPU seconds used so far by each process in job except skip (the bench and
// its probe, which are counted as other load).
func treeCPU(job windows.Handle, skip uint32) map[uint32]float64 {
	pids := jobPIDs(job)
	return pidsCPU(slices.DeleteFunc(pids, func(p uint32) bool { return p == skip }))
}

func pidsCPU(pids []uint32) map[uint32]float64 {
	out := map[uint32]float64{}
	for _, pid := range pids {
		h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
		if err != nil {
			continue
		}
		var c, e, k, u windows.Filetime
		if windows.GetProcessTimes(h, &c, &e, &k, &u) == nil {
			out[pid] = float64(ftTicks(k)+ftTicks(u)) / 1e7
		}
		windows.CloseHandle(h)
	}
	return out
}

// ftTicks reads a FILETIME that holds a duration, in 100 ns ticks.
func ftTicks(f windows.Filetime) uint64 { return uint64(f.HighDateTime)<<32 | uint64(f.LowDateTime) }

func cpuDelta(before, after map[uint32]float64) float64 {
	d := 0.0
	for pid, t := range after {
		d += t - before[pid]
	}
	return d
}

func cmdBench(args []string) int {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	jobs := fs.Int("jobs", 10, "heavy jobs started at once")
	mode := fs.String("mode", "fair", "plain (as agents run today) or fair (through fairshare run); extra run flags go after --")
	units := fs.Int("units", 300, "work per job")
	threads := fs.Int("threads", 2, "threads per job")
	mem := fs.Int("mem", 100, "MB each job holds")
	io := fs.Int("io", 0, "KB each job writes and reads back per unit")
	spawnEvery := fs.Int("spawn-every", 0, "each job thread starts a node child process every this many units (0 = never)")
	files := fs.Int("files", 0, "small files each job thread reads per unit")
	kind := fs.String("kind", "js", "js (test-runner-like, lots of garbage) or math (optimizer-like dense math that never waits)")
	baseSecs := fs.Float64("baseline", 5, "seconds of probe before load")
	slice := fs.Float64("slice", 5, "seconds per on or off slice")
	pairs := fs.Int("pairs", 3, "on/off slice pairs")
	deadline := fs.Duration("deadline", 8*time.Minute, "kill everything after this")
	out := fs.String("out", "", "append the JSON result to this file")
	fs.Parse(args)
	runFlags := fs.Args()

	exe, _ := os.Executable()
	tmp, err := os.MkdirTemp("", "fairshare-bench-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(tmp)
	burn := filepath.Join(tmp, "burn.js")
	os.WriteFile(burn, burnJS, 0o644)
	job, err := killOnExitJob()
	if err != nil {
		fmt.Fprintln(os.Stderr, "bench job:", err)
		return 1
	}
	self := windows.GetCurrentProcessId()

	res := BenchResult{When: time.Now().Format(time.RFC3339), Mode: strings.TrimSpace(*mode + " " + strings.Join(runFlags, " ")), Jobs: *jobs, Units: *units, Threads: *threads, MemMB: *mem, IOKB: *io, SpawnEvery: *spawnEvery, Files: *files, Kind: *kind}
	work := calibrate(2500 * time.Microsecond)
	res.Baseline = runProbe(time.Duration(*baseSecs*float64(time.Second)), work)
	fmt.Fprintf(os.Stderr, "before:   %s\n", res.Baseline)

	burnArgs := []string{burn, strconv.Itoa(*units), strconv.Itoa(*threads), strconv.Itoa(*mem), strconv.Itoa(*io), tmp, strconv.Itoa(*spawnEvery), strconv.Itoa(*files), *kind}
	if *files > 0 { // build the shared file tree once, before the jobs race to read it
		if out, err := exec.Command("node", burn, "1", "1", "0", "0", tmp, "0", "1").CombinedOutput(); err != nil {
			fmt.Fprintln(os.Stderr, "make file tree:", err, string(out))
			return 1
		}
	}
	var mu sync.Mutex
	var durs []float64
	var wg sync.WaitGroup
	t0 := time.Now()
	for i := range *jobs {
		var cmd *exec.Cmd
		switch *mode {
		case "plain":
			cmd = exec.Command("node", burnArgs...)
		case "fair":
			cmd = exec.Command(exe, append(append(append([]string{"run"}, runFlags...), "--", "node"), burnArgs...)...)
		default:
			fmt.Fprintln(os.Stderr, "unknown mode", *mode)
			return 2
		}
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			res.Failed++
			res.FirstErr = err.Error()
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := cmd.Wait()
			mu.Lock()
			defer mu.Unlock()
			durs = append(durs, time.Since(t0).Seconds())
			if err != nil {
				res.Failed++
				if res.FirstErr == "" {
					res.FirstErr = fmt.Sprintf("job %d: %v %s", i, err, strings.TrimSpace(stderr.String()))
				}
			}
		}(i)
	}

	stopSampling := make(chan struct{})
	var cpuSum float64
	var cpuN int
	res.MinFreeMB, res.MinCommitMB = 1<<62, 1<<62
	var sampWG sync.WaitGroup
	sampWG.Add(1)
	go func() {
		defer sampWG.Done()
		var c cpuSampler
		c.sample()
		for {
			select {
			case <-stopSampling:
				return
			case <-time.After(time.Second):
			}
			mu.Lock()
			cpuSum += c.sample()
			cpuN++
			phys, commit, _ := MemStatus()
			res.MinFreeMB, res.MinCommitMB = min(res.MinFreeMB, phys), min(res.MinCommitMB, commit)
			mu.Unlock()
		}
	}()

	time.Sleep(3 * time.Second) // let every job start
	var onF, onW, offF, offW []float64
	var onOther, offOther, onMine float64
	var offTime time.Duration
	ncpu := float64(runtime.NumCPU())
	frozen := false
	for i := range *pairs * 2 {
		on := i%4 == 0 || i%4 == 3
		if on == frozen { // suspending twice needs two resumes, so only ever change state
			freezeJob(job, !on, self)
			frozen = !on
		}
		var c cpuSampler
		c.sample()
		mine0 := treeCPU(job, self)
		s := time.Now()
		f, w, secs := probeRaw(time.Duration(*slice*float64(time.Second)), work)
		total := c.sample()
		after := treeCPU(job, self)
		mine := 100 * cpuDelta(mine0, after) / secs / ncpu
		if os.Getenv("FAIRSHARE_DEBUG") != "" {
			fmt.Fprintf(os.Stderr, "  slice %d on=%v: %d processes in job, %d read, %.2f CPU s over %.1f s\n", i, on, len(jobPIDs(job)), len(after), cpuDelta(mine0, after), secs)
		}
		if on {
			onF, onW = append(onF, f...), append(onW, w...)
			onOther += (total - mine) / float64(*pairs)
			onMine += mine / float64(*pairs)
		} else {
			offF, offW = append(offF, f...), append(offW, w...)
			offOther += (total - mine) / float64(*pairs)
			offTime += time.Since(s)
		}
	}
	if frozen {
		freezeJob(job, false, self)
	}
	res.On = summarize(onF, onW, work, float64(*pairs)**slice)
	res.Off = summarize(offF, offW, work, float64(*pairs)**slice)
	res.OnOtherCPU, res.OffOtherCPU, res.OnMineCPU = onOther, offOther, onMine
	fmt.Fprintf(os.Stderr, "jobs off: %s (other load %.0f%% CPU)\n", res.Off, res.OffOtherCPU)
	fmt.Fprintf(os.Stderr, "jobs on:  %s (other load %.0f%% CPU, these jobs %.0f%%)\n", res.On, res.OnOtherCPU, res.OnMineCPU)

	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(*deadline - time.Since(t0)):
		res.TimedOut = true
		for _, pid := range jobPIDs(job) { // everything but the bench itself, so it can still report
			if pid == self {
				continue
			}
			if h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, pid); err == nil {
				windows.TerminateProcess(h, 1)
				windows.CloseHandle(h)
			}
		}
		<-finished
	}
	res.WorkS = (time.Since(t0) - offTime).Seconds()
	close(stopSampling)
	sampWG.Wait()
	mu.Lock()
	slices.Sort(durs)
	if len(durs) > 0 {
		res.JobP50S, res.JobMaxS = durs[len(durs)/2]-offTime.Seconds(), durs[len(durs)-1]-offTime.Seconds()
	}
	res.CPUAvgPct = cpuSum / float64(max(1, cpuN))
	res.UnitsPerSec = float64(*units*(len(durs)-res.Failed)) / res.WorkS
	mu.Unlock()

	fmt.Fprintf(os.Stderr, "%d jobs (%s): all done in %.1fs of running time, %.0f units/s, %d failed, CPU avg %.0f%%, min free RAM %d MB\n", res.Jobs, res.Mode, res.WorkS, res.UnitsPerSec, res.Failed, res.CPUAvgPct, res.MinFreeMB)
	if res.FirstErr != "" {
		fmt.Fprintln(os.Stderr, "first error:", res.FirstErr)
	}
	b, _ := json.Marshal(res)
	fmt.Println(string(b))
	if *out != "" {
		f, err := os.OpenFile(*out, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err == nil {
			f.Write(append(b, '\n'))
			f.Close()
		}
	}
	return 0
}
