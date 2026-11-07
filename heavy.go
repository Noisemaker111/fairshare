package main

// The heavy list: the commands that have been measured loading the PC, one entry per repo and
// command, in %LOCALAPPDATA%\fairshare\heavy.json. Only a command on it waits for a heavy-job slot
// (slot.go). Nothing is guessed from a command's name: every run that `exec` or `slot` wraps is
// measured, and a command joins the list the first time one run of it uses heavyCPUSeconds. The
// file is meant to be read and edited by hand; deleting an entry takes the command off.
//
// The repo is the repo's identity (its origin URL, or its top-level folder name when it has none),
// never the worktree path, so every worktree of one repo shares one entry. The command is its text
// with whitespace collapsed and quote marks dropped, so `exec` (which sees the agent's text) and
// `slot` (which sees the words the shell made of it) agree on one key.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Noisemaker111/fairshare/internal/windows"
)

// heavyCPUSeconds is the CPU one run must use to put its command on the list: 2 core-minutes. On
// 2026-11-06 the real heavies used 280-2,600 CPU-s (go test ./..., cargo test, verify:smoke) and
// the commands wrongly queued behind them (vite build, tsc --noEmit, biome check) used seconds.
const heavyCPUSeconds = 120

type heavyEntry struct {
	Repo        string  `json:"repo"`
	Cmd         string  `json:"cmd"`
	CPUSeconds  float64 `json:"cpu_s"`   // the latest run
	WallSeconds float64 `json:"wall_s"`  // the latest run
	PeakMB      float64 `json:"peak_mb"` // the latest run
	FirstSeen   string  `json:"first_seen"`
	LastSeen    string  `json:"last_seen"`
	Runs        int     `json:"runs"` // runs since it joined the list, that one included
}

func heavyFile() string { return filepath.Join(eventsDir(), "heavy.json") }

// heavyKey is the command part of an entry's key.
func heavyKey(cmd string) string {
	return strings.Join(strings.Fields(strings.NewReplacer(`"`, "", "'", "").Replace(cmd)), " ")
}

// readHeavy reads the list. A missing file is an empty list; a file that does not parse (a hand
// edit gone wrong) is an error, and nothing is written over it.
func readHeavy() ([]heavyEntry, error) {
	b, err := os.ReadFile(heavyFile())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var list []heavyEntry
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("%s does not parse: %w", heavyFile(), err)
	}
	return list, nil
}

// heavyListed finds the entry for a command run in dir. A command its repository says takes its
// own slot (ownslot.go) is never listed: it waits for a slot around its local steps itself.
func heavyListed(dir, cmd string) (heavyEntry, bool) {
	if ownSlot(cmd, dir) {
		return heavyEntry{}, false
	}
	list, err := readHeavy()
	if err != nil {
		logEvent("error", map[string]any{"where": "heavy", "err": err.Error()})
		return heavyEntry{}, false
	}
	repo, key := repoID(dir), heavyKey(cmd)
	for _, e := range list {
		if e.Repo == repo && e.Cmd == key {
			return e, true
		}
	}
	return heavyEntry{}, false
}

// heavyRun is one measured run.
type heavyRun struct {
	Dir, Cmd    string
	CPUSeconds  float64
	WallSeconds float64
	PeakMB      float64
	At          time.Time
}

// recordHeavy notes a measured run: a listed command's entry is brought up to date, and an
// unlisted one joins when it used heavyCPUSeconds. It says whether the command joined now.
func recordHeavy(r heavyRun) bool {
	if ownSlot(r.Cmd, r.Dir) {
		return false // its CPU is its own slotted steps', which are measured and listed themselves
	}
	if r.CPUSeconds < heavyCPUSeconds {
		if _, listed := heavyListed(r.Dir, r.Cmd); !listed {
			return false // the common case: one read, no write
		}
	}
	added := false
	err := editHeavy(func(list []heavyEntry) []heavyEntry {
		var a bool
		list, a = noteRun(list, repoID(r.Dir), r)
		added = a
		return list
	})
	if err != nil {
		logEvent("error", map[string]any{"where": "heavy", "err": err.Error()})
		return false
	}
	if added {
		logEvent("heavy", map[string]any{"repo": repoID(r.Dir), "cmd": clip(heavyKey(r.Cmd), 300), "cpu_ms": int64(r.CPUSeconds * 1000), "wall_ms": int64(r.WallSeconds * 1000)})
	}
	return added
}

// noteRun applies one run to the list (no file access).
func noteRun(list []heavyEntry, repo string, r heavyRun) ([]heavyEntry, bool) {
	key, at := heavyKey(r.Cmd), r.At.UTC().Format(time.RFC3339)
	for i := range list {
		if e := &list[i]; e.Repo == repo && e.Cmd == key {
			e.CPUSeconds, e.WallSeconds, e.PeakMB = round1(r.CPUSeconds), round1(r.WallSeconds), round1(r.PeakMB)
			e.LastSeen = at
			e.Runs++
			return list, false
		}
	}
	if r.CPUSeconds < heavyCPUSeconds {
		return list, false
	}
	return append(list, heavyEntry{Repo: repo, Cmd: key, CPUSeconds: round1(r.CPUSeconds), WallSeconds: round1(r.WallSeconds), PeakMB: round1(r.PeakMB), FirstSeen: at, LastSeen: at, Runs: 1}), true
}

// editHeavy changes the list under a lock, and replaces the file whole so a reader never sees
// half of it.
func editHeavy(change func([]heavyEntry) []heavyEntry) error {
	if err := os.MkdirAll(eventsDir(), 0o755); err != nil {
		return err
	}
	lock, err := lockFile(filepath.Join(eventsDir(), "heavy.lock"), true)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(lock)
	list, err := readHeavy()
	if err != nil {
		return err
	}
	list = change(list)
	if list == nil {
		list = []heavyEntry{} // "[]", which reads better by hand than "null"
	}
	slices.SortFunc(list, func(a, b heavyEntry) int { return strings.Compare(a.Repo+"\x00"+a.Cmd, b.Repo+"\x00"+b.Cmd) })
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := heavyFile() + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, heavyFile())
}

// repoID is the identity of the repo that holds dir: its origin URL (host/owner/name, lower
// case), or the repo's top-level folder name when it has no origin. Outside any repo it is the
// folder's own name.
func repoID(dir string) string {
	dir = filepath.Clean(dir)
	for d := dir; ; d = filepath.Dir(d) {
		if id, ok := repoAt(d); ok {
			return id
		}
		if filepath.Dir(d) == d {
			return strings.ToLower(filepath.Base(dir))
		}
	}
}

// repoAt is the identity of the repo whose top level is d, if d is one.
func repoAt(d string) (string, bool) {
	dotgit := filepath.Join(d, ".git")
	st, err := os.Stat(dotgit)
	if err != nil {
		return "", false
	}
	gitDir := dotgit
	if !st.IsDir() { // a worktree: "gitdir: <main>/.git/worktrees/<name>", whose commondir is the main .git
		b, err := os.ReadFile(dotgit)
		if err != nil {
			return "", false
		}
		gitDir = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(b)), "gitdir:"))
		if !filepath.IsAbs(gitDir) {
			gitDir = filepath.Join(d, gitDir)
		}
		if c, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
			common := strings.TrimSpace(string(c))
			if !filepath.IsAbs(common) {
				common = filepath.Join(gitDir, common)
			}
			gitDir = filepath.Clean(common)
		}
	}
	if url := originURL(filepath.Join(gitDir, "config")); url != "" {
		return url, true
	}
	top := d
	if filepath.Base(gitDir) == ".git" { // a worktree's repo is named after its main checkout
		top = filepath.Dir(gitDir)
	}
	return strings.ToLower(filepath.Base(top)), true
}

// originURL reads [remote "origin"] url from a git config and normalizes it, so the https and ssh
// forms of one repo are the same identity.
func originURL(config string) string {
	f, err := os.Open(config)
	if err != nil {
		return ""
	}
	defer f.Close()
	in := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			in = line == `[remote "origin"]`
			continue
		}
		if k, v, ok := strings.Cut(line, "="); in && ok && strings.TrimSpace(k) == "url" {
			return normalizeRemote(strings.TrimSpace(v))
		}
	}
	return ""
}

func normalizeRemote(u string) string {
	u = strings.ToLower(strings.TrimSpace(u))
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
	} else if at := strings.Index(u, "@"); at >= 0 && strings.Contains(u[at:], ":") { // git@host:owner/name
		u = strings.Replace(u[at+1:], ":", "/", 1)
	}
	if at := strings.LastIndex(strings.SplitN(u, "/", 2)[0], "@"); at >= 0 { // user@ in a URL
		u = u[at+1:]
	}
	u = strings.TrimSuffix(strings.TrimSuffix(u, "/"), ".git")
	return strings.ReplaceAll(u, `\`, "/")
}

// repoIDOfGone is repoID for a folder that may no longer exist (an old run's worktree): it asks the
// nearest folder above that still does, and when that is not in a repo, a repo beside the gone
// folder (worktrees of one repo live side by side).
func repoIDOfGone(dir string) string {
	dir = filepath.Clean(dir)
	if exists(dir) {
		return repoID(dir)
	}
	d := dir
	for !exists(d) && filepath.Dir(d) != d {
		d = filepath.Dir(d)
	}
	for up := d; ; up = filepath.Dir(up) {
		if id, ok := repoAt(up); ok {
			return id
		}
		if filepath.Dir(up) == up {
			break
		}
	}
	entries, _ := os.ReadDir(d)
	for _, e := range entries[:min(len(entries), 50)] {
		if e.IsDir() {
			if id, ok := repoAt(filepath.Join(d, e.Name())); ok {
				return id
			}
		}
	}
	return strings.ToLower(filepath.Base(dir))
}

// seedHeavy builds the list from the runs already measured in the event log, once: runs measured
// before the list existed count as found out. It does nothing when the list already exists.
func seedHeavy() (int, error) {
	if exists(heavyFile()) {
		return 0, nil
	}
	ids := map[string]string{}
	var list []heavyEntry
	for _, e := range readEvents(time.Time{}) {
		if e.str("ev") != "run" || e["cpu_ms"] == nil || e.str("cwd") == "" {
			continue
		}
		cwd := e.str("cwd")
		id, ok := ids[cwd]
		if !ok {
			id = repoIDOfGone(cwd)
			ids[cwd] = id
		}
		at, err := time.Parse("2006-01-02T15:04:05.000Z", e.str("ts"))
		if err != nil {
			continue
		}
		list, _ = noteRun(list, id, heavyRun{Cmd: e.str("cmd"), CPUSeconds: e.num("cpu_ms") / 1000, WallSeconds: e.num("wall_ms") / 1000, PeakMB: e.num("peak_mb"), At: at})
	}
	n := len(list)
	return n, editHeavy(func(cur []heavyEntry) []heavyEntry {
		if len(cur) > 0 { // someone wrote it meanwhile; theirs stands
			n = 0
			return cur
		}
		return list
	})
}

// heavyNote is what a run on the list tells the agent about why it waits.
func heavyNote(e heavyEntry) string {
	day := e.FirstSeen
	if len(day) >= 10 {
		day = day[:10]
	}
	return fmt.Sprintf("it is on the heavy list (%s: %.0f CPU-seconds on its last run, listed %s)", heavyFile(), e.CPUSeconds, day)
}

// boatHint points a waiting or refused check at a cloud Linux machine (the boat skill), through a
// boat lease so it keeps to the PC's two machines, and with --no-env so no stored login goes along.
func boatHint(cmd string) string {
	return fmt.Sprintf(" If this check runs on Linux, it can run on a boat.dev cloud machine instead: `fairshare lease boat -- ~/.ascii/bin/boat.exe run --no-env --timeout 1200 --push . -- %s` (see the boat skill).", strings.TrimSpace(cmd))
}
