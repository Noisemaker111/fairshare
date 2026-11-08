package main

// `fairshare hook claude` on PostToolUseFailure: a note added to a failed call, for failures whose
// own text sends the agent the wrong way.
//
// pwsh-no-start: Claude's PowerShell tool sometimes fails with nothing but "Access is denied." On
// 2026-11-07 that was 11 calls in 8 chats, about 7% of PowerShell calls between 01:00 and 08:11 UTC,
// on Get-Content and Get-ChildItem as well as Get-CimInstance: PowerShell itself did not start.
// Most were CPU-load and process checks (Get-CimInstance Win32_Processor LoadPercentage), which
// agents then retried or rebuilt; Owl answers that question in one command.

import "regexp"

var (
	accessDenied = regexp.MustCompile(`(?i)\bAccess is denied\b`)
	// The whole error is the exit code and "Access is denied.": no cmdlet name, no position, so
	// PowerShell never got as far as running the command.
	bareDenied = regexp.MustCompile(`(?i)^\s*(?:Exit code -?\d+\s*)?Access is denied\.?\s*$`)
	// A question about load or what is running.
	loadQuery = regexp.MustCompile(`(?i)\b(?:Get-CimInstance|gcim|Get-WmiObject|gwmi|Get-Counter|Get-Process|gps)\b|\bLoadPercentage\b|\bWin32_(?:Processor|Process|PerfFormattedData)`)
)

// Both forms: when PowerShell will not start, the Bash one still works.
const owlWhatsRunning = "`\"$LOCALAPPDATA/Programs/Owl/v2/owl.exe\" answers whats-running --json` from Bash, or " +
	"`& \"$env:LOCALAPPDATA\\Programs\\Owl\\v2\\owl.exe\" answers whats-running --json` from PowerShell"

// failureNote is the context to add to a failed call, or "".
func failureNote(agent string, p hookPayload) string {
	if agent != "claude" || p.Tool != "PowerShell" || !accessDenied.MatchString(p.Error) {
		return ""
	}
	cmd, _ := p.Input["command"].(string)
	note := ""
	if bareDenied.MatchString(p.Error) {
		note = "[fairshare] PowerShell itself did not start (\"Access is denied\" and nothing else), so nothing in this command ran. It comes and goes and is not about the command. "
	}
	if loadQuery.MatchString(cmd) {
		if note == "" {
			note = "[fairshare] "
		}
		return note + "For CPU load or what is running, use Owl's what-is-running command instead (no PowerShell or CIM access needed): " +
			owlWhatsRunning + " (top programs by processor, memory, disk and graphics; `answers why-slow` says what stands out)."
	}
	if note != "" {
		return note + "Run it again, or do it from Bash."
	}
	return ""
}
