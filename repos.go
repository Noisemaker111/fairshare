package main

// What a thread of each repo takes, measured. A thread's estimate was the machine's average per
// open thread, which on 2026-11-11 was 4.5 processors, learned from Owl threads compiling Rust;
// an AppForGutters thread mostly waits on its model and uses about one. Now the meter adds up, each
// minute, what each agent session's process tree used, under the repo its runner works in, and a
// new thread of a repo with enough measured minutes counts as that repo's own average.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

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

// repoDay is one repo's measured minutes on one day: Minutes is session-minutes (two sessions for
// one minute is two), Cores and MemGB their sums.
type repoDay struct {
	Minutes float64 `json:"minutes"`
	Cores   float64 `json:"cores"`
	MemGB   float64 `json:"mem_gb"`
}

// repoHistory is repo -> day (2006-01-02) -> what was measured.
type repoHistory map[string]map[string]repoDay

func reposFile() string { return filepath.Join(homeDir(), "repos.json") }

func loadRepos() repoHistory {
	h := repoHistory{}
	if b, err := os.ReadFile(reposFile()); err == nil {
		json.Unmarshal(b, &h)
	}
	return h
}

func saveRepos(h repoHistory) error {
	if err := os.MkdirAll(homeDir(), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	tmp := reposFile() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, reposFile())
}

// sessionUse is one agent session's use over one span: its repo, processors and memory.
type sessionUse struct {
	Repo  string
	Cores float64
	MemGB float64
}

// add records one span of minutes for the sessions measured, and forgets days older than keep.
func (h repoHistory) add(now time.Time, minutes float64, sessions []sessionUse, keep int) {
	day := now.Format("2006-01-02")
	for _, u := range sessions {
		if u.Repo == "" {
			continue
		}
		if h[u.Repo] == nil {
			h[u.Repo] = map[string]repoDay{}
		}
		d := h[u.Repo][day]
		d.Minutes += minutes
		d.Cores += u.Cores * minutes
		d.MemGB += u.MemGB * minutes
		h[u.Repo][day] = d
	}
	oldest := now.AddDate(0, 0, -keep).Format("2006-01-02")
	for repo, days := range h {
		for d := range days {
			if d < oldest {
				delete(days, d)
			}
		}
		if len(days) == 0 {
			delete(h, repo)
		}
	}
}

// repoNeed is what a session of one repo was measured using.
type repoNeed struct {
	Repo    string  `json:"repo"`
	Cores   float64 `json:"cores"`
	MemGB   float64 `json:"mem_gb"`
	Minutes float64 `json:"minutes"`
}

// need is a repo's average per session over the last repo_days, when it has repo_min_minutes of
// them; ok is false otherwise, and the machine's average stands.
func (h repoHistory) need(repo string, now time.Time, s Settings) (repoNeed, bool) {
	repo = strings.ToLower(strings.TrimSpace(repo))
	n := repoNeed{Repo: repo}
	if repo == "" {
		return n, false
	}
	oldest := now.AddDate(0, 0, -int(s.RepoDays)).Format("2006-01-02")
	var cores, mem float64
	for d, v := range h[repo] {
		if d < oldest {
			continue
		}
		n.Minutes += v.Minutes
		cores += v.Cores
		mem += v.MemGB
	}
	if n.Minutes < s.RepoMinMinutes {
		return n, false
	}
	n.Cores, n.MemGB = round2(cores/n.Minutes), round2(mem/n.Minutes)
	return n, true
}

// table lists every repo's average, busiest first, for `fairshare repos`.
func (h repoHistory) table(now time.Time, s Settings) []repoNeed {
	var out []repoNeed
	for repo := range h {
		n, _ := h.need(repo, now, s)
		if n.Minutes > 0 && n.Cores == 0 {
			var cores, mem float64
			for _, v := range h[repo] {
				cores, mem = cores+v.Cores, mem+v.MemGB
			}
			n.Cores, n.MemGB = round2(cores/n.Minutes), round2(mem/n.Minutes)
		}
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Cores != out[j].Cores {
			return out[i].Cores > out[j].Cores
		}
		return out[i].Repo < out[j].Repo
	})
	return out
}

// procNode is one process of a look, for finding the session it belongs to.
type procNode struct {
	pid, parent uint32
	created     uint64
	name        string
	cores       float64
	memGB       float64
}

// sessionOf finds, for every process, the agent runner above it (a process named in runners), by
// its parent chain. A parent counts only when it started before its child, since Windows reuses
// process ids. It returns the runner's index in procs for each index, -1 for none.
func sessionOf(procs []procNode, runners []string) []int {
	byPID := make(map[uint32]int, len(procs))
	for i, p := range procs {
		byPID[p.pid] = i
	}
	isRunner := func(name string) bool {
		for _, r := range runners {
			if strings.EqualFold(name, r) {
				return true
			}
		}
		return false
	}
	out := make([]int, len(procs))
	for i := range out {
		out[i] = -2
	}
	var find func(i, depth int) int
	find = func(i, depth int) int {
		if out[i] != -2 {
			return out[i]
		}
		if isRunner(procs[i].name) {
			out[i] = i
			return i
		}
		got := -1
		if j, ok := byPID[procs[i].parent]; ok && j != i && depth < 64 && procs[j].created <= procs[i].created {
			got = find(j, depth+1)
		}
		out[i] = got
		return got
	}
	for i := range procs {
		find(i, 0)
	}
	return out
}

func repoWords(n repoNeed) string {
	return fmt.Sprintf("%s sessions were measured at %.1f processors and %.1f GB over %.0f session-minutes", n.Repo, n.Cores, n.MemGB, n.Minutes)
}
