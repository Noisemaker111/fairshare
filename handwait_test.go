package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Real foreground waits from Claude chats on this PC, 2026-11-06..08 (session-audit reruns.txt):
// each ran to its timeout and was run again.
var realHandWaits = map[string]struct{ shell, cmd string }{
	"deadline loop, sleep as tail -f /dev/null (50 min of reruns)": {"bash",
		`T="C:/Users/Jk101/AppData/Local/Temp/claude/ed00eb02f8a36350/c6c8d8b4-529b-4ee4-9490-9ca1c525c48c/tasks"; end=$((SECONDS+540)); done=""; while [ $SECONDS -lt $end ]; do for t in b2vlwb0dw bbm2syppe back94bzr; do grep -qE "lane:close|rror|FAIL|PR:" $T/$t.output 2>/dev/null && { echo "== $t"; cat $T/$t.output; done=1; }; done; [ -n "$done" ] && break; timeout 30 tail -f /dev/null; done; date +%H:%M`},
	"deadline loop, sleep as read -t": {"bash",
		`W=/c/Users/Jk101/Projects/jgengine/.claude/worktrees; end=$((SECONDS+580)); while [ $SECONDS -lt $end ]; do n=$(for w in build-tiers storage-box; do grep -l "^lane:close:\|exited\|error" $W/$w/.scratch/close.log 2>/dev/null; done | wc -l); [ "$n" -gt 0 ] && break; read -t 15 < /dev/zero 2>/dev/null || timeout 15 tail -f /dev/null; done; for w in build-tiers storage-box; do echo "-- $w"; tail -n 3 $W/$w/.scratch/close.log | cut -c1-220; done`},
	"node -e log poller (48 min)": {"bash",
		"cd C:/Users/Jk101/.t3/worktrees/appforgutters/t3-c94f4ac1 && timeout 570 node -e '\nconst fs=require(\"fs\");const f=\".scratch/verify-full5.log\";\nconst check=()=>{let t=\"\";try{t=fs.readFileSync(f,\"utf8\")}catch{};if(/VERIFY_EXIT (\\d+)/.test(t)){console.log(\"DONE\");process.exit(0);}};\ncheck();setInterval(check,5000);\n'; date -u; grep -a -E \"^\\[fail\\]|VERIFY_EXIT|Verification\" .scratch/verify-full5.log | tail"},
	"fairshare await by hand (35 min)": {"bash",
		`/c/Users/Jk101/AppData/Local/fairshare/bin/fairshare.exe await "C:/Users/Jk101/AppData/Local/Temp/claude/ed00eb02f8a36350/c6c8d8b4-529b-4ee4-9490-9ca1c525c48c/tasks/bce9mpekt.output" 570 600 2>&1 | tail -6 | cut -c1-260`},
	"for/seq poll on a task output (12 min)": {"bash",
		`F=/c/Users/Jk101/AppData/Local/Temp/claude/ed00eb02f8a36350/c6c8d8b4-529b-4ee4-9490-9ca1c525c48c/tasks/brcl8a8xu.output; for i in $(seq 1 55); do grep -q "^exit " $F 2>/dev/null && break; sleep 10; done; tail -4 /c/Users/Jk101/Projects/jgengine/.claude/worktrees/shelter/.scratch/close.log | cut -c1-300`},
	"until a results file appears (10 min)": {"bash",
		`until [ -f /c/Users/Jk101/AppData/Local/Temp/claude/4eed703267cf5c7f/scratch/packflags/results.json ] || ! tasklist //FI "PID eq 44588" //NH | grep -q python; do sleep 5; done; cat /c/Users/Jk101/AppData/Local/Temp/claude/4eed703267cf5c7f/1f5c7b8d-1ce6-4255-b1e6-fe2df366f6e3/tasks/bb83smso4.output`},
	"until a log says exit": {"bash",
		`until grep -q "^exit" lane6.log; do sleep 5; done; tail -3 lane6.log`},
	"log watched for a fixed time (10 min)": {"bash",
		`timeout 590 tail -n +1 -f C:/Users/Jk101/AppData/Local/Temp/claude/ed00eb02f8a36350/c6c8d8b4/scratch/red.log`},
	"PowerShell deadline loop": {"pwsh",
		`$s='C:\Users\Jk101\.t3\scratch\x'; $deadline=(Get-Date).AddSeconds(200); while ((Get-Date) -lt $deadline -and -not (Select-String -Path "$s\probe-backend.log" -Pattern 'functions ready' -Quiet)) { Start-Sleep 5 }; Get-Content "$s\probe-backend.log" -Tail 20`},
	"PowerShell do/until": {"pwsh",
		`do { Start-Sleep 10 } until (Test-Path .scratch\done.txt); Get-Content .scratch\done.txt`},
}

// Commands that look alike but are work, a server readiness check, or a wait that ends at a line.
var notHandWaits = map[string]struct{ shell, cmd string }{
	"dev server log, 2 min": {"bash", `for i in $(seq 1 40); do grep -q -i "ready\|functions ready\|error" .scratch/convex-dev.log && break; sleep 3; done; tail -8 .scratch/convex-dev.log`},
	"heredoc mentions node": {"bash", "cat > fn.txt <<'EOF'\nnode -e 'setInterval(()=>fs.readFileSync(f),1000)'\nEOF\nwc -l fn.txt"},
	"server readiness":      {"bash", `for i in $(seq 1 30); do curl -s -o /dev/null -w "%{http_code}" http://localhost:3001/ | grep -q 200 && break; sleep 2; done; curl -s http://localhost:3001/`},
	"sampling":              {"bash", `for i in 1 2 3 4 5; do nvidia-smi --query-gpu=utilization.gpu --format=csv,noheader; sleep 1; done`},
	"retries over a list":   {"bash", "for i in 1 2 3; do curl -s x; sleep 5; done"},
	"loop then a pause":     {"bash", `d="$TEMP/cc"; for n in 1 2 3; do bun run sb f$n.pid >/dev/null; done; sleep 2; ls *.pid`},
	"read loop":             {"bash", `while read -r f; do wc -l "$f"; done < list.txt`},
	"plain loop":            {"bash", "for f in a b; do go vet ./$f; done"},
	"waits for a line":      {"bash", "tail -n +1 -F run/server.log | grep -m1 StartGame"},
	"capped tail of a line": {"bash", "timeout 120 tail -f run/server.log | grep -m1 ready"},
	"node timer, no file":   {"bash", `node -e "for (const x of a) setTimeout(()=>{}, 10)"; echo "sleep 5"`},
	"node reads a file":     {"bash", `node -e "console.log(require('fs').readFileSync('package.json','utf8').length)"`},
	"python heredoc":        {"bash", "python -I - <<'EOF'\nimport time\nwhile True:\n    time.sleep(1)\n    break\nEOF"},
	"pause between steps":   {"pwsh", "python lab.py click A 1 2; Start-Sleep 12; python lab.py shot A a.png"},
	"word done in a path":   {"bash", `for f in /c/x/done/*.log; do tail -1 "$f"; done; sleep 1`},
	"test, build, status":   {"bash", "go vet ./... && go test ./... && git status"},
	"PowerShell foreach":    {"pwsh", `Get-ChildItem *.log | ForEach-Object { Get-Content $_ -Tail 1 }; Start-Sleep 1`},
}

func TestHandWaitsAreSentBack(t *testing.T) {
	dir := t.TempDir()
	for name, c := range realHandWaits {
		d := testCall(t, dir, c.cmd, c.shell, "claude", nil)
		if d.Kind != "deny" || d.Rules[0] != "hand-wait" {
			t.Errorf("%s: not sent back: %+v", name, d)
			continue
		}
		if !strings.Contains(d.Reason, "run_in_background: true") || !strings.Contains(d.Reason, "completion notice") {
			t.Errorf("%s: reason does not point to the background option: %s", name, d.Reason)
		}
		// In the background the same command is the tool's own wait, and Codex has no completion notice.
		if d := testCall(t, dir, c.cmd, c.shell, "claude", map[string]any{"run_in_background": true}); d.Kind == "deny" {
			t.Errorf("%s: sent back from the background: %s", name, d.Reason)
		}
		if d := testCall(t, dir, c.cmd, "pwsh", "codex", nil); d.Kind == "deny" {
			t.Errorf("%s: sent back for Codex", name)
		}
	}
	for name, c := range notHandWaits {
		if d := testCall(t, dir, c.cmd, c.shell, "claude", nil); d.Kind == "deny" {
			t.Errorf("%s: sent back: %s\n%s", name, c.cmd, d.Reason)
		}
	}
}

// The installed hook answers Claude's PostToolUseFailure for a PowerShell call that never started
// with a pointer to Owl's what-is-running command; other failures get nothing.
func TestPowerShellAccessDeniedPointsToOwl(t *testing.T) {
	run := func(payload string) string {
		c := exec.Command(fairshareBin(t), "hook", "claude")
		c.Stdin = strings.NewReader(payload)
		c.Env = append(os.Environ(), "FAIRSHARE_EVENTS="+filepath.Join(t.TempDir(), "events.jsonl"))
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatal(err, string(out))
		}
		return string(out)
	}
	failed := func(cmd, errText string) map[string]any {
		b, _ := json.Marshal(map[string]any{"hook_event_name": "PostToolUseFailure", "tool_name": "PowerShell",
			"tool_input": map[string]any{"command": cmd}, "error": errText})
		out := run(string(b))
		if out == "" {
			return nil
		}
		var r struct{ HookSpecificOutput map[string]any }
		if err := json.Unmarshal([]byte(out), &r); err != nil {
			t.Fatal(out)
		}
		return r.HookSpecificOutput
	}
	// The real one, 2026-11-07 03:06 UTC.
	cpu := `(Get-CimInstance Win32_Processor | Measure-Object -Property LoadPercentage -Average).Average; (Get-CimInstance Win32_Process -Filter "Name='bun.exe'" | Where-Object { $_.CommandLine -match 'verify-fast|lane-close|makensis|smoke' }).Count`
	o := failed(cpu, "Exit code 1\nAccess is denied.")
	note, _ := o["additionalContext"].(string)
	if o["hookEventName"] != "PostToolUseFailure" || !strings.Contains(note, "answers whats-running") || !strings.Contains(note, "did not start") {
		t.Errorf("CPU check: %v", o)
	}
	// A cmdlet's own access error: PowerShell ran, so only the Owl pointer.
	o = failed("Get-CimInstance Win32_Process", "Get-CimInstance: Access is denied.")
	if note, _ := o["additionalContext"].(string); !strings.Contains(note, "whats-running") || strings.Contains(note, "did not start") {
		t.Errorf("cmdlet error: %v", o)
	}
	o = failed("Get-Content a.json | ConvertFrom-Json", "Exit code 1 Access is denied.")
	if note, _ := o["additionalContext"].(string); !strings.Contains(note, "did not start") || strings.Contains(note, "Owl") {
		t.Errorf("non-load command: %v", o)
	}
	if o := failed(cpu, "Exit code 1\nGet-CimInstance: Invalid class \"Win32_Processorx\""); o != nil {
		t.Errorf("unrelated failure got a note: %v", o)
	}
	if out := run(`{"hook_event_name":"PostToolUseFailure","tool_name":"Bash","tool_input":{"command":"ls"},"error":"Exit code 1 Access is denied."}`); out != "" {
		t.Errorf("Bash failure got a note: %s", out)
	}
}
