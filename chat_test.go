package main

import "testing"

// The process layout seen on this PC: T3 Code's main process, its server, the sessions the server
// starts (any provider) and T3's own helpers, plus an agent started from a terminal.
func TestChatRule(t *testing.T) {
	p := func(ppid uint32, name string, created uint64) procInfo {
		return procInfo{ppid: ppid, name: name, created: created}
	}
	procs := map[uint32]procInfo{
		4:   p(0, "system", 1),
		100: p(4, "explorer.exe", 10),
		200: p(100, "t3 code (nightly).exe", 20),  // T3 main
		201: p(200, "t3 code (nightly).exe", 21),  // renderer
		210: p(200, "t3 code (nightly).exe", 22),  // T3 server
		220: p(210, "t3-resource-monitor.exe", 30), // T3's own helper: a chat with no work
		300: p(210, "claude.exe", 40),               // a Claude session
		301: p(300, "cmd.exe", 41),
		302: p(301, "pwsh.exe", 42),
		303: p(302, "claude.exe", 43), // `claude -p` run by that chat: still its work
		304: p(303, "node.exe", 44),
		400: p(210, "someagent.exe", 50), // a provider fairshare has never heard of
		401: p(400, "go.exe", 51),
		500: p(100, "windowsterminal.exe", 60),
		501: p(500, "pwsh.exe", 61),
		502: p(501, "codex.exe", 62), // an agent started from a terminal
		503: p(502, "cargo.exe", 63),
		600: p(100, "notepad.exe", 70),
		700: p(999, "orphan.exe", 80), // parent gone
		800: p(300, "old.exe", 35),    // started before 300: 300 is a reused ID, not its parent
	}
	r := newChatRule(defaultHosts, defaultHarness)
	for pid, want := range map[uint32]uint32{
		301: 300, 302: 300, 303: 300, 304: 300,
		401: 400,
		503: 502,
		300: 0, 220: 0, 201: 0, 210: 0, 200: 0, 502: 0, 501: 0, 600: 0, 700: 0, 800: 0,
	} {
		if got := r.of(pid, procs); got != want {
			t.Errorf("chat of %d %s = %d, want %d", pid, procs[pid].name, got, want)
		}
	}
	r.only = map[uint32]bool{302: true}
	if got := r.of(304, procs); got != 302 {
		t.Errorf("--pid 302: chat of 304 = %d", got)
	}
}
