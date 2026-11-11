//go:build linux

package main

import (
	"encoding/json"
	"errors"
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

func runMeter() error {
	return errors.New("Linux needs no meter: the kernel keeps the averages, and `fairshare reading` reads them")
}

func install(uninstall bool) error {
	return errors.New("nothing to install on Linux: copy the fairshare file anywhere and run it")
}
