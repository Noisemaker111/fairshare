package main

// The room rule: whether one machine can take more work now, from what was measured on it. It
// replaces counting threads. Seven threads waiting on a model use a fraction of one processor and
// five threads building Rust use all of them, so a count is wrong in both directions; this asks
// what the work is really taking.
//
// A thread is let in while, after it is counted, the processors the work wants stay under the
// target share of the processors agent work may use, the memory reserve stays free and the disk
// reserve stays free. The work's want is the higher of its short and long average, so neither a
// calm minute in a busy hour nor a calm hour before a busy minute lets a thread in. A new thread
// is counted at the machine's own measured average per open thread, and never at less than the
// settings' floor; threads let in during the last settle time are still counted at that estimate,
// because their load does not show yet.

import (
	"fmt"
	"math"
	"strings"
)

// Need is what a piece of work is counted as.
type Need struct {
	Cores  float64 `json:"cores"`
	MemGB  float64 `json:"mem_gb"`
	DiskGB float64 `json:"disk_gb"`
}

func (n Need) times(k int) Need {
	return Need{n.Cores * float64(k), n.MemGB * float64(k), n.DiskGB * float64(k)}
}

func (n Need) plus(o Need) Need {
	return Need{n.Cores + o.Cores, n.MemGB + o.MemGB, n.DiskGB + o.DiskGB}
}

// Limit is one resource's side of the answer.
type Limit struct {
	OK bool `json:"ok"`
	// Used is what is taken now, Pending what threads just let in are counted as, Need what the
	// new work is counted as, and Limit the most the three may add up to.
	Used    float64 `json:"used"`
	Pending float64 `json:"pending"`
	Need    float64 `json:"need"`
	Limit   float64 `json:"limit"`
	Unit    string  `json:"unit"`
}

// Verdict is the answer for one machine.
type Verdict struct {
	OK      bool    `json:"ok"`
	Reason  string  `json:"reason"`
	Machine string  `json:"machine"`
	Target  float64 `json:"cpu_target"`
	Threads int     `json:"threads"`
	Count   int     `json:"count"`
	CPU     Limit   `json:"cpu"`
	Mem     Limit   `json:"mem"`
	Disk    Limit   `json:"disk"`
	// Line is the part of each Pending that is launches waiting in the line ahead of this one.
	Line    Need    `json:"line"`
	Reading Reading `json:"reading"`
}

// Ask is what the caller wants room for.
type Ask struct {
	// Threads is how many agent threads are open on the machine now; 0 when the caller does not
	// know, and then a new thread is counted at the settings' floor.
	Threads int
	// Count is how many new threads are asked for at once.
	Count int
	// Explicit, when a field is more than 0, replaces the estimate for one new thread.
	Explicit Need
	// Disk says the work makes a checkout, so the disk is checked too.
	Disk bool
}

func cpuTarget(r Reading, s Settings) float64 {
	switch r.Presence.State {
	case "full-screen":
		return s.CPUTargetFullScreen
	case "away", "none":
		return s.CPUTargetAway
	}
	return s.CPUTarget
}

// estimate is what one new thread is counted as on this machine.
func estimate(r Reading, s Settings, a Ask) Need {
	n := Need{Cores: s.ThreadCores, MemGB: s.ThreadMemGB}
	if a.Threads > 0 {
		// The machine's own average per open thread, capped so one heavy hour cannot make every
		// later thread look as heavy as a quarter of the machine.
		demand := math.Max(r.DemandShort, r.DemandLong)
		n.Cores = math.Max(n.Cores, math.Min(demand/float64(a.Threads), r.Capacity/4))
		work := r.WorkMemGB
		if r.OS == "linux" {
			work = r.MemTotalGB - r.MemAvailGB
		}
		n.MemGB = math.Max(n.MemGB, math.Min(work/float64(a.Threads), r.MemTotalGB/8))
	}
	if a.Disk {
		n.DiskGB = s.ThreadDiskGB
	}
	if a.Explicit.Cores > 0 {
		n.Cores = a.Explicit.Cores
	}
	if a.Explicit.MemGB > 0 {
		n.MemGB = a.Explicit.MemGB
	}
	if a.Explicit.DiskGB > 0 {
		n.DiskGB = a.Explicit.DiskGB
	}
	return n
}

// decide answers for one machine. pending is what threads let in during the settle time count as.
func decide(r Reading, s Settings, a Ask, pending Need) Verdict {
	return decideBehind(r, s, a, pending, Need{})
}

// decideBehind answers for a launch that arrives behind a line: granted is what threads let in
// during the settle time count as, line what the launches waiting ahead of it do.
func decideBehind(r Reading, s Settings, a Ask, granted, line Need) Verdict {
	pending := granted.plus(line)
	if a.Count < 1 {
		a.Count = 1
	}
	need := estimate(r, s, a).times(a.Count)
	target := cpuTarget(r, s)
	v := Verdict{Machine: r.Machine, Target: target, Threads: a.Threads, Count: a.Count, Reading: r, Line: line}

	demand := math.Max(r.DemandShort, r.DemandLong)
	v.CPU = Limit{Used: demand, Pending: pending.Cores, Need: need.Cores, Limit: r.Capacity * target, Unit: "processors"}
	v.CPU.OK = demand+pending.Cores+need.Cores <= v.CPU.Limit+1e-9

	reserve := r.MemTotalGB * s.MemReserveFraction
	v.Mem = Limit{Used: r.MemTotalGB - r.MemAvailGB, Pending: pending.MemGB, Need: need.MemGB, Limit: r.MemTotalGB - reserve, Unit: "GB of memory"}
	v.Mem.OK = v.Mem.Used+pending.MemGB+need.MemGB <= v.Mem.Limit+1e-9

	v.Disk = Limit{OK: true, Pending: pending.DiskGB, Need: need.DiskGB, Limit: r.DiskFreeGB - s.DiskReserveGB, Unit: "GB of disk"}
	if a.Disk {
		v.Disk.OK = pending.DiskGB+need.DiskGB <= v.Disk.Limit+1e-9
	}

	v.OK = v.CPU.OK && v.Mem.OK && v.Disk.OK
	v.Reason = reason(v, s, a)
	return v
}

func reason(v Verdict, s Settings, a Ask) string {
	r := v.Reading
	what := "a new thread"
	if v.Count > 1 {
		what = fmt.Sprintf("%d new threads", v.Count)
	}
	var b strings.Builder
	if v.OK {
		fmt.Fprintf(&b, "%s has room for %s: work is using %.1f of the %.0f processors it may use (limit %.1f), %.1f GB of memory is free and %.0f GB of disk.",
			r.Machine, what, v.CPU.Used+v.CPU.Pending, r.Capacity, v.CPU.Limit, r.MemAvailGB, r.DiskFreeGB)
		return b.String()
	}
	fmt.Fprintf(&b, "%s has no room for %s.", r.Machine, what)
	if !v.CPU.OK {
		fmt.Fprintf(&b, " Work is using %.1f of the %.0f processors agent work may use, over the last %s", v.CPU.Used, r.Capacity, span(windowOf(r)))
		if w := waitingOf(r); w >= 1 {
			fmt.Fprintf(&b, " (and %.0f more threads waited for one while they were full)", w)
		}
		if let := v.CPU.Pending - v.Line.Cores; let > 0.05 {
			fmt.Fprintf(&b, ", threads let in during the last %.0f minutes count as %.1f more", s.SettleMinutes, let)
		}
		if v.Line.Cores > 0 {
			fmt.Fprintf(&b, ", the launches ahead of it in the line count as %.1f more", v.Line.Cores)
		}
		fmt.Fprintf(&b, ", and %s counts as %.1f; the limit is %.1f (%.0f%%%s).", what, v.CPU.Need, v.CPU.Limit, v.Target*100, presenceWords(r.Presence))
		if top := programWords(r.Programs, 4); top != "" {
			fmt.Fprintf(&b, " Busiest: %s.", top)
		}
	}
	if !v.Mem.OK {
		fmt.Fprintf(&b, " %.1f GB of %.0f GB of memory is free, %.1f GB is kept free, and %s counts as %.1f GB",
			r.MemAvailGB, r.MemTotalGB, r.MemTotalGB*s.MemReserveFraction, what, v.Mem.Need)
		if v.Mem.Pending > 0 {
			fmt.Fprintf(&b, " on top of %.1f GB for threads just let in", v.Mem.Pending)
		}
		b.WriteString(".")
	}
	if !v.Disk.OK {
		fmt.Fprintf(&b, " %s has %.1f GB free, %.0f GB is kept free, and %s with a checkout counts as %.0f GB",
			r.DiskPath, r.DiskFreeGB, s.DiskReserveGB, what, v.Disk.Need)
		if v.Disk.Pending > 0 {
			fmt.Fprintf(&b, " on top of %.0f GB for threads just let in", v.Disk.Pending)
		}
		b.WriteString(".")
	}
	return b.String()
}

// waitingOf is the threads that waited in the window whose demand is the higher.
func waitingOf(r Reading) float64 {
	if r.DemandLong > r.DemandShort {
		return r.WaitingLong
	}
	return r.WaitingShort
}

// windowOf is the length of the window whose demand is the higher.
func windowOf(r Reading) float64 {
	if r.DemandLong > r.DemandShort {
		return r.LongSeconds
	}
	return r.ShortSeconds
}

func span(seconds float64) string {
	if seconds >= 90 {
		return fmt.Sprintf("%.0f minutes", seconds/60)
	}
	if seconds >= 45 {
		return "minute"
	}
	return fmt.Sprintf("%.0f seconds", seconds)
}

func presenceWords(p Presence) string {
	switch p.State {
	case "full-screen":
		if p.App != "" {
			return ", because " + p.App + " is full screen"
		}
		return ", because something is full screen"
	case "away":
		return fmt.Sprintf(", nobody at the PC for %.0f minutes", p.IdleMinutes)
	}
	return ""
}

func programWords(ps []Program, n int) string {
	var parts []string
	for i, p := range ps {
		if i == n {
			break
		}
		parts = append(parts, fmt.Sprintf("%s %.1f", p.Name, p.Cores))
	}
	return strings.Join(parts, ", ")
}
