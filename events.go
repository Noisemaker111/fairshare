package main

// The event log: one JSON line per tool call, run and piece of agent work, appended with a single
// write so many processes can share the file. It costs one small append per event and is the data
// `fairshare report` reads.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const eventsRotateBytes = 64 << 20

func eventsDir() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "fairshare")
}

func eventsFile() string {
	if p := os.Getenv("FAIRSHARE_EVENTS"); p != "" {
		return p
	}
	return filepath.Join(eventsDir(), "events.jsonl")
}

// logEvent appends ev (with the time and kind added) to the event log. It never fails the caller.
func logEvent(kind string, ev map[string]any) {
	ev["ts"] = time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	ev["ev"] = kind
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	path := eventsFile()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		os.MkdirAll(filepath.Dir(path), 0o755)
		if f, err = os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err != nil {
			return
		}
	}
	f.Write(append(b, '\n'))
	st, err := f.Stat()
	f.Close()
	if err == nil && st.Size() > eventsRotateBytes {
		os.Rename(path, strings.TrimSuffix(path, ".jsonl")+"-"+time.Now().Format("20060102-150405")+".jsonl")
	}
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
