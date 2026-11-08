package main

// A lane is one chat: its harness process and everything under it, held in a jobSet adopt owns.
// The main job is named so `fairshare throttle` can find the chat; work Windows will not put in it
// gets side jobs (see jobset.go). Only a process that refuses even an empty job stays outside, and
// gets the lane's priorities directly. Either way the lane counts it.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Noisemaker111/fairshare/internal/windows"
)

type laneProc struct {
	name    string
	cpu     float64
	outside bool
}

type lane struct {
	harness    uint32
	created    uint64
	name       string
	unit       *Unit // the main job, named laneName
	jobs       jobSet
	jobCPU     map[windows.Handle]float64 // CPU seconds each job had at the last cap update
	started    time.Time
	outside    map[uint32]bool // processes that refused even an empty job
	whyOutside string
	procs      map[[2]uint64]*laneProc // every process seen twice a second, by ID and start time
	lastCPU    float64
	cores      float64
	peakCores  float64
	cpuWaitS   float64 // thread-seconds spent ready to run but given no CPU
	heldS      float64 // seconds this chat was held back for others
	holds      int
	demoted    bool
	lowered    map[uint32]bool
	capPct     float64 // the throttle in force, percent of the whole PC; 0 for none
	done       bool    // closed; its jobs are gone
	measured   bool    // lastCPU holds a measurement
	joined     int
	sided      int
}

func newLane(gov *Governor, harness uint32, created uint64, name string, u *Unit) *lane {
	l := &lane{harness: harness, created: created, name: name, unit: u, started: time.Now(), jobCPU: map[windows.Handle]float64{},
		outside: map[uint32]bool{}, procs: map[[2]uint64]*laneProc{}, lowered: map[uint32]bool{}}
	l.jobs = jobSet{main: u.Job, newSide: func() (windows.Handle, func(), error) {
		side, err := gov.NewUnit(false, "")
		if err != nil {
			return 0, nil, err
		}
		return side.Job, func() { gov.Close(side) }, nil
	}}
	return l
}

// place puts a process in the lane: the main job, else a side job of its own, else tuned directly.
func (l *lane) place(gov *Governor, pid uint32, h windows.Handle) string {
	side, err := l.jobs.place(h)
	switch {
	case err == nil && side == 0:
		l.joined++
		return "joined"
	case err == nil:
		l.sided++
		if l.capPct > 0 {
			setCPUCap(side, l.capPct/float64(len(l.jobs.handles())))
		}
		return "side job"
	}
	l.outside[pid] = true
	l.whyOutside = err.Error()
	windows.SetPriorityClass(h, gov.policy.PriorityClass)
	gov.tune(pid)
	return "outside: " + err.Error()
}

// pids lists the lane's live processes (side jobs that have emptied are released on the way).
func (l *lane) pids() map[uint32]bool {
	s := map[uint32]bool{}
	for _, p := range l.jobs.pids() {
		s[p] = true
	}
	for p := range l.outside {
		s[p] = true
	}
	return s
}

// cpu is everything the chat used: jobs count all their processes, even ones gone before any
// sample; processes outside every job count as last seen.
func (l *lane) cpu(js JobStats) float64 {
	c := js.CPUSeconds
	for _, p := range l.procs {
		if p.outside {
			c += p.cpu
		}
	}
	return c
}

// applyCap holds the lane's jobs together under pct percent of the PC: each job's share follows
// what it used last time, so a lone busy job gets nearly all of it and the total never exceeds it.
func (l *lane) applyCap(pct float64) {
	jobs := l.jobs.handles()
	if pct <= 0 {
		if l.capPct > 0 {
			for _, j := range jobs {
				setCPUCap(j, 0)
			}
		}
		l.capPct = 0
		return
	}
	l.capPct = pct
	use := make([]float64, len(jobs))
	total := 0.0
	for i, j := range jobs {
		c := jobStats(j).CPUSeconds
		use[i] = max(0, c-l.jobCPU[j]) + 0.01 // a little for idle jobs, so they can start
		l.jobCPU[j] = c
		total += use[i]
	}
	for i, j := range jobs {
		setCPUCap(j, max(0.01, pct*use[i]/total))
	}
}

// laneBalance is one measurement of every lane, taken from the same snapshot adopt uses to find
// new processes, so measuring costs no extra look at the PC.
type laneBalance struct {
	lanes   []*lane
	sets    []map[uint32]bool
	states  []ThreadStates
	threads [][]threadInfo
	procs   map[uint32]procInfo
}

func startBalance(lanes []*lane) *laneBalance {
	b := &laneBalance{lanes: lanes, sets: make([]map[uint32]bool, len(lanes)), states: make([]ThreadStates, len(lanes)),
		threads: make([][]threadInfo, len(lanes)), procs: map[uint32]procInfo{}}
	group := map[uint32]int{}
	for i, l := range lanes {
		b.sets[i] = l.pids()
		for p := range b.sets[i] {
			group[p] = i
		}
	}
	scan(func(e procEntry) bool {
		b.procs[e.pid] = procInfo{ppid: e.ppid, name: e.name, created: e.created, unstarted: e.unstarted}
		i, ok := group[e.pid]
		if !ok {
			return false
		}
		l := lanes[i]
		k := [2]uint64{uint64(e.pid), e.created}
		p := l.procs[k]
		if p == nil {
			p = &laneProc{name: e.name, outside: l.outside[e.pid]}
			l.procs[k] = p
		}
		p.cpu = e.cpu
		return true
	}, func(t threadInfo) {
		i := group[t.pid]
		b.states[i].add(t)
		if lanes[i].demoted || t.state == 1 || t.state == 3 || t.state == 7 {
			b.threads[i] = append(b.threads[i], t)
		}
	})
	return b
}

// finish applies throttles and, when allowed, holds back the chats that take more than an equal
// share while another chat waits for the CPU. caps are the throttles in force, by lane key.
// Lanes closed since the measurement are skipped. It says whether any throttle changed.
func (b *laneBalance) finish(dt float64, allowed bool, caps map[string]float64) (capsChanged bool) {
	lanes, states, sets := b.lanes, b.states, b.sets
	total, active := 0.0, 0
	for i, l := range lanes {
		if l.done {
			continue
		}
		was := l.capPct
		l.applyCap(caps[l.key()])
		capsChanged = capsChanged || l.capPct != was
		// the first look only sets the start: processes bring the CPU they used before joining
		c := l.cpu(l.jobs.stats())
		if l.measured {
			l.cores = max(0, (c-l.lastCPU)/dt)
		}
		l.lastCPU, l.measured = c, true
		l.peakCores = max(l.peakCores, l.cores)
		l.cpuWaitS += float64(states[i].Ready) * dt
		if l.cores >= 0.1 {
			active++
			total += l.cores
		}
	}
	equal := total / float64(max(1, active))
	starved := false
	for i, l := range lanes {
		if !l.done && states[i].Ready > 0 && l.cores < equal {
			starved = true
		}
	}
	for i, l := range lanes {
		if l.done {
			continue
		}
		hold := allowed && active >= 2 && l.cores > 1.25*equal && (starved || l.demoted)
		switch {
		case hold:
			if !l.demoted {
				l.holds++
				l.demoted = true
			}
			// the measured threads: all of a held lane's, else the ones that want a CPU now;
			// later looks catch threads started since
			lowerThreads(b.threads[i], l.lowered)
			l.heldS += dt
		case l.demoted:
			l.demoted = false
			restoreThreads(sets[i], l.lowered)
		}
	}
	return capsChanged
}

// close logs what the chat cost. When the chat itself ended (procs is the look that found it gone),
// everything left in its lane ends too: nothing a chat started outlives it. Windows never ends a
// child when its parent dies, so without this a helper whose parent was killed ran on for days
// (2026-11-07: 75 Convex `cvx-node-executor` node.exe, up to 94 h old, their backend long gone).
// When adopt stops (procs is nil), adopted work runs on at low priority, without a cap.
func (l *lane) close(gov *Governor, why string, procs map[uint32]procInfo) {
	l.done = true
	ended := 0
	if procs != nil {
		ended = l.end(procs)
	}
	pids := l.pids()
	if l.demoted {
		restoreThreads(pids, l.lowered)
	}
	l.applyCap(0)
	js := l.jobs.stats()
	cpu := l.cpu(js)
	byName := map[string]float64{}
	seenInJobs, outside := 0.0, 0
	for _, p := range l.procs {
		byName[p.name] += p.cpu
		if p.outside {
			outside++
		} else {
			seenInJobs += p.cpu
		}
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	slices.SortFunc(names, func(a, b string) int { return int(1000 * (byName[b] - byName[a])) })
	var top []string
	for _, n := range names[:min(8, len(names))] {
		top = append(top, fmt.Sprintf("%s %.1fs", or(n, "?"), byName[n]))
	}
	wall := time.Since(l.started).Seconds()
	logEvent("lane", map[string]any{
		"harness": l.name, "pid": l.harness, "why": why,
		"wall_ms": int(wall * 1000), "cpu_ms": int(cpu * 1000), "avg_cores": round2(cpu / max(wall, 0.001)), "peak_cores": round2(l.peakCores),
		"procs": int(js.Processes) + outside, "joined": l.joined, "side_jobs": l.sided, "outside": outside, "outside_why": l.whyOutside,
		"peak_mb": js.PeakMB, "read_mb": js.ReadMB, "write_mb": js.WriteMB,
		"held_ms": int(l.heldS * 1000), "holds": l.holds, "cpu_wait_ms": int(l.cpuWaitS * 1000),
		"short_cpu_ms": int(max(0, js.CPUSeconds-seenInJobs) * 1000), "top": strings.Join(top, ", "),
		"ended": ended,
	})
	l.jobs.release()
	gov.Close(l.unit)
}

// end stops everything left in the lane: every process in its jobs (and the jobs nested in them),
// every process that refused a job, and anything below those that has not been placed yet. It
// returns how many processes it stopped.
func (l *lane) end(procs map[uint32]procInfo) int {
	left := l.pids()
	for p := range l.pids() {
		for _, d := range descendants(procs, p) {
			left[d] = true
		}
	}
	for _, j := range l.jobs.handles() {
		windows.TerminateJobObject(j, 1)
	}
	self := windows.GetCurrentProcessId()
	for pid := range left {
		p, ok := procs[pid]
		if !ok || pid == self || pid == l.harness || (p.created != 0 && p.created != creationTime(pid)) {
			continue // gone, or the ID now names another process
		}
		if h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, pid); err == nil {
			windows.TerminateProcess(h, 1)
			windows.CloseHandle(h)
		}
	}
	return len(left)
}

func round2(f float64) float64 { return float64(int(f*100+0.5)) / 100 }

// key names the lane in throttle files and the status file: harness ID and start time.
func (l *lane) key() string { return fmt.Sprintf("%d-%d", l.harness, l.created) }

func throttleDir() string { return filepath.Join(filepath.Dir(eventsFile()), "throttle") }

// readCaps reads the throttles `fairshare throttle` left for adopt: one file per chat.
func readCaps() map[string]float64 {
	caps := map[string]float64{}
	entries, _ := os.ReadDir(throttleDir())
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(throttleDir(), e.Name()))
		if err != nil {
			continue
		}
		if pct, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64); err == nil {
			caps[e.Name()] = pct
		}
	}
	return caps
}

// laneStatus is one chat as adopt last saw it, for `fairshare throttle` to list.
type laneStatus struct {
	PID      uint32  `json:"pid"`
	Harness  string  `json:"harness"`
	Cores    float64 `json:"cores"`
	CPUS     float64 `json:"cpu_s"`
	Procs    int     `json:"procs"`
	Jobs     int     `json:"jobs"`
	Outside  int     `json:"outside"`
	Held     bool    `json:"held"`
	CapPct   float64 `json:"cap_pct"`
	PeakMB   float64 `json:"peak_mb"`
	AliveMin float64 `json:"alive_min"`
}

func statusFile() string { return filepath.Join(filepath.Dir(eventsFile()), "lanes.json") }

func writeStatus(lanes []*lane) {
	out := struct {
		At    string       `json:"at"`
		Lanes []laneStatus `json:"lanes"`
	}{At: time.Now().Format(time.RFC3339)}
	for _, l := range lanes {
		js := l.jobs.stats()
		out.Lanes = append(out.Lanes, laneStatus{PID: l.harness, Harness: l.name, Cores: round2(l.cores), CPUS: round2(l.cpu(js)),
			Procs: int(js.Processes) + len(l.outside), Jobs: len(l.jobs.handles()), Outside: len(l.outside), Held: l.demoted, CapPct: l.capPct,
			PeakMB: js.PeakMB, AliveMin: round2(time.Since(l.started).Minutes())})
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	os.WriteFile(statusFile()+".tmp", b, 0o644)
	os.Rename(statusFile()+".tmp", statusFile())
}
