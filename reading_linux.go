//go:build linux

package main

import (
	"errors"
	"os"
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

// currentReading measures this machine now, from the kernel's own averages.
func currentReading(s Settings) (Reading, error) {
	f := linuxFiles{
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

func runMeter() error {
	return errors.New("Linux needs no meter: the kernel keeps the averages, and `fairshare reading` reads them")
}

func install(uninstall bool) error {
	return errors.New("nothing to install on Linux: copy the fairshare file anywhere and run it")
}
