package main

// Reading Linux's own numbers. The kernel keeps the averages a Windows PC needs a meter for: the
// load average is the tasks that ran or waited for a processor over 1, 5 and 15 minutes, and
// MemAvailable is the memory a new program could take without swapping. A container sees its
// host's /proc, so its own limits come from the cgroup files when they are there. These parsers
// take the files' text, so they are tested on any machine.

import (
	"fmt"
	"strconv"
	"strings"
)

func parseLoadavg(text string) (one, five float64, err error) {
	f := strings.Fields(text)
	if len(f) < 2 {
		return 0, 0, fmt.Errorf("loadavg has %d fields", len(f))
	}
	if one, err = strconv.ParseFloat(f[0], 64); err != nil {
		return 0, 0, err
	}
	five, err = strconv.ParseFloat(f[1], 64)
	return one, five, err
}

// parseMeminfo returns MemTotal and MemAvailable in bytes.
func parseMeminfo(text string) (total, avail float64, err error) {
	found := 0
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		kb, perr := strconv.ParseFloat(f[1], 64)
		if perr != nil {
			continue
		}
		switch f[0] {
		case "MemTotal:":
			total, found = kb*1024, found+1
		case "MemAvailable:":
			avail, found = kb*1024, found+1
		}
	}
	if found < 2 {
		return 0, 0, fmt.Errorf("meminfo has no MemTotal and MemAvailable")
	}
	return total, avail, nil
}

// parseCgroupMem returns a cgroup's memory limit and what counts against it, in bytes: memory.max,
// and memory.current less the file cache the kernel gives back under pressure. ok is false when
// the cgroup has no limit.
func parseCgroupMem(max, current, stat string) (limit, used float64, ok bool) {
	limit, err := strconv.ParseFloat(strings.TrimSpace(max), 64)
	if err != nil || limit <= 0 {
		return 0, 0, false
	}
	used, err = strconv.ParseFloat(strings.TrimSpace(current), 64)
	if err != nil {
		return 0, 0, false
	}
	for _, line := range strings.Split(stat, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && (f[0] == "inactive_file" || f[0] == "active_file") {
			if v, err := strconv.ParseFloat(f[1], 64); err == nil {
				used -= v
			}
		}
	}
	if used < 0 {
		used = 0
	}
	return limit, used, true
}

// parseCgroupCPU returns the processors a cgroup's cpu.max allows ("200000 100000" is 2); ok is
// false for "max", which is no limit.
func parseCgroupCPU(cpuMax string) (cores float64, ok bool) {
	f := strings.Fields(cpuMax)
	if len(f) != 2 || f[0] == "max" {
		return 0, false
	}
	quota, err1 := strconv.ParseFloat(f[0], 64)
	period, err2 := strconv.ParseFloat(f[1], 64)
	if err1 != nil || err2 != nil || period <= 0 || quota <= 0 {
		return 0, false
	}
	return quota / period, true
}

// parsePressure returns the "some avg60" share of a /proc/pressure file: the percent of the last
// minute in which at least one task was held up waiting for that resource.
func parsePressure(text string) (some60 float64, ok bool) {
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || f[0] != "some" {
			continue
		}
		for _, kv := range f[1:] {
			if v, found := strings.CutPrefix(kv, "avg60="); found {
				p, err := strconv.ParseFloat(v, 64)
				return p, err == nil
			}
		}
	}
	return 0, false
}

// linuxFiles is the text of the files a Linux reading is made from; a missing file is "".
type linuxFiles struct {
	Loadavg, Meminfo               string
	CgroupMemMax, CgroupMemCurrent string
	CgroupMemStat, CgroupCPUMax    string
	PressureCPU, PressureMem       string
}

// linuxReading builds a reading from those files. processors is the logical processors the
// process may run on.
func linuxReading(f linuxFiles, processors int) (Reading, error) {
	r := Reading{OS: "linux", Processors: processors, Capacity: float64(processors), ShortSeconds: 60, LongSeconds: 300,
		Presence: Presence{State: "none"}}
	if cores, ok := parseCgroupCPU(f.CgroupCPUMax); ok && cores < r.Capacity {
		r.Capacity = cores
	}
	one, five, err := parseLoadavg(f.Loadavg)
	if err != nil {
		return r, fmt.Errorf("/proc/loadavg: %w", err)
	}
	r.DemandShort, r.DemandLong = one, five
	r.BusyShort, r.WaitingShort = split(one, r.Capacity)
	r.BusyLong, r.WaitingLong = split(five, r.Capacity)
	r.WorkCores = r.BusyShort
	total, avail, err := parseMeminfo(f.Meminfo)
	if err != nil {
		return r, fmt.Errorf("/proc/meminfo: %w", err)
	}
	if limit, used, ok := parseCgroupMem(f.CgroupMemMax, f.CgroupMemCurrent, f.CgroupMemStat); ok && limit < total {
		total = limit
		if free := limit - used; free < avail {
			avail = free
		}
	}
	const gb = 1 << 30
	r.MemTotalGB, r.MemAvailGB = round2(total/gb), round2(avail/gb)
	r.WorkMemGB = round2((total - avail) / gb)
	if p, ok := parsePressure(f.PressureCPU); ok && p >= 1 {
		r.Notes = append(r.Notes, fmt.Sprintf("tasks waited for a processor in %.0f%% of the last minute", p))
	}
	if p, ok := parsePressure(f.PressureMem); ok && p >= 1 {
		r.Notes = append(r.Notes, fmt.Sprintf("tasks waited for memory in %.0f%% of the last minute", p))
	}
	return r, nil
}

// split divides a load average into the part that ran and the part that waited.
func split(load, capacity float64) (busy, waiting float64) {
	if load > capacity {
		return capacity, load - capacity
	}
	return load, 0
}
