package main

// Threads let in a moment ago. Their load does not show in the measurements yet, so without this
// ten launches in one minute would each see the same free machine. Each stays counted at its
// estimate for the settle time, then the measurements speak for it. Every answer is also written
// to decisions.jsonl, so "why was that launch refused" has a record.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

type grant struct {
	At    time.Time `json:"at"`
	Need  Need      `json:"need"`
	Label string    `json:"label,omitempty"`
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

// pendingNeed is what the threads let in during the settle time count as.
func pendingNeed(now time.Time, settle time.Duration) Need {
	var n Need
	for _, g := range readGrants(now, settle) {
		n = n.plus(g.Need)
	}
	return n
}

// pendingSinceNeed is what the threads let in during the settle time, and no earlier than since,
// count as: under an urgent hold, the threads let in before it began step aside with the rest.
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
func takeGrant(now time.Time, settle time.Duration, need Need, label string) error {
	if err := os.MkdirAll(homeDir(), 0o755); err != nil {
		return err
	}
	live := append(readGrants(now, settle), grant{At: now, Need: need, Label: label})
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
