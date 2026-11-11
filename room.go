package main

// The room rule: whether one machine can take more work now, from what was measured on it. It
// replaces counting threads. Seven threads waiting on a model use a fraction of one processor and
// five threads building Rust use all of them, so a count is wrong in both directions; this asks
// what the work is really taking.
//
// A thread is let in while, after it is counted, the processors the work wants stay under the
// target share of the processors agent work may use, the memory reserve stays free and the disk
// reserve stays free. The work's want is the higher of its short and long average, so neither a
// calm minute in a busy hour nor a calm hour before a busy minute lets a thread in.
//
// Nothing is counted at a number somebody chose. The new work counts as what work like it was
// measured using (learn.go). The work already open counts as what each piece is expected to use,
// from its own measured minutes, even while it is quiet: six threads waiting on their models
// before each runs a gate are not an idle machine. And because open work does not all peak in the
// same minute, room is kept for its swing taken together (the spreads add as squares), not for
// every piece's peak: a steady build needs its average, a bursty thread needs more than its
// average, ten bursty threads need far less than ten peaks. Memory is judged the same way and
// not by adding up the most each piece ever held: the open work counts as what it is expected to
// hold, with room kept once for it to hold more (memLoad, work.go). Work let in a moment ago
// counts at its estimate only until the meter sees it running, and then as itself.

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
	// Used is what counts as taken now, Pending what work let in and not yet seen running is
	// counted as, Need what the new work adds, and Limit the most the three may add up to.
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
	Line Need `json:"line"`
	// Urgent says the launch was judged as urgent: against the work that would not step aside.
	Urgent bool `json:"urgent,omitempty"`
	// Sized is what one piece of the new work is expected to use and what that was learned from;
	// Measured what this machine itself measured for exactly this work, for the pool to pass on.
	Sized    Sizing  `json:"sized"`
	Measured *Sizing `json:"measured,omitempty"`
	// Desk is the desktop's side of the answer, for work that drives it; nil for all other work.
	Desk *Desk `json:"desktop,omitempty"`
	// Open is the work already open here as the rule counted it: how many pieces, what they use
	// over the short window, what they are expected to use, and their swing taken together.
	Open    OpenWork `json:"open"`
	Reading Reading  `json:"reading"`
}

// OpenWork is the open work of a machine, added up.
type OpenWork struct {
	Pieces int     `json:"pieces"`
	Now    float64 `json:"now"`
	Expect float64 `json:"expect"`
	Swing  float64 `json:"swing"`
	// Rest is the processors in use that belong to no piece of work.
	Rest float64 `json:"rest"`
	// MemGB is the memory the pieces hold now, MemExpect what they are expected to hold, and
	// MemKept what is kept free above that for when they hold more (memLoad.kept).
	MemGB     float64 `json:"mem_gb"`
	MemExpect float64 `json:"mem_expect"`
	MemKept   float64 `json:"mem_kept"`
}

// Desk says whether work that drives the desktop may start, and why not.
type Desk struct {
	OK  bool   `json:"ok"`
	Why string `json:"why,omitempty"`
}

// desktopFree is the rule for work that drives the desktop: work that opens windows on it, moves
// the pointer or captures the screen. It starts only on a machine that has a desktop, while
// nobody is at it, and one piece at a time; taken says such work was let in a moment ago.
//
// Why: on 2026-11-11 Jon's PC lagged about five seconds at a time while he worked. It was not
// short of processors: with 8 kept for him and 4 of them idle, Windows' window manager stalled 25
// times in 100 seconds, 0.2 to 2 seconds each (a frame is 17 to 40 ms), and the window he was in
// sat blocked waiting on it. Three gates of Owl's port were driving fixture windows, dragging and
// capturing the screen on his desktop at once; in the 50 seconds they paused the frames were
// normal at the same load. Processors, memory and disk all said there was room.
func desktopFree(r Reading, taken bool) Desk {
	switch r.Presence.State {
	case "none", "":
		return Desk{Why: "this machine has no desktop"}
	case "active":
		return Desk{Why: "someone is at the PC"}
	case "full-screen":
		return Desk{Why: cmpOr(r.Presence.App, "something") + " is full screen"}
	}
	for _, w := range r.Work {
		if w.Desktop {
			return Desk{Why: "the " + workWords(w) + " has it"}
		}
	}
	if taken {
		return Desk{Why: "work let in a moment ago is about to drive it"}
	}
	return Desk{OK: true}
}

// Ask is what the caller wants room for.
type Ask struct {
	// Threads is how many agent threads are open on the machine now; 0 when the caller does not
	// know. It matters only on a machine that has measured no work yet.
	Threads int
	// Count is how many new pieces are asked for at once.
	Count int
	// Explicit is the caller's own guess. Its processors and memory are where the estimate starts,
	// and stand only until work like this is measured; its disk replaces the checkout's.
	Explicit Need
	// Disk says the work makes a checkout, so the disk is checked too.
	Disk bool
	// Kind is "run" for a run; anything else is a thread.
	Kind string
	// Learned is the evidence about work like this, the widest level first (learned.levels).
	Learned []level
	// Urgent judges the launch against the work that would not step aside for it.
	Urgent bool
	// Desktop says the work drives the desktop (desktopFree), and DesktopTaken that such work was
	// let in a moment ago and is not yet seen running.
	Desktop, DesktopTaken bool
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

// estimate is what one new piece of work is counted as on this machine, and what that came from.
func estimate(r Reading, s Settings, a Ask) (Need, Sizing) {
	base := Need{Cores: s.ThreadCores, MemGB: s.ThreadMemGB}
	if len(a.Learned) == 0 && a.Threads > 0 {
		// A machine that has measured no work yet: its average per open thread, capped so one
		// heavy hour cannot make every later thread look as heavy as a quarter of the machine.
		demand := math.Max(r.DemandShort, r.DemandLong)
		base.Cores = math.Max(base.Cores, math.Min(demand/float64(a.Threads), r.Capacity/4))
		work := r.WorkMemGB
		if r.OS == "linux" {
			work = r.MemTotalGB - r.MemAvailGB
		}
		base.MemGB = math.Max(base.MemGB, math.Min(work/float64(a.Threads), r.MemTotalGB/8))
	}
	if a.Explicit.Cores > 0 {
		base.Cores = a.Explicit.Cores
	}
	if a.Explicit.MemGB > 0 {
		base.MemGB = a.Explicit.MemGB
	}
	sz := size(base, a.Learned, s.LearnTrustMinutes)
	// No piece of work uses more processors than the machine has, and one that was measured
	// holding most of a bigger machine's memory is not refused here for ever.
	sz.Cores = math.Min(sz.Cores, r.Capacity)
	sz.Swing = math.Min(sz.Swing, r.Capacity-sz.Cores)
	sz.MemMost = math.Min(sz.MemMost, r.MemTotalGB/2)
	sz.MemGB = math.Min(sz.MemGB, sz.MemMost)
	n := Need{Cores: sz.Cores, MemGB: sz.MemGB}
	if a.Disk {
		n.DiskGB = s.ThreadDiskGB
	}
	if a.Explicit.DiskGB > 0 {
		n.DiskGB = a.Explicit.DiskGB
	}
	return n, sz
}

// decide answers for one machine. pending is what work let in and not yet seen running counts as.
func decide(r Reading, s Settings, a Ask, pending Need) Verdict {
	return decideBehind(r, s, a, pending, Need{})
}

// decideBehind answers for a launch that arrives behind a line: granted is what work let in and
// not yet seen running counts as, line what the launches waiting ahead of it do.
func decideBehind(r Reading, s Settings, a Ask, granted, line Need) Verdict {
	pending := granted.plus(line)
	if a.Count < 1 {
		a.Count = 1
	}
	one, sz := estimate(r, s, a)
	need := one.times(a.Count)
	target := cpuTarget(r, s)
	v := Verdict{Machine: r.Machine, Target: target, Threads: a.Threads, Count: a.Count, Reading: r, Line: line, Urgent: a.Urgent, Sized: sz}
	v.Sized.Cores, v.Sized.Swing, v.Sized.MemGB = round2(sz.Cores), round2(sz.Swing), round2(sz.MemGB)
	v.Sized.MemSwing, v.Sized.MemMost = round2(sz.MemSwing), round2(sz.MemMost)

	// What is counted as in use: the processors busy over the short and the long window, and
	// what the open work is expected to use with room for its swing, whichever is most.
	busy := math.Max(r.DemandShort, r.DemandLong)
	now, expect, swingSq, mem := openWork(r.Work)
	rest := math.Max(0, r.DemandShort-now)
	v.Open = OpenWork{Pieces: len(r.Work), Now: round2(now), Expect: round2(expect), Swing: round2(math.Sqrt(swingSq)), Rest: round2(rest)}
	used := math.Max(busy, rest+expect+s.LearnMargin*math.Sqrt(swingSq))
	after := math.Max(busy+need.Cores, rest+expect+need.Cores+s.LearnMargin*math.Sqrt(swingSq+float64(a.Count)*sz.Swing*sz.Swing))
	if a.Urgent {
		// The open work steps aside for it, so only what would not is counted.
		used = math.Max(r.FirmShort, r.FirmLong)
		after = used + need.Cores
		mem = memLoad{}
	}
	v.CPU = Limit{Used: used, Pending: pending.Cores, Need: after - used, Limit: r.Capacity * target, Unit: "processors"}
	v.CPU.OK = used+pending.Cores+v.CPU.Need <= v.CPU.Limit+1e-9

	// Memory the same way. What counts as taken is what is in use now, and on top of it whatever
	// the open work is expected to hold, with room for it to hold more (memLoad.kept), beyond
	// what it holds already. The new work adds what it is expected to hold and whatever more has
	// to be kept once it is one of them. It is the difference that is added, never the pieces'
	// memory taken off the machine's: a piece's memory counts shared pages more than once, so the
	// pieces can add up to more than the machine has in use.
	memUsed := r.MemTotalGB - r.MemAvailGB
	memTaken := memUsed + math.Max(0, mem.expect+mem.kept(s.LearnMemMargin)-mem.held)
	with := mem
	with.add(0, sz.MemGB, sz.MemSwing, sz.MemMost, a.Count)
	memAfter := memUsed + math.Max(need.MemGB, with.expect+with.kept(s.LearnMemMargin)-mem.held)
	v.Open.MemGB, v.Open.MemExpect, v.Open.MemKept = round2(mem.held), round2(mem.expect), round2(mem.kept(s.LearnMemMargin))
	reserve := r.MemTotalGB * s.MemReserveFraction
	v.Mem = Limit{Used: memUsed, Pending: pending.MemGB + memTaken - memUsed, Need: memAfter - memTaken, Limit: r.MemTotalGB - reserve, Unit: "GB of memory"}
	v.Mem.OK = v.Mem.Used+v.Mem.Pending+v.Mem.Need <= v.Mem.Limit+1e-9

	v.Disk = Limit{OK: true, Pending: pending.DiskGB, Need: need.DiskGB, Limit: r.DiskFreeGB - s.DiskReserveGB, Unit: "GB of disk"}
	if a.Disk {
		v.Disk.OK = pending.DiskGB+need.DiskGB <= v.Disk.Limit+1e-9
	}

	v.OK = v.CPU.OK && v.Mem.OK && v.Disk.OK
	if a.Desktop {
		d := desktopFree(r, a.DesktopTaken)
		v.Desk = &d
		v.OK = v.OK && d.OK
	}
	v.Reason = reason(v, s, a)
	return v
}

// countedWords says what the new work counts as and why.
func countedWords(v Verdict, a Ask) string {
	what := "it counts"
	if v.Count > 1 {
		what = "they count"
	}
	switch {
	case v.Sized.From != "":
		text := fmt.Sprintf("%s as %.1f processors", what, v.CPU.Need)
		if v.Sized.Swing >= 0.1 && v.Count == 1 {
			text += fmt.Sprintf(" (%.1f expected and %.1f for its swing)", v.Sized.Cores, v.CPU.Need-v.Sized.Cores)
		}
		text += ": " + sizedWords(v.Sized)
		if math.Abs(v.Sized.Cores-v.Sized.FromCores) >= 0.3 {
			text += ", too few minutes yet to count alone"
		}
		return text
	case a.Explicit.Cores > 0:
		return fmt.Sprintf("%s as %.1f processors, its caller's own number, until work like it is measured here", what, v.CPU.Need)
	case a.Threads > 0:
		return fmt.Sprintf("%s as %.1f processors, the machine's average per open thread, until work like it is measured here", what, v.CPU.Need)
	}
	return fmt.Sprintf("%s as %.1f processors: %s", what, v.CPU.Need, sizedWords(v.Sized))
}

// openWords says what the open work was counted as, when that is more than it uses this minute.
func openWords(v Verdict) string {
	r := v.Reading
	if v.Urgent || v.Open.Pieces == 0 || v.CPU.Used <= math.Max(r.DemandShort, r.DemandLong)+0.05 {
		return ""
	}
	text := fmt.Sprintf(" The %d pieces of work open here use %.1f this minute and are expected to use %.1f", v.Open.Pieces, v.Open.Now, v.Open.Expect)
	if v.Open.Pieces == 1 {
		text = fmt.Sprintf(" The one piece of work open here uses %.1f this minute and is expected to use %.1f", v.Open.Now, v.Open.Expect)
	}
	if v.Open.Swing >= 0.1 {
		text += fmt.Sprintf(", give or take %.1f", v.Open.Swing)
	}
	if v.Open.Rest >= 0.1 {
		text += fmt.Sprintf(", beside %.1f that is no piece of work", v.Open.Rest)
	}
	return text + fmt.Sprintf(": counted as %.1f in use.", v.CPU.Used)
}

// openMemWords says what the open work's memory was counted as, when that is more than it holds.
func openMemWords(v Verdict) string {
	o := v.Open
	if v.Urgent || o.Pieces == 0 || o.MemExpect+o.MemKept <= o.MemGB+0.05 {
		return ""
	}
	text := fmt.Sprintf(" The %d pieces of work open here hold %.1f GB and are expected to hold %.1f", o.Pieces, o.MemGB, o.MemExpect)
	if o.Pieces == 1 {
		text = fmt.Sprintf(" The one piece of work open here holds %.1f GB and is expected to hold %.1f", o.MemGB, o.MemExpect)
	}
	if o.MemKept >= 0.1 {
		if o.Pieces > 1 {
			text += fmt.Sprintf(", with %.1f kept for when they hold more (never every piece at its most at once)", o.MemKept)
		} else {
			text += fmt.Sprintf(", with %.1f kept for when it holds more", o.MemKept)
		}
	}
	return text + "."
}

func reason(v Verdict, s Settings, a Ask) string {
	r := v.Reading
	what := "a new thread"
	if v.Count > 1 {
		what = fmt.Sprintf("%d new threads", v.Count)
	}
	if a.Kind == "run" {
		what = "this run"
		if v.Count > 1 {
			what = fmt.Sprintf("%d of this run", v.Count)
		}
	}
	if v.Urgent {
		what = "an urgent " + strings.TrimPrefix(what, "a ")
	}
	var b strings.Builder
	counted := countedWords(v, a)
	if v.OK {
		if v.Urgent {
			held := math.Max(r.DemandShort, r.DemandLong) - v.CPU.Used
			fmt.Fprintf(&b, "%s has room for %s: work that does not step aside uses %.1f of the %.0f processors agent work may use (limit %.1f), and %s. The other agent work (%.1f processors) is held at idle priority while it runs; nothing is stopped, and the processors kept for you stay yours.",
				r.Machine, what, v.CPU.Used+v.CPU.Pending, r.Capacity, v.CPU.Limit, counted, max(0, held))
			return b.String()
		}
		fmt.Fprintf(&b, "%s has room for %s: work is using %.1f of the %.0f processors it may use (limit %.1f), %.1f GB of memory is free and %.0f GB of disk; %s.%s",
			r.Machine, what, math.Max(r.DemandShort, r.DemandLong)+v.CPU.Pending, r.Capacity, v.CPU.Limit, r.MemAvailGB, r.DiskFreeGB, counted, openWords(v))
		return b.String()
	}
	fmt.Fprintf(&b, "%s has no room for %s.", r.Machine, what)
	if v.Desk != nil && !v.Desk.OK {
		fmt.Fprintf(&b, " It drives the desktop, and %s: work that opens windows on the screen, moves the pointer or captures it starts only while nobody is at the PC, one piece at a time.", v.Desk.Why)
	}
	if !v.CPU.OK {
		if v.Urgent {
			fmt.Fprintf(&b, " Even with other agent work held back, work that does not step aside uses %.1f of the %.0f processors agent work may use", v.CPU.Used, r.Capacity)
		} else {
			fmt.Fprintf(&b, " Work is using %.1f of the %.0f processors agent work may use, over the last %s", math.Max(r.DemandShort, r.DemandLong), r.Capacity, span(windowOf(r)))
		}
		if w := waitingOf(r); w >= 1 {
			fmt.Fprintf(&b, " (and %.0f more threads waited for one while they were full)", w)
		}
		if over := v.CPU.Used - math.Max(r.DemandShort, r.DemandLong); !v.Urgent && over > 0.05 {
			fmt.Fprintf(&b, ", the work open here is expected to use %.1f more than it does now", over)
		}
		if let := v.CPU.Pending - v.Line.Cores; let > 0.05 {
			fmt.Fprintf(&b, ", work let in and not yet seen running counts as %.1f more", let)
		}
		if v.Line.Cores > 0 {
			fmt.Fprintf(&b, ", the launches ahead of it in the line count as %.1f more", v.Line.Cores)
		}
		fmt.Fprintf(&b, ", and %s counts as %.1f; the limit is %.1f (%.0f%%%s).", what, v.CPU.Need, v.CPU.Limit, v.Target*100, presenceWords(r.Presence))
		if v.Sized.From != "" {
			words := sizedWords(v.Sized)
			fmt.Fprintf(&b, " %s%s.", strings.ToUpper(words[:1]), words[1:])
		}
		b.WriteString(openWords(v))
		if top := programWords(r.Programs, 4); top != "" {
			fmt.Fprintf(&b, " Busiest: %s.", top)
		}
	}
	if !v.Mem.OK {
		fmt.Fprintf(&b, " %.1f GB of %.0f GB of memory is free, %.1f GB is kept free, and %s counts as %.1f GB",
			r.MemAvailGB, r.MemTotalGB, r.MemTotalGB*s.MemReserveFraction, what, v.Mem.Need)
		if more := v.Mem.Need - v.Sized.MemGB*float64(v.Count); more >= 0.1 {
			fmt.Fprintf(&b, " (%.1f expected and %.1f for how far it reaches above that)", v.Sized.MemGB*float64(v.Count), more)
		}
		if v.Mem.Pending > 0 {
			fmt.Fprintf(&b, " on top of %.1f GB that work let in or open here is still expected to take", v.Mem.Pending)
		}
		b.WriteString(".")
		b.WriteString(openMemWords(v))
	}
	if !v.Disk.OK {
		fmt.Fprintf(&b, " %s has %.1f GB free, %.0f GB is kept free, and %s with a checkout counts as %.0f GB",
			r.DiskPath, r.DiskFreeGB, s.DiskReserveGB, what, v.Disk.Need)
		if v.Disk.Pending > 0 {
			fmt.Fprintf(&b, " on top of %.0f GB for work just let in", v.Disk.Pending)
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
