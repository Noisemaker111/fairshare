package main

// Which chat a process belongs to. One rule for adopt (whose lane it goes in) and the hook (which
// chat a call came from):
//
//   - Under T3 Code, a chat is a direct child of a T3 Code process: T3 starts each session (Claude
//     Code, Codex, OpenCode or anything else) as its own child. So every provider is covered without
//     knowing its program name, and T3's own processes (its windows, the resource monitor, the
//     tunnel) are never anyone's work.
//   - Anywhere else, a chat is the nearest agent harness by name (an agent started from a terminal).
//   - A harness started by a T3 chat (an agent running `claude -p`) is that chat's work.
//
// The walk stops at a parent whose start time is later than its child's: that ID was reused.

import "strings"

const (
	defaultHosts   = "t3 code"
	defaultHarness = "claude.exe,codex.exe,opencode.exe"
)

type chatRule struct {
	hosts   []string        // lower-case name prefixes of the programs that start chats ("t3 code")
	harness map[string]bool // lower-case harness program names
	only    map[uint32]bool // when set, these process IDs are the only chats (tests)
}

func newChatRule(hosts, harness string) chatRule {
	r := chatRule{harness: nameSet(harness)}
	for h := range nameSet(hosts) {
		r.hosts = append(r.hosts, h)
	}
	return r
}

func (r chatRule) isHost(name string) bool {
	for _, h := range r.hosts {
		if strings.HasPrefix(name, h) {
			return true
		}
	}
	return false
}

// of returns the chat pid belongs to, or 0. A chat is never its own chat's work.
func (r chatRule) of(pid uint32, procs map[uint32]procInfo) uint32 {
	var nearest uint32
	cur := pid
	for depth := 0; depth < 64; depth++ {
		c := procs[cur]
		par, ok := procs[c.ppid]
		if !ok || c.ppid == cur || par.created > c.created {
			break
		}
		if len(r.only) > 0 {
			if r.only[c.ppid] {
				return c.ppid
			}
		} else if r.isHost(par.name) {
			if cur == pid || r.isHost(c.name) {
				return 0 // a chat itself, or T3 Code's own process
			}
			return cur
		} else if nearest == 0 && r.harness[par.name] {
			nearest = c.ppid
		}
		cur = c.ppid
	}
	return nearest
}
