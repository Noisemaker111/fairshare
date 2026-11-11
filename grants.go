package main

// Work let in a moment ago. It is not running yet, so without this ten launches in one minute
// would each see the same free machine. Each stays counted at its estimate until the meter sees
// it running (a new piece of work of its kind and repo, begun since it was let in), and from then
// it counts as itself, as open work. Work that never shows up (a launch that failed) stops
// counting after the settle time, and on a machine nothing measures, that time is all there is.
// Every answer is also written to decisions.jsonl, so "why was that launch refused" has a record.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type grant struct {
	At    time.Time `json:"at"`
	Need  Need      `json:"need"`
	Label string    `json:"label,omitempty"`
	// Kind and Repo are what was let in ("thread" when unset) and Count how many pieces Need is.
	Kind  string `json:"kind,omitempty"`
	Repo  string `json:"repo,omitempty"`
	Count int    `json:"count,omitempty"`
}

// unmet is the part of the grants whose work is not running yet. Each piece of open work that
// began since a grant, of the grant's kind and repo, is one of its pieces arrived; the oldest
// grant is matched first, and no piece of work answers for two grants.
func unmet(grants []grant, work []LiveWork) []grant {
	taken := make([]bool, len(work))
	var out []grant
	for _, g := range grants {
		kind, pieces := g.Kind, max(g.Count, 1)
		if kind == "" {
			kind = "thread"
		}
		arrived := 0
		for i, w := range work {
			if arrived == pieces {
				break
			}
			// A few seconds of slack: the two clocks are the meter's and the launch's.
			if taken[i] || w.Kind != kind || (g.Repo != "" && w.Repo != strings.ToLower(g.Repo)) || w.Since.Before(g.At.Add(-10*time.Second)) {
				continue
			}
			taken[i] = true
			arrived++
		}
		if arrived == pieces {
			continue
		}
		left := float64(pieces-arrived) / float64(pieces)
		g.Need = Need{g.Need.Cores * left, g.Need.MemGB * left, g.Need.DiskGB * left}
		out = append(out, g)
	}
	return out
}

func grantsFile() string { return filepath.Join(homeDir(), "grants.json") }

func readGrants(now time.Time, settle time.Duration) []grant {
	var all, live []grant
	b, err := os.ReadFile(grantsFile())
	if err != nil || json.Unmarshal(b, &all) != nil {
		return nil
	}
	for _, g := range all {
		if now.Sub(g.At) < settle && !g.At.After(now.Add(time.Minute)) {
			live = append(live, g)
		}
	}
	return live
}

// pendingNeed is what the work let in during the settle time and not yet seen running counts as.
// work is the machine's open work.
func pendingNeed(now time.Time, settle time.Duration, work []LiveWork) Need {
	var n Need
	for _, g := range unmet(readGrants(now, settle), work) {
		n = n.plus(g.Need)
	}
	return n
}

// pendingSinceNeed is what the threads let in during the settle time, and no earlier than since,
// count as: under an urgent hold, the threads let in before it began step aside with the rest.
// The open work steps aside too, so here a thread counts for the whole settle time.
func pendingSinceNeed(now time.Time, settle time.Duration, since time.Time) Need {
	var n Need
	for _, g := range readGrants(now, settle) {
		if !g.At.Before(since) {
			n = n.plus(g.Need)
		}
	}
	return n
}

// takeGrant records work that was let in. Two launches in the same instant can each miss the
// other's record; the next look still sees both.
func takeGrant(now time.Time, settle time.Duration, g grant) error {
	if err := os.MkdirAll(homeDir(), 0o755); err != nil {
		return err
	}
	g.At = now
	live := append(readGrants(now, settle), g)
	b, err := json.Marshal(live)
	if err != nil {
		return err
	}
	tmp := grantsFile() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, grantsFile())
}

// endGrant ends the grant of a piece of work that has itself ended, so work that came and went
// inside the settle time does not go on counting as if it were still to come. began is when the
// work began: the oldest grant of its kind and repo from before then is the one it was let in on.
func endGrant(kind, repo string, began time.Time, settle time.Duration) error {
	return withLock(func() error {
		now := time.Now()
		live := readGrants(now, settle)
		for i, g := range live {
			k := g.Kind
			if k == "" {
				k = "thread"
			}
			if k != kind || (g.Repo != "" && !strings.EqualFold(g.Repo, repo)) || g.At.After(began.Add(10*time.Second)) {
				continue
			}
			if g.Count > 1 {
				left := float64(g.Count-1) / float64(g.Count)
				live[i].Need, live[i].Count = Need{g.Need.Cores * left, g.Need.MemGB * left, g.Need.DiskGB * left}, g.Count-1
			} else {
				live = append(live[:i], live[i+1:]...)
			}
			if live == nil {
				live = []grant{}
			}
			b, err := json.Marshal(live)
			if err != nil {
				return err
			}
			tmp := grantsFile() + ".tmp"
			if err := os.WriteFile(tmp, b, 0o644); err != nil {
				return err
			}
			return os.Rename(tmp, grantsFile())
		}
		return nil
	})
}

// appendLine adds one JSON line to a log under fairshare's home, starting a new file once the old
// one passes maxBytes (the old one is kept as <name>.1).
func appendLine(name string, v any, maxBytes int64) error {
	if err := os.MkdirAll(homeDir(), 0o755); err != nil {
		return err
	}
	path := filepath.Join(homeDir(), name)
	if st, err := os.Stat(path); err == nil && st.Size() > maxBytes {
		os.Remove(path + ".1")
		os.Rename(path, path+".1")
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}
