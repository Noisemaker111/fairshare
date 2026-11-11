package main

// Which work uses what. Every look, each process is put under the piece of work it belongs to: a
// thread (an agent runner and everything it started), a run (a command the pool ran here) or a
// job (work in a repo's worktree that no thread on this machine owns). What each piece used is
// what learn.go learns from and what the room rule counts the open work as.
//
// A process belongs to the nearest piece of work above it, and stays there after the processes
// between them are gone. That last part is most of the load: measured on a boat host on
// 2026-11-11, six threads' runners and their living children used 0.05 processors while the
// machine ran at 3.2, because the land a thread starts in the background outlives the shell that
// started it and hangs under nothing. A process found with nothing above it is asked where it
// works: the thread it was started for (T3 puts its id in the environment), else the folder it
// runs in, which names a thread's folder, a worktree named after a thread's, or a repo's worktree
// with no thread here.

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// procNode is one process of a look.
type procNode struct {
	pid, parent uint32
	created     uint64
	name        string
	cores       float64 // processor-seconds it used since the look before
	memGB       float64
	// work says the machine sorts it as agent work: on Windows a process Owl holds at low
	// priority, on a Linux host every process. Only such a process is asked where it works.
	work bool
	// started is when it began, when the machine says; zero otherwise.
	started time.Time
	// total is the processor-seconds it has used since it began (on Linux its ended children's
	// too).
	total float64
}

type procKey struct {
	pid     uint32
	created uint64
}

// where says the folder a process runs in and the T3 thread it was started for, "" when unknown.
type where func(pid uint32, created uint64) (dir, thread string)

// LiveWork is one piece of work open on the machine now.
type LiveWork struct {
	// Kind is "thread", "run" or "job"; What a run's command, or the worktree's name.
	Kind  string    `json:"kind"`
	Repo  string    `json:"repo,omitempty"`
	What  string    `json:"what,omitempty"`
	Since time.Time `json:"since"`
	// Now is the processors it used over the short window; Cores its own average lately, over
	// Minutes of its own measured minutes (recent ones count for more).
	Now     float64 `json:"now"`
	Cores   float64 `json:"cores"`
	Minutes float64 `json:"minutes"`
	// Expect is what it is expected to use: its own minutes, and until it has enough of them,
	// what work like it was measured using. Swing is how far its minutes stray from that.
	Expect float64 `json:"expect"`
	Swing  float64 `json:"swing"`
	// MemGB is the memory it holds now and ExpectMemGB what it is expected to reach.
	MemGB       float64 `json:"mem_gb"`
	ExpectMemGB float64 `json:"expect_mem_gb"`
	Processes   int     `json:"processes"`
}

// runMark is what a run leaves in fairshare's home while it runs, so the meter knows the process
// tree under it is that run, and a machine with no meter still counts it.
type runMark struct {
	PID   uint32    `json:"pid"`
	Repo  string    `json:"repo,omitempty"`
	What  string    `json:"what,omitempty"`
	Label string    `json:"label,omitempty"`
	At    time.Time `json:"at"`
	Need  Need      `json:"need"`
}

func marksDir() string { return filepath.Join(homeDir(), "marks") }

// leaveMark writes this process's mark and returns what removes it.
func leaveMark(m runMark) func() {
	if os.MkdirAll(marksDir(), 0o755) != nil {
		return func() {}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return func() {}
	}
	path := filepath.Join(marksDir(), strconv.FormatUint(uint64(m.PID), 10)+".json")
	if os.WriteFile(path, b, 0o644) != nil {
		return func() {}
	}
	return func() { os.Remove(path) }
}

// readMarks reads the marks of the runs going on now. A mark whose process is gone (a run that
// was killed) is removed: alive says whether a process id is a living fairshare.
func readMarks(alive func(pid uint32) bool) []runMark {
	entries, err := os.ReadDir(marksDir())
	if err != nil {
		return nil
	}
	var out []runMark
	for _, e := range entries {
		path := filepath.Join(marksDir(), e.Name())
		var m runMark
		b, err := os.ReadFile(path)
		if err != nil || json.Unmarshal(b, &m) != nil || m.PID == 0 {
			continue
		}
		if alive != nil && !alive(m.PID) {
			if time.Since(m.At) > 30*time.Second { // a run a moment old may not be in the look yet
				os.Remove(path)
			}
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out
}

func normDir(dir string) string {
	return strings.TrimRight(strings.ToLower(strings.ReplaceAll(dir, `\`, "/")), "/")
}

// worktreeOf names the repo and the worktree a folder is inside, when it is inside an agent
// worktree: …/.t3/worktrees/<repo>/<tree>/… or …/<repo>/.claude/worktrees/<tree>/….
func worktreeOf(dir string) (repo, tree string) {
	parts := strings.Split(normDir(dir), "/")
	for i := 0; i+3 < len(parts); i++ {
		if parts[i] == ".t3" && parts[i+1] == "worktrees" {
			return parts[i+2], parts[i+3]
		}
	}
	for i := 1; i+2 < len(parts); i++ {
		if (parts[i] == ".claude" || parts[i] == ".codex") && parts[i+1] == "worktrees" {
			return parts[i-1], parts[i+2]
		}
	}
	return "", ""
}

// repoOf names the repo a session works in from its folder: a T3 worktree
// (…\.t3\worktrees\<repo>\<branch>), a worktree inside a checkout (<repo>\.claude\worktrees\<x>),
// or the checkout itself (its last folder).
func repoOf(dir string) string {
	parts := strings.FieldsFunc(strings.ToLower(dir), func(r rune) bool { return r == '\\' || r == '/' })
	for i := 0; i+2 < len(parts); i++ {
		if parts[i] == ".t3" && parts[i+1] == "worktrees" {
			return parts[i+2]
		}
	}
	for i := 1; i+1 < len(parts); i++ {
		if (parts[i] == ".claude" || parts[i] == ".codex") && parts[i+1] == "worktrees" {
			return parts[i-1]
		}
	}
	if len(parts) == 0 || (len(parts) == 1 && strings.HasSuffix(parts[0], ":")) {
		return ""
	}
	return parts[len(parts)-1]
}

// isRunner says whether a program is one of the runners, named with or without ".exe".
func isRunner(name string, runners []string) bool {
	name = strings.TrimSuffix(strings.ToLower(name), ".exe")
	for _, r := range runners {
		if name == strings.TrimSuffix(strings.ToLower(r), ".exe") {
			return true
		}
	}
	return false
}

type unitLook struct {
	at           time.Time
	seconds, cpu float64
}

// unit is one piece of work the tracker follows.
type unit struct {
	kind, repo, what string
	// dir is a thread's folder, tree its worktree's name, thread its T3 thread.
	dir, tree, thread string
	// root is the process a thread or a run hangs under: its runner, or the run's fairshare.
	root  procKey
	since time.Time
	// The stretch being measured: processor-seconds, seconds, and the most memory in it.
	cpu, seconds, memPeak float64
	// Its own minutes so far, the recent ones counting for more: minutes, the sum of minutes
	// times processors and times processors squared, and the most memory lately.
	n, sum, sq, memOwn float64
	recent             []unitLook
	mem                float64
	procs              int
	lived              float64
	// fresh says this is the look that found it; total adds up what its processes used so far.
	fresh bool
	total float64
}

// tracker follows every piece of work from look to look.
type tracker struct {
	units   map[int]*unit
	owner   map[procKey]int // the unit a process belongs to; 0 when it was asked and is nobody's
	next    int
	flushed time.Time
}

func (t *tracker) open(u unit) int {
	u.fresh = true
	t.next++
	t.units[t.next] = &u
	return t.next
}

func (t *tracker) ids() []int {
	ids := make([]int, 0, len(t.units))
	for id := range t.units {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

// adopt finds the piece of work a process with nothing above it belongs to, from the thread it
// was started for and the folder it runs in; 0 when it is nobody's.
func (t *tracker) adopt(dir, thread string, since time.Time) int {
	ids := t.ids()
	if thread != "" {
		for _, id := range ids {
			if u := t.units[id]; u.kind == "thread" && u.thread == thread {
				return id
			}
		}
	}
	d := normDir(dir)
	if d == "" {
		return 0
	}
	best := 0
	for _, id := range ids {
		u := t.units[id]
		if u.kind == "thread" && u.dir != "" && (d == u.dir || strings.HasPrefix(d, u.dir+"/")) && (best == 0 || len(u.dir) > len(t.units[best].dir)) {
			best = id
		}
	}
	if best != 0 {
		return best
	}
	repo, tree := worktreeOf(d)
	if repo == "" {
		return 0
	}
	// A worktree named after a thread's worktree is that thread's (a land's ".land-batch-<name>");
	// when two threads' names fit, the longer name, and neither when they are as long.
	tied := false
	for _, id := range ids {
		u := t.units[id]
		if u.kind != "thread" || u.repo != repo || u.tree == "" || !strings.Contains(tree, u.tree) {
			continue
		}
		switch {
		case best == 0 || len(u.tree) > len(t.units[best].tree):
			best, tied = id, false
		case len(u.tree) == len(t.units[best].tree) && u.tree != t.units[best].tree:
			tied = true
		}
	}
	if best != 0 && !tied {
		return best
	}
	for _, id := range ids {
		if u := t.units[id]; u.kind == "job" && u.repo == repo && u.tree == tree {
			return id
		}
	}
	return t.open(unit{kind: "job", repo: repo, what: tree, tree: tree, since: since})
}

// look puts this look's processes under their work and adds up what each piece used. seconds is
// the time since the look before. It returns the stretches to learn from: every piece's last
// minute, once a minute, and the last stretch of each piece that ended.
func (t *tracker) look(now time.Time, seconds float64, nodes []procNode, marks []runMark, find where, s Settings) []stretch {
	if t.units == nil {
		// The first look covers the seconds before it, so the first minute ends a minute after those.
		t.units, t.owner, t.flushed = map[int]*unit{}, map[procKey]int{}, now.Add(-time.Duration(seconds*float64(time.Second)))
	}
	byPID := make(map[uint32]int, len(nodes))
	for i, n := range nodes {
		byPID[n.pid] = i
	}
	markOf := make(map[uint32]runMark, len(marks))
	for _, m := range marks {
		markOf[m.PID] = m
	}
	began := func(n procNode) time.Time {
		if !n.started.IsZero() && n.started.Before(now) {
			return n.started
		}
		return now
	}
	const unknown = -1
	own := make([]int, len(nodes))
	var late []int
	// The roots first, so that a process asked where it works finds its thread already there.
	for i, n := range nodes {
		own[i] = unknown
		k := procKey{n.pid, n.created}
		m, marked := markOf[n.pid]
		marked = marked && strings.HasPrefix(strings.ToLower(n.name), "fairshare")
		if id, known := t.owner[k]; known {
			if id > 0 && t.units[id] != nil && (!marked || t.units[id].root == k) {
				own[i] = id
				continue
			}
			if marked {
				// A look caught the run before it had left its mark, and put it and what it had
				// started under whatever was above it. It is a run from here on, and so are they.
				late = append(late, i)
			}
		}
		if marked {
			own[i] = t.open(unit{kind: "run", repo: strings.ToLower(strings.TrimSpace(m.Repo)), what: m.What, root: k, since: began(n)})
			t.owner[k] = own[i]
		} else if isRunner(n.name, s.Runners) {
			dir, thread := "", ""
			if find != nil {
				dir, thread = find(n.pid, n.created)
			}
			_, tree := worktreeOf(dir)
			if tree == "" {
				tree = repoOf(dir)
			}
			own[i] = t.open(unit{kind: "thread", repo: repoOf(dir), what: tree, dir: normDir(dir), tree: tree, thread: thread, root: k, since: began(n)})
			t.owner[k] = own[i]
		}
	}
	if len(late) > 0 {
		kids := map[uint32][]int{}
		for i, n := range nodes {
			kids[n.parent] = append(kids[n.parent], i)
		}
		for len(late) > 0 {
			at := late[0]
			late = late[1:]
			for _, i := range kids[nodes[at].pid] {
				k := procKey{nodes[i].pid, nodes[i].created}
				if id := t.owner[k]; i == at || nodes[i].created < nodes[at].created || (id > 0 && t.units[id] != nil && t.units[id].root == k) {
					continue // not its child, or a piece of work of its own
				}
				delete(t.owner, k)
				own[i] = unknown
				late = append(late, i)
			}
		}
	}
	var resolve func(i, depth int) int
	resolve = func(i, depth int) int {
		if own[i] != unknown {
			return own[i]
		}
		own[i] = 0 // while it is being worked out, so a loop of parents ends
		n := nodes[i]
		k := procKey{n.pid, n.created}
		if id, ok := t.owner[k]; ok && (id == 0 || t.units[id] != nil) {
			own[i] = id
			return id
		}
		id := 0
		// A parent counts only when it started before its child: process ids are reused.
		if j, ok := byPID[n.parent]; ok && j != i && depth < 64 && nodes[j].created <= n.created {
			id = resolve(j, depth+1)
		}
		if id != 0 {
			t.owner[k] = id
		} else if n.work && find != nil {
			dir, thread := find(n.pid, n.created)
			id = t.adopt(dir, thread, began(n))
			t.owner[k] = id // also when it is nobody's: asking is not free
		}
		own[i] = id
		return id
	}
	used := map[int]float64{}
	for _, u := range t.units {
		u.mem, u.procs = 0, 0
	}
	present := make(map[procKey]int, len(nodes))
	for i, n := range nodes {
		id := resolve(i, 0)
		k := procKey{n.pid, n.created}
		if v, ok := t.owner[k]; ok {
			present[k] = v
		}
		if id <= 0 {
			continue
		}
		u := t.units[id]
		u.cpu += n.cores
		u.mem += n.memGB
		u.procs++
		if u.fresh {
			u.total += n.total
		}
		used[id] += n.cores
	}
	t.owner = present
	// A piece of work found long after it began (the meter started, or restarted, under it) is
	// not unknown: what its processes have used since they began, over how long that is, stands
	// in for the minutes the meter did not see. It counts for no more than learn_recent_minutes,
	// so the minutes it does see replace it.
	for _, u := range t.units {
		if !u.fresh {
			continue
		}
		u.fresh = false
		if age := now.Sub(u.since).Minutes(); age >= 2 && u.total > 0 {
			mean := u.total / (age * 60)
			u.n = math.Min(age, s.LearnRecentMinutes)
			u.sum, u.sq, u.memOwn = u.n*mean, u.n*mean*mean, u.mem
		}
	}

	var out []stretch
	minute := now.Sub(t.flushed) >= time.Minute
	for _, id := range t.ids() {
		u := t.units[id]
		if u.procs > 0 {
			u.seconds += seconds
			u.lived += seconds
			u.memPeak = math.Max(u.memPeak, u.mem)
			u.recent = append(u.recent, unitLook{now, seconds, used[id]})
			for len(u.recent) > 1 && now.Sub(u.recent[0].at) >= time.Minute {
				u.recent = u.recent[1:]
			}
		}
		ended := u.procs == 0
		if !ended && !minute {
			continue
		}
		sp := stretch{Kind: u.kind, Repo: u.repo, What: u.what, Ended: ended, Seconds: u.lived, Began: u.since}
		if u.kind != "run" {
			sp.What = ""
		}
		if u.seconds > 0 {
			sp.Minutes, sp.Cores, sp.MemGB = u.seconds/60, u.cpu/u.seconds, u.memPeak
			f := math.Pow(0.5, sp.Minutes/s.LearnRecentMinutes)
			u.n, u.sum, u.sq = u.n*f+sp.Minutes, u.sum*f+sp.Minutes*sp.Cores, u.sq*f+sp.Minutes*sp.Cores*sp.Cores
			u.memOwn = math.Max(u.memOwn*f, u.memPeak)
			u.cpu, u.seconds, u.memPeak = 0, 0, 0
		}
		if sp.Minutes > 0 || ended {
			out = append(out, sp)
		}
		if ended {
			delete(t.units, id)
		}
	}
	if minute {
		t.flushed = now
	}
	return out
}

// live is the work open now, the piece expected to use the most first.
func (t *tracker) live(now time.Time, l learned, s Settings) []LiveWork {
	out := []LiveWork{}
	for _, id := range t.ids() {
		u := t.units[id]
		what := u.what
		if u.kind != "run" {
			what = ""
		}
		prior := size(Need{Cores: s.ThreadCores, MemGB: s.ThreadMemGB}, l.levels(u.kind, u.repo, what, nil, now, s), s.LearnTrustMinutes)
		w := u.n / (u.n + s.LearnTrustMinutes)
		var mean, meanSq float64
		if u.n > 0 {
			mean, meanSq = u.sum/u.n, u.sq/u.n
		}
		lw := LiveWork{Kind: u.kind, Repo: u.repo, What: u.what, Since: u.since.UTC(), Cores: round2(mean), Minutes: round2(u.n), MemGB: round2(u.mem), Processes: u.procs}
		lw.Expect = w*mean + (1-w)*prior.Cores
		lw.Swing = round2(spread(lw.Expect, w*meanSq+(1-w)*(prior.Swing*prior.Swing+prior.Cores*prior.Cores)))
		lw.Expect = round2(lw.Expect)
		lw.ExpectMemGB = round2(math.Max(u.mem, w*u.memOwn+(1-w)*prior.MemGB))
		var cpu, secs float64
		for _, k := range u.recent {
			cpu, secs = cpu+k.cpu, secs+k.seconds
		}
		if secs > 0 {
			lw.Now = round2(cpu / secs)
		}
		out = append(out, lw)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Expect > out[j].Expect })
	return out
}

// learn records the stretches a look handed over, and ends the grants of the threads and jobs
// that ended (a run ends its own, in `pool here`). It returns what is known now.
func learn(learnt []stretch, now time.Time, s Settings) learned {
	known := loadLearned()
	known.forget(now, s)
	for _, sp := range learnt {
		known.add(now, sp, s)
		if sp.Ended && sp.Kind != "run" {
			if err := endGrant(sp.Kind, sp.Repo, sp.Began, settle(s)); err != nil {
				fmt.Fprintln(os.Stderr, "degraded:", err)
			}
		}
	}
	if err := saveLearned(known); err != nil {
		fmt.Fprintln(os.Stderr, "degraded: what was learned was not saved:", err)
	}
	return known
}

// liveFromMarks is the runs going on, counted at what each was let in as: what a machine whose
// meter is not running still knows about its open work.
func liveFromMarks(marks []runMark) []LiveWork {
	var out []LiveWork
	for _, m := range marks {
		out = append(out, LiveWork{Kind: "run", Repo: strings.ToLower(m.Repo), What: m.What, Since: m.At.UTC(), Expect: m.Need.Cores, ExpectMemGB: m.Need.MemGB, Processes: 1})
	}
	return out
}

// openWork adds the open work up: what it uses over the short window, what it is expected to use,
// its swing squared (the pieces swing apart from each other, so their squares add), and the
// memory it is expected to reach beyond what it holds now.
func openWork(work []LiveWork) (now, expect, swingSq, memOwed float64) {
	for _, w := range work {
		now += w.Now
		expect += w.Expect
		swingSq += w.Swing * w.Swing
		memOwed += math.Max(0, w.ExpectMemGB-w.MemGB)
	}
	return now, expect, swingSq, memOwed
}

func workWords(w LiveWork) string {
	name := w.Repo
	if name == "" {
		name = "no repo"
	}
	switch w.Kind {
	case "run":
		return fmt.Sprintf("run in %s: %s", name, w.What)
	case "job":
		return fmt.Sprintf("job in %s (%s)", name, w.What)
	}
	return fmt.Sprintf("thread in %s (%s)", name, w.What)
}
