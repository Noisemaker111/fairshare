package main

// `fairshare hook claude|codex|opencode` reads one PreToolUse payload (or Claude's PostToolUseFailure,
// failure.go) on stdin and answers in that agent's protocol. On any internal error it prints nothing, which lets the call through unchanged.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"time"

	"github.com/Noisemaker111/fairshare/internal/windows"
)

// sinceProcessStart is how long ago Windows created this process, in ms: what a call costs before
// the hook's own work (loader, runtime and package init) plus that work. hook_ms is the work alone.
func sinceProcessStart() int64 {
	var created, exited, kernel, user windows.Filetime
	if windows.GetProcessTimes(windows.CurrentProcess(), &created, &exited, &kernel, &user) != nil {
		return -1
	}
	return time.Since(time.Unix(0, created.Nanoseconds())).Milliseconds()
}

type hookPayload struct {
	Event     string         `json:"hook_event_name"`
	Tool      string         `json:"tool_name"`
	Input     map[string]any `json:"tool_input"`
	Session   string         `json:"session_id"`
	Cwd       string         `json:"cwd"`
	Mode      string         `json:"permission_mode"`
	OpenField string         `json:"command_field"` // opencode: which input field holds the command
	Error     string         `json:"error"`         // PostToolUseFailure: what the failed call printed
}

func toCall(agent string, p hookPayload) Call {
	shell := "pwsh"
	if agent == "claude" {
		if p.Tool != "PowerShell" {
			shell = "bash"
		}
	} else if runtime.GOOS != "windows" {
		shell = "bash"
	} else if agent == "opencode" && os.Getenv("FAIRSHARE_OPENCODE_SHELL") == "bash" {
		shell = "bash"
	}
	c := Call{Agent: agent, Tool: p.Tool, Shell: shell, Cwd: p.Cwd, Session: p.Session, Input: p.Input}
	field := p.OpenField
	if field == "" {
		field = "command"
	}
	if s, ok := p.Input[field].(string); ok {
		c.Command = &s
	}
	return c
}

// pendingNote is a line for the agent (lowdisk.go) that rides on the hook's reply: Claude reads it
// as additionalContext; the other agents get it on stderr.
var pendingNote string

// noteOnly is the reply of a call that passes unchanged but carries pendingNote.
func noteOnly(agent string) {
	if pendingNote == "" {
		return
	}
	if agent != "claude" {
		fmt.Fprintln(os.Stderr, pendingNote)
		return
	}
	b, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PreToolUse", "additionalContext": pendingNote}})
	os.Stdout.Write(b)
}

// answer is the hook's reply in the agent's protocol.
func answer(agent string, p hookPayload, d Decision) map[string]any {
	out := map[string]any{"hookEventName": "PreToolUse"}
	if pendingNote != "" && agent == "claude" {
		out["additionalContext"] = pendingNote
	}
	if d.Kind == "deny" {
		out["permissionDecision"] = "deny"
		out["permissionDecisionReason"] = "[fairshare] " + d.Reason
		return map[string]any{"hookSpecificOutput": out}
	}
	updated := d.Input
	if d.Kind == "rewrite" {
		updated = maps(p.Input)
		field := p.OpenField
		if field == "" {
			field = "command"
		}
		updated[field] = d.Command
		for k, v := range d.Patch {
			updated[k] = v
		}
	}
	out["updatedInput"] = updated
	// Codex only applies updatedInput with an explicit allow; its own approval and sandbox checks
	// still run on the rewritten call. Claude applies it without a decision, keeping its own flow.
	if agent == "codex" {
		out["permissionDecision"] = "allow"
	}
	return map[string]any{"hookSpecificOutput": out}
}

func cmdHook(args []string) int {
	t0 := time.Now()
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: fairshare hook claude|codex|opencode")
		return 0
	}
	agent := args[0]
	defer func() {
		if r := recover(); r != nil {
			logEvent("error", map[string]any{"agent": agent, "error": clip(fmt.Sprint(r), 600)})
		}
	}()
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return 0
	}
	var p hookPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		logEvent("error", map[string]any{"agent": agent, "error": "bad payload: " + clip(err.Error(), 200)})
		return 0
	}
	if p.Input == nil {
		p.Input = map[string]any{}
	}
	if p.Event == "PostToolUseFailure" {
		if note := failureNote(agent, p); note != "" {
			b, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": p.Event, "additionalContext": note}})
			os.Stdout.Write(b)
			cmd, _ := p.Input["command"].(string)
			logEvent("failure", map[string]any{"agent": agent, "tool": p.Tool, "session": p.Session, "rules": []string{"pwsh-no-start"},
				"cmd": clip(cmd, 300), "error": clip(p.Error, 200)})
		}
		return 0
	}
	if p.Event != "" && p.Event != "PreToolUse" {
		return 0
	}
	pendingNote = lowDiskCheck(time.Now())
	// chat_pid ties the call to its chat's lane (adopt names lanes by harness process). Only logged
	// calls look it up: a Read that waits on nothing logs nothing.
	ev := map[string]any{"agent": agent, "tool": p.Tool, "session": p.Session, "cwd": p.Cwd}
	chat := func() uint32 { return newChatRule(defaultHosts, defaultHarness).of(uint32(os.Getpid()), liveProc) }

	// Claude's Read of a still-running task's output: wait here, then let the read through (no turn spent).
	if agent == "claude" && p.Tool == "Read" {
		file, _ := p.Input["file_path"].(string)
		if running := runningTaskFile(file); running != "" {
			why := awaitTask(running, 300*time.Second, 20*time.Second)
			ev["kind"], ev["rules"], ev["cmd"], ev["waited"] = "wait", "early-read", clip(file, 300), why
			ev["chat_pid"], ev["hook_ms"], ev["proc_ms"] = chat(), time.Since(t0).Milliseconds(), sinceProcessStart()
			logEvent("call", ev)
		}
		noteOnly(agent)
		return 0
	}

	c := toCall(agent, p)
	d := decide(c)
	ev["kind"] = d.Kind
	if c.Command != nil {
		ev["cmd"] = clip(*c.Command, 300)
	}
	if len(d.Rules) > 0 {
		ev["rules"] = d.Rules
	}
	switch d.Kind {
	case "rewrite":
		ev["after"] = clip(d.Command, 300)
	case "deny":
		ev["reason"] = clip(d.Reason, 300)
	case "input":
		b, _ := json.Marshal(d.Input)
		ev["after"] = clip(string(b), 300)
	}
	if d.Kind != "pass" {
		b, err := json.Marshal(answer(agent, p, d))
		if err == nil {
			os.Stdout.Write(b)
		}
		if agent != "claude" && pendingNote != "" {
			fmt.Fprintln(os.Stderr, pendingNote)
		}
	} else {
		noteOnly(agent)
	}
	ev["chat_pid"], ev["hook_ms"], ev["proc_ms"] = chat(), time.Since(t0).Milliseconds(), sinceProcessStart()
	logEvent("call", ev)
	return 0
}
