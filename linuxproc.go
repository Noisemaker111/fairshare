package main

// Reading Linux's own numbers. The kernel keeps the averages a Windows PC needs a meter for: the
// load average is the tasks that ran or waited for a processor over 1, 5 and 15 minutes, and
// MemAvailable is the memory a new program could take without swapping. A container sees its
// host's /proc, so its own limits come from the cgroup files when they are there. These parsers
// take the files' text, so they are tested on any machine. The same goes for the per-process
// files the Linux meter reads to see which work uses what.

import (
	"fmt"
	"math"
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
	// Use is the processors in use over the short and the long window, measured from
	// /proc/stat (cpuUse); nil when it could not be.
	Use *cpuUse
}

// cpuUse is processors in use over two windows, with each window's length in seconds.
type cpuUse struct {
	Short, Long               float64
	ShortSeconds, LongSeconds float64
}

// parseProcStat returns the busy and the total processor time, in ticks since the machine
// started, from the first line of /proc/stat. Idle and waiting-for-disk time are not busy.
func parseProcStat(text string) (busy, total float64, ok bool) {
	line, _, _ := strings.Cut(text, "\n")
	f := strings.Fields(line)
	if len(f) < 5 || f[0] != "cpu" {
		return 0, 0, false
	}
	for i, v := range f[1:] {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return 0, 0, false
		}
		if i >= 8 { // guest time is already inside user time
			break
		}
		total += n
		if i != 3 && i != 4 { // idle, iowait
			busy += n
		}
	}
	return busy, total, total > 0
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
	// The load average also counts tasks waiting on a disk or the network, which use no
	// processor: a host read as "77 wanted of 8". Room is judged by processors in use; the load
	// over the processors says how many tasks waited, and only while the processors were full.
	if u := f.Use; u != nil {
		r.BusyShort, r.BusyLong = math.Min(u.Short, r.Capacity), math.Min(u.Long, r.Capacity)
		r.ShortSeconds, r.LongSeconds = u.ShortSeconds, u.LongSeconds
	} else {
		r.BusyShort, r.BusyLong = math.Min(one, r.Capacity), math.Min(five, r.Capacity)
		r.Notes = append(r.Notes, "processor use not read; judged by the load average, which also counts tasks waiting on a disk")
	}
	r.WaitingShort = waitingWhenFull(math.Max(0, one-r.Capacity), r.BusyShort, r.Capacity)
	r.WaitingLong = waitingWhenFull(math.Max(0, five-r.Capacity), r.BusyLong, r.Capacity)
	r.DemandShort, r.DemandLong = r.BusyShort, r.BusyLong
	r.BusyShort, r.BusyLong, r.DemandShort, r.DemandLong = round2(r.BusyShort), round2(r.BusyLong), round2(r.DemandShort), round2(r.DemandLong)
	// Nothing on Linux holds agent work back, so nothing steps aside for an urgent launch.
	r.FirmShort, r.FirmLong = r.DemandShort, r.DemandLong
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

// pidStat is one process as /proc/<pid>/stat describes it.
type pidStat struct {
	pid, parent uint32
	name        string
	// ticks is the processor time of the process and of every child it has waited for, so the
	// time of a compiler job that started and ended between two looks is still counted, under
	// the process that started it.
	ticks float64
	// start is when it began, in ticks since the machine started; pages its resident memory.
	start uint64
	pages float64
}

// parsePidStat reads one /proc/<pid>/stat. The name sits in brackets and may itself hold spaces
// and brackets, so the fields are counted from the last closing one.
func parsePidStat(text string) (pidStat, bool) {
	var p pidStat
	open, shut := strings.IndexByte(text, '('), strings.LastIndexByte(text, ')')
	if open < 0 || shut < open {
		return p, false
	}
	pid, err := strconv.ParseUint(strings.TrimSpace(text[:open]), 10, 32)
	if err != nil {
		return p, false
	}
	f := strings.Fields(text[shut+1:])
	// After the name: state(0) ppid(1) ... utime(11) stime(12) cutime(13) cstime(14) ... starttime(19) vsize(20) rss(21)
	if len(f) < 22 {
		return p, false
	}
	parent, err := strconv.ParseUint(f[1], 10, 32)
	if err != nil {
		return p, false
	}
	for _, i := range []int{11, 12, 13, 14} {
		v, err := strconv.ParseFloat(f[i], 64)
		if err != nil {
			return p, false
		}
		p.ticks += v
	}
	if p.start, err = strconv.ParseUint(f[19], 10, 64); err != nil {
		return p, false
	}
	if p.pages, err = strconv.ParseFloat(f[21], 64); err != nil {
		return p, false
	}
	p.pid, p.parent, p.name = uint32(pid), uint32(parent), text[open+1:shut]
	return p, true
}

// parseBootTime returns when the machine started, in seconds since 1970, from /proc/stat.
func parseBootTime(text string) (float64, bool) {
	for _, line := range strings.Split(text, "\n") {
		if rest, ok := strings.CutPrefix(line, "btime "); ok {
			v, err := strconv.ParseFloat(strings.TrimSpace(rest), 64)
			return v, err == nil
		}
	}
	return 0, false
}

// procSeen is what the look before knew of a process.
type procSeen struct {
	seconds float64 // its processor time then, its waited-for children's included
	parent  procKey
}

// usedSince turns two looks into what each living process used between them. A process's time
// includes its waited-for children's, so a child that ended since the look before shows up in
// its parent, whole: what was already counted for it while it lived is taken off the nearest
// ancestor still alive, which leaves that ancestor with its own use and the child's last moments.
// A look is not one instant, so a parent can be read a moment before its child's time lands on
// it; what was taken off too early is carried in carry to the look in which it lands. first says
// there was no look before, and then nothing is counted.
func usedSince(now, before map[procKey]procSeen, carry map[procKey]float64, first bool) map[procKey]float64 {
	used := make(map[procKey]float64, len(now))
	if first {
		return used
	}
	for k, p := range now {
		if b, seen := before[k]; seen {
			used[k] = p.seconds - b.seconds
		} else {
			used[k] = p.seconds
		}
		used[k] += carry[k]
	}
	for k, b := range before {
		if _, alive := now[k]; alive {
			continue
		}
		up := b.parent
		for hops := 0; hops < 64; hops++ {
			if _, alive := now[up]; alive {
				used[up] -= b.seconds
				break
			}
			gone, known := before[up]
			if !known {
				break
			}
			up = gone.parent
		}
	}
	for k := range carry {
		delete(carry, k)
	}
	for k, v := range used {
		if v < 0 {
			carry[k], used[k] = v, 0
		}
	}
	return used
}
