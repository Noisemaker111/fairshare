//go:build linux

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

func slurp(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// cgroupDir is this process's cgroup v2 folder, or "" when there is none.
func cgroupDir() string {
	for _, line := range strings.Split(slurp("/proc/self/cgroup"), "\n") {
		if rest, ok := strings.CutPrefix(line, "0::"); ok {
			return "/sys/fs/cgroup" + strings.TrimSpace(rest)
		}
	}
	return ""
}

// cpuMark is the processor time counted up to one moment.
type cpuMark struct {
	At    time.Time `json:"at"`
	Busy  float64   `json:"busy"`
	Total float64   `json:"total"`
}

// measureUse returns the processors in use over the last minute or so and over the last ten,
// from /proc/stat now against the marks earlier runs left in cpu.json. With no mark of the right
// age (the first run in a while) it watches for one second instead.
func measureUse(processors int) *cpuUse {
	busy, total, ok := parseProcStat(slurp("/proc/stat"))
	if !ok {
		return nil
	}
	now := time.Now()
	file := filepath.Join(homeDir(), "cpu.json")
	var marks, keep []cpuMark
	if b, err := os.ReadFile(file); err == nil {
		json.Unmarshal(b, &marks)
	}
	since := func(m cpuMark, b, t float64) (float64, bool) {
		if t <= m.Total || b < m.Busy {
			return 0, false // the machine restarted since the mark
		}
		return (b - m.Busy) / (t - m.Total) * float64(processors), true
	}
	u := cpuUse{}
	for _, m := range marks { // oldest first
		age := now.Sub(m.At)
		if age > 10*time.Minute || age < 0 {
			continue
		}
		keep = append(keep, m)
		v, ok := since(m, busy, total)
		if !ok || age < 20*time.Second {
			continue
		}
		if u.LongSeconds == 0 {
			u.Long, u.LongSeconds = v, age.Seconds()
		}
		if age <= 90*time.Second {
			u.Short, u.ShortSeconds = v, age.Seconds()
		}
	}
	if u.ShortSeconds == 0 {
		time.Sleep(time.Second)
		b2, t2, ok := parseProcStat(slurp("/proc/stat"))
		v, fine := since(cpuMark{Busy: busy, Total: total}, b2, t2)
		if !ok || !fine {
			return nil
		}
		u.Short, u.ShortSeconds = v, 1
	}
	if u.LongSeconds == 0 {
		u.Long, u.LongSeconds = u.Short, u.ShortSeconds
	}
	if len(keep) == 0 || now.Sub(keep[len(keep)-1].At) >= 15*time.Second {
		keep = append(keep, cpuMark{now, busy, total})
		if b, err := json.Marshal(keep); err == nil && os.MkdirAll(homeDir(), 0o755) == nil {
			tmp := file + ".tmp"
			if os.WriteFile(tmp, b, 0o644) == nil {
				os.Rename(tmp, file)
			}
		}
	}
	return &u
}

// currentReading measures this machine now: processors in use from the kernel's counters, the
// rest from its own averages.
func currentReading(s Settings) (Reading, error) {
	// A reading written by hand, for the checks that need "full" and "room" to be certain
	// whatever this machine is doing. It is held to the meter's rule: never an old one.
	if file := os.Getenv("FAIRSHARE_READING"); file != "" {
		r, err := readingFrom(file)
		if err == nil && time.Since(r.At) > time.Duration(max(3*s.SampleSeconds, 20)*float64(time.Second)) {
			err = fmt.Errorf("the reading in %s is %.0f s old", file, time.Since(r.At).Seconds())
		}
		if err == nil && len(r.Work) == 0 {
			r.Work = liveFromMarks(readMarks(fairshareAlive))
		}
		return r, err
	}
	f := linuxFiles{
		Use:         measureUse(runtime.NumCPU()),
		Loadavg:     slurp("/proc/loadavg"),
		Meminfo:     slurp("/proc/meminfo"),
		PressureCPU: slurp("/proc/pressure/cpu"),
		PressureMem: slurp("/proc/pressure/memory"),
	}
	// The nearest cgroup with a memory limit is the one that ends a process that passes it.
	for dir := cgroupDir(); strings.HasPrefix(dir, "/sys/fs/cgroup"); dir = dir[:strings.LastIndex(dir, "/")] {
		if max := strings.TrimSpace(slurp(dir + "/memory.max")); max != "" && max != "max" && f.CgroupMemMax == "" {
			f.CgroupMemMax, f.CgroupMemCurrent, f.CgroupMemStat = max, slurp(dir+"/memory.current"), slurp(dir+"/memory.stat")
		}
		if cpu := strings.TrimSpace(slurp(dir + "/cpu.max")); cpu != "" && !strings.HasPrefix(cpu, "max") && f.CgroupCPUMax == "" {
			f.CgroupCPUMax = cpu
		}
		if dir == "/sys/fs/cgroup" {
			break
		}
	}
	r, err := linuxReading(f, runtime.NumCPU())
	if err != nil {
		return r, err
	}
	// Which work uses what is the meter's to say. When it is not running the reading still stands,
	// from the kernel's counters, and the open work counts only as far as it shows in them.
	if work, ok := readWork(s); ok {
		r.Work = work
	} else {
		r.Work = liveFromMarks(readMarks(fairshareAlive))
		if err := startMeter(); err != nil {
			r.Notes = append(r.Notes, "the work open here is not measured piece by piece: "+err.Error())
		} else {
			r.Notes = append(r.Notes, "the work open here was not being measured piece by piece; the meter that does it has been started")
		}
	}
	r.Machine, _ = os.Hostname()
	r.At = time.Now().UTC()
	r.DiskPath = s.DiskPath
	if r.DiskPath == "" {
		r.DiskPath, _ = os.UserHomeDir()
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(r.DiskPath, &st); err != nil {
		r.Notes = append(r.Notes, "disk not read: "+err.Error())
	} else {
		r.DiskFreeGB = round2(float64(st.Bavail) * float64(st.Bsize) / (1 << 30))
	}
	return r, nil
}

func hideWindow(*exec.Cmd) {}

// ownGroup starts a command in a process group of its own, so killTree ends everything it started.
func ownGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killTree ends a command and everything it started.
func killTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) != nil {
		cmd.Process.Kill()
	}
}

// listProcTimes is every process with the processor time it has used (pool.go, watch).
func listProcTimes() []procTime {
	var out []procTime
	for _, p := range snapshot() {
		out = append(out, procTime{pid: p.pid, parent: p.parent, group: p.group, seconds: p.ticks / ticksPerSecond})
	}
	return out
}

// tellYield has nobody to tell on Linux: no keeper holds agent work there.
func tellYield(Settings, time.Time) error {
	return errors.New("nothing holds agent work back on Linux")
}

// The Linux meter. The kernel keeps what a reading needs, so a Linux machine answered without a
// meter; but nothing kept which work uses what, and a host is where most threads run. This one
// looks at every process every sample_seconds, puts each under its work (work.go), writes the
// open work to work.json and what it learns to learned.json. The first question a machine is
// asked starts it, so there is still nothing to install, and it ends by itself when the fairshare
// file it was started from is replaced, so a new build takes over at the next question.

const ticksPerSecond = 100 // what /proc counts processor time in, on every Linux

func workFile() string { return filepath.Join(homeDir(), "work.json") }

type workNow struct {
	At   time.Time  `json:"at"`
	Work []LiveWork `json:"work"`
}

// readWork is the meter's open work, when the meter wrote it within the last few looks.
func readWork(s Settings) ([]LiveWork, bool) {
	var w workNow
	b, err := os.ReadFile(workFile())
	if err != nil || json.Unmarshal(b, &w) != nil {
		return nil, false
	}
	if time.Since(w.At) > time.Duration(max(3*s.SampleSeconds, 20)*float64(time.Second)) {
		return nil, false
	}
	return w.Work, true
}

// fairshareAlive says whether a process id is a living fairshare: a run's mark outlives a run
// that was killed.
func fairshareAlive(pid uint32) bool {
	p, ok := parsePidStat(slurp(fmt.Sprintf("/proc/%d/stat", pid)))
	return ok && strings.HasPrefix(p.name, "fairshare")
}

// startMeter starts the meter for this home, apart from the command that asked. Never from a
// test binary, and not when FAIRSHARE_METER=off.
func startMeter() error {
	if os.Getenv("FAIRSHARE_METER") == "off" {
		return errors.New("FAIRSHARE_METER=off")
	}
	exe := self()
	if base := filepath.Base(exe); !strings.HasPrefix(base, "fairshare") || strings.HasSuffix(base, ".test") {
		return errors.New("this program is not fairshare, so it starts no meter")
	}
	if err := os.MkdirAll(homeDir(), 0o755); err != nil {
		return err
	}
	logPath := filepath.Join(homeDir(), "meter.log")
	if st, err := os.Stat(logPath); err == nil && st.Size() > 1<<20 {
		os.Remove(logPath)
	}
	log, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer log.Close()
	cmd := exec.Command(exe, "meter")
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// whereLinux says the folder a process runs in and the T3 thread it was started for.
func whereLinux(pid uint32, _ uint64) (dir, thread string) {
	dir, _ = os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
	for _, kv := range strings.Split(slurp(fmt.Sprintf("/proc/%d/environ", pid)), "\x00") {
		if v, ok := strings.CutPrefix(kv, "T3CODE_THREAD_ID="); ok {
			thread = v
			break
		}
	}
	return dir, thread
}

// snapshot reads every process once.
func snapshot() []pidStat {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	out := make([]pidStat, 0, len(entries))
	for _, e := range entries {
		if name := e.Name(); name[0] < '0' || name[0] > '9' {
			continue
		}
		if p, ok := parsePidStat(slurp("/proc/" + e.Name() + "/stat")); ok {
			out = append(out, p)
		}
	}
	return out
}

func stopFile() string { return filepath.Join(homeDir(), "meter.stop") }

func runMeter() error {
	if err := os.MkdirAll(homeDir(), 0o755); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(homeDir(), "meter.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer lock.Close()
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return nil // the meter for this home already runs
	}
	os.Remove(stopFile())
	exe := self()
	built, _ := os.Stat(exe)
	boot, _ := parseBootTime(slurp("/proc/stat"))
	page := float64(os.Getpagesize())
	var t tracker
	before, carry := map[procKey]procSeen{}, map[procKey]float64{}
	var prevAt time.Time
	known := loadLearned()
	for {
		s, err := loadSettings()
		if err != nil {
			fmt.Fprintln(os.Stderr, "degraded:", err)
		}
		now := time.Now()
		procs := snapshot()
		seen := make(map[procKey]procSeen, len(procs))
		byPID := make(map[uint32]procKey, len(procs))
		names := make(map[uint32]string, len(procs))
		for _, p := range procs {
			byPID[p.pid], names[p.pid] = procKey{p.pid, p.start}, p.name
		}
		for _, p := range procs {
			seen[procKey{p.pid, p.start}] = procSeen{seconds: p.ticks / ticksPerSecond, parent: byPID[p.parent]}
		}
		used := usedSince(seen, before, carry, prevAt.IsZero())
		if seconds := now.Sub(prevAt).Seconds(); !prevAt.IsZero() && seconds > 0 {
			nodes := make([]procNode, 0, len(procs))
			for _, p := range procs {
				n := procNode{pid: p.pid, parent: p.parent, created: p.start, name: p.name, cores: used[procKey{p.pid, p.start}], memGB: p.pages * page / (1 << 30), work: true,
					total: p.ticks / ticksPerSecond}
				if boot > 0 {
					n.started = time.Unix(0, int64((boot+float64(p.start)/ticksPerSecond)*1e9))
				}
				nodes = append(nodes, n)
			}
			alive := func(pid uint32) bool { return strings.HasPrefix(names[pid], "fairshare") }
			if learnt := t.look(now, seconds, nodes, readMarks(alive), whereLinux, s); len(learnt) > 0 {
				known = learn(learnt, now, s)
			}
			if b, err := json.Marshal(workNow{At: now.UTC(), Work: t.live(now, known, s)}); err == nil {
				tmp := workFile() + ".tmp"
				if os.WriteFile(tmp, b, 0o644) == nil {
					os.Rename(tmp, workFile())
				}
			}
		}
		before, prevAt = seen, now
		for wait := time.Duration(s.SampleSeconds * float64(time.Second)); wait > 0; wait -= 250 * time.Millisecond {
			if _, err := os.Stat(stopFile()); err == nil {
				os.Remove(stopFile())
				os.Remove(workFile())
				return nil
			}
			time.Sleep(min(wait, 250*time.Millisecond))
		}
		// A new build was copied over the file this one was started from: the next question
		// starts that one.
		if st, err := os.Stat(exe); built != nil && (err != nil || !st.ModTime().Equal(built.ModTime()) || st.Size() != built.Size()) {
			os.Remove(workFile())
			return nil
		}
	}
}

func install(uninstall bool) error {
	if uninstall {
		// Stop the meter; the next question would start it again unless FAIRSHARE_METER=off.
		return os.WriteFile(stopFile(), nil, 0o644)
	}
	return errors.New("nothing to install on Linux: copy the fairshare file anywhere and run it; its first question starts its meter")
}
