package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The hook starts on every agent tool call, so every package init is paid on every call. net's
// init starts Winsock (3 ms) though fairshare opens no socket; golang.org/x/sys/windows pulled it
// in, which is why Windows calls go through internal/windows.
func TestNoNetInBinary(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Skip("go list:", err)
	}
	for _, p := range strings.Fields(string(out)) {
		if p == "net" {
			t.Fatal("fairshare links package net; its init costs every hook call 3 ms. `go list -f '{{.ImportPath}}: {{.Imports}}' -deps .` shows who imports it")
		}
	}
}

// Each logged call carries what it cost since Windows created the process (proc_ms) besides the
// hook's own work (hook_ms): the start is most of a call, and only the hook can see it.
func TestHookLogsProcessStartCost(t *testing.T) {
	events := filepath.Join(t.TempDir(), "events.jsonl")
	c := exec.Command(fairshareBin(t), "hook", "claude")
	c.Stdin = strings.NewReader(`{"tool_name":"Bash","tool_input":{"command":"true"},"cwd":"C:/"}`)
	c.Env = append(os.Environ(), "FAIRSHARE_EVENTS="+events)
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	b, _ := os.ReadFile(events)
	var ev struct {
		HookMs *int64 `json:"hook_ms"`
		ProcMs *int64 `json:"proc_ms"`
	}
	if err := json.Unmarshal(b, &ev); err != nil || ev.HookMs == nil || ev.ProcMs == nil || *ev.ProcMs <= 0 || *ev.ProcMs < *ev.HookMs {
		t.Fatalf("event %s: hook_ms %v proc_ms %v", b, ev.HookMs, ev.ProcMs)
	}
}
