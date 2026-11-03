package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func testCall(t *testing.T, dir, command, shell, agent string, input map[string]any) Decision {
	t.Helper()
	in := map[string]any{"command": command}
	for k, v := range input {
		in[k] = v
	}
	return decide(Call{Agent: agent, Tool: "Bash", Shell: shell, Cwd: dir, Command: &command, Input: in})
}

func bigFile(t *testing.T) string {
	dir := t.TempDir()
	var b strings.Builder
	for i := range 3000 {
		b.WriteString("const line" + itoa(i) + " = " + itoa(i) + ";\n")
	}
	os.WriteFile(filepath.Join(dir, "big.ts"), []byte(strings.TrimSuffix(b.String(), "\n")), 0o644)
	return dir
}

func itoa(i int) string { return strconv.Itoa(i) }

func match(t *testing.T, s, pattern string) {
	t.Helper()
	if !regexp.MustCompile(pattern).MatchString(s) {
		t.Errorf("%q does not match %s", s, pattern)
	}
}

func TestOnlyUnsafeRmIsSentBack(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []string{"Start-Sleep 30; Get-Content out.log -Tail 5", "until grep -q done log; do sleep 5; done", "find / -maxdepth 6 -name x", "cat nope.ts", `rm -rf "$D"`, `rm -rf "$TEMP/x"`, "rm -rf build dist"} {
		for _, agent := range []string{"claude", "codex", "opencode"} {
			shell := "pwsh"
			if agent == "claude" {
				shell = "bash"
			}
			if d := testCall(t, dir, c, shell, agent, nil); d.Kind == "deny" {
				t.Errorf("%s %q was sent back: %s", agent, c, d.Reason)
			}
		}
	}
}

func TestUnsafeRmMatchesClaudeSafetyCheck(t *testing.T) {
	dir := t.TempDir()
	sent := []string{
		`S=/c/x/models; for p in a b; do rm -rf "$S/$p" && cp -r src/$p "$S/$p"; done`,
		`rm -rf ~/$name`, `rm -rf /`, `rm -rf /*`, `rm -rf ~`, `rm -rf .`, "rm -rf " + posix(homeDir()),
	}
	for _, c := range sent {
		if d := testCall(t, dir, c, "bash", "claude", nil); d.Kind != "deny" || d.Rules[0] != "unsafe-rm" {
			t.Errorf("%q not sent back: %+v", c, d)
		}
	}
	for _, c := range []string{`rm -rf "$D"`, `rm -rf "$TEMP/owl-done"`, "rm -rf node_modules/*", "rm -rf /c/qw/tmp/*", "rm -f /tmp/x"} {
		if d := testCall(t, dir, c, "bash", "claude", nil); d.Kind == "deny" {
			t.Errorf("%q sent back: %s", c, d.Reason)
		}
	}
	if d := testCall(t, dir, `rm -rf "$S/$p"`, "bash", "codex", nil); d.Kind == "deny" {
		t.Error("Codex has no such check, so nothing to send back")
	}
	if d := testCall(t, dir, `Remove-Item -Recurse -Force "$a\$b"`, "pwsh", "claude", nil); d.Kind != "deny" {
		t.Errorf("PowerShell delete that empties to the root not sent back: %+v", d)
	}
}

func TestTextInsideQuotesIsNeverRewritten(t *testing.T) {
	d := testCall(t, t.TempDir(), `& "C:\Program Files\Git\bin\bash.exe" -c "tail -F log 2>/dev/null | grep -m1 done"`, "pwsh", "codex", nil)
	if d.Kind != "pass" && strings.Contains(d.Command, "2>$null") {
		t.Error(d.Command)
	}
}

func TestCmdSyntaxIsLeftAlone(t *testing.T) {
	if d := testCall(t, t.TempDir(), `cd /d C:\Users\Jk101 & dir /b *.log 2>nul`, "pwsh", "codex", nil); d.Kind != "pass" {
		t.Error(d)
	}
}

func TestClaudeWaits(t *testing.T) {
	dir := t.TempDir()
	if d := testCall(t, dir, "tail -n +1 -F run/server.log | grep -m1 StartGame", "bash", "claude", nil); d.Patch["timeout"] != 600000 || d.Patch["run_in_background"] != nil {
		t.Error("bounded wait", d)
	}
	for _, c := range []string{"tail -F app.log", "bun run build --watch"} {
		if d := testCall(t, dir, c, "bash", "claude", nil); d.Patch["run_in_background"] != true {
			t.Error("unbounded wait", c, d)
		}
	}
	for _, c := range []string{"until grep -q done log; do sleep 5; done", "for i in 1 2 3; do curl -s x; sleep 5; done", "Wait-Process -Id 42", "gh pr checks 7 --watch"} {
		shell := "bash"
		if strings.HasPrefix(c, "Wait") {
			shell = "pwsh"
		}
		if d := testCall(t, dir, c, shell, "claude", nil); d.Patch["timeout"] != 600000 {
			t.Error("wait that ends", c, d)
		}
	}
	if d := testCall(t, dir, `node -e "for (const x of a) setTimeout(()=>{}, 10)"; echo "sleep 5"`, "bash", "claude", nil); d.Kind != "pass" {
		t.Error(d)
	}
	if d := testCall(t, dir, "python lab.py click A 1 2; Start-Sleep 12; python lab.py shot A a.png", "pwsh", "claude", nil); d.Kind != "pass" {
		t.Error(d)
	}
	if d := testCall(t, dir, "sleep 60", "bash", "claude", map[string]any{"run_in_background": true}); d.Kind != "pass" {
		t.Error(d)
	}
}

func TestEarlyReadWaitsInTheSameCall(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "tasks"), 0o755)
	out := posix(filepath.Join(dir, "tasks", "b123.output"))
	os.WriteFile(out, []byte("still going\n"), 0o644)
	d := testCall(t, dir, `cat "`+out+`"`, "bash", "claude", nil)
	match(t, d.Command, `' await '.*b123\.output' 300 20; cat`)
	if d.Patch["timeout"] != 330000 {
		t.Error(d.Patch)
	}
	f, _ := os.OpenFile(out, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("[exited with code 0]\n")
	f.Close()
	if d := testCall(t, dir, `cat "`+out+`"`, "bash", "claude", nil); d.Kind != "pass" {
		t.Error(d)
	}
}

func TestLongClaudeRunsGetTenMinutes(t *testing.T) {
	dir := t.TempDir()
	if d := testCall(t, dir, "bun run check:core 2>&1 | tail -5", "bash", "claude", nil); d.Patch["timeout"] != 600000 {
		t.Error(d)
	}
	if d := testCall(t, dir, "bun test test/a.test.ts", "bash", "claude", nil); d.Patch["timeout"] != nil {
		t.Error(d)
	}
	if d := testCall(t, dir, "bun run check:core", "bash", "claude", map[string]any{"timeout": 300000.0}); d.Patch["timeout"] != nil {
		t.Error(d)
	}
	if d := testCall(t, dir, `bun -e "for (let i = 0; i < 3; i++) console.log(i)"`, "bash", "claude", nil); d.Kind != "pass" {
		t.Error(d)
	}
}

func TestLongRunsUseTheRunnerUnlessTheyNeedTheAgentShell(t *testing.T) {
	dir := t.TempDir()
	// Redirected runs stay in the agent's shell; whole suites only wait for a heavy-job slot there.
	match(t, testCall(t, dir, "bun run check:repo *> $log", "pwsh", "codex", nil).Command, `^& '.*' slot bun run check:repo \*> \$log$`)
	if d := testCall(t, dir, "bun test > out.txt", "bash", "claude", nil); strings.Contains(d.Command, " exec ") {
		t.Error(d)
	}
	match(t, testCall(t, dir, "bun test", "pwsh", "codex", nil).Command, `^& '.*' exec pwsh \S+ 0$`)
	match(t, testCall(t, dir, "bun test", "bash", "claude", nil).Command, `^'.*' exec bash \S+ 585000$`)
}

func TestServersAreNeverLongRuns(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []string{"bunx convex dev --typecheck disable --local-cloud-port 3250", "bun run dev", "npm start", "vite preview --port 4173", "cargo build --watch"} {
		if d := testCall(t, dir, c, "pwsh", "codex", nil); slicesContains(d.Rules, "long-run") {
			t.Error(c, d)
		}
	}
	for _, c := range []string{"go test ./... -run TestStart", "bun run test:dev-tools", "turbo run build"} {
		if d := testCall(t, dir, c, "pwsh", "codex", nil); !slicesContains(d.Rules, "long-run") {
			t.Error("should be a long run:", c, d)
		}
	}
}

func slicesContains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func TestDriveScansAreCutOffAndPromptsPreAnswered(t *testing.T) {
	dir := t.TempDir()
	match(t, testCall(t, dir, "find / -maxdepth 6 -iname x.ts 2>/dev/null", "bash", "claude", nil).Command, `^timeout 45 find /`)
	if d := testCall(t, dir, "find ~ -maxdepth 3 -name x.ts", "bash", "claude", nil); d.Kind != "pass" {
		t.Error(d)
	}
	match(t, testCall(t, dir, `powershell -Command "echo hi"`, "pwsh", "codex", nil).Command, `powershell -NoProfile -Command`)
	match(t, testCall(t, dir, "npx cowsay hi", "bash", "claude", nil).Command, `npx -y cowsay`)
	match(t, testCall(t, dir, "git merge origin/beta", "bash", "claude", nil).Command, `git merge --no-edit origin/beta`)
	match(t, testCall(t, dir, "ssh host uptime", "bash", "claude", nil).Command, `ssh -o BatchMode=yes host uptime`)
	if d := testCall(t, dir, "cat notes.md 2>/dev/null", "bash", "claude", nil); d.Kind != "pass" {
		t.Error(d)
	}
}

func TestBashHabitsInPowerShell(t *testing.T) {
	d := testCall(t, t.TempDir(), "git diff --stat | tail -n 20; which node", "pwsh", "codex", nil)
	match(t, d.Command, `Select-Object -Last 20`)
	match(t, d.Command, `Get-Command node`)
}

func TestBigWholeFilePrintBecomesAWindow(t *testing.T) {
	dir := bigFile(t)
	d := testCall(t, dir, "Get-Content "+filepath.Join(dir, "big.ts"), "pwsh", "codex", nil)
	match(t, d.Command, `-TotalCount 200`)
	match(t, d.Command, `3,000 lines`)
}

func TestMissingPaths(t *testing.T) {
	dir := bigFile(t)
	os.MkdirAll(filepath.Join(dir, "src"), 0o755)
	os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
	os.WriteFile(filepath.Join(dir, "src", "only.ts"), []byte("x"), 0o644)
	match(t, testCall(t, dir, "Get-Content only.ts", "pwsh", "codex", nil).Command, `^Write-Output '\[fairshare: only\.ts does not exist; reading src/only\.ts\]'; Get-Content src/only\.ts$`)
	match(t, testCall(t, dir, "Get-Content lib/only.ts", "pwsh", "codex", nil).Command, `^Write-Output '\[fairshare: lib/only\.ts does not exist\. Closest: src/only\.ts\]'; Get-Content lib/only\.ts$`)
	match(t, testCall(t, dir, "Get-Content bigg.ts", "pwsh", "codex", nil).Command, `Closest: big\.ts`)
	if d := testCall(t, dir, "Select-String -Path big.ts -Pattern foo.bar", "pwsh", "codex", nil); d.Kind != "pass" {
		t.Error(d)
	}
}

func TestEmptySearchEndingABatch(t *testing.T) {
	dir := bigFile(t)
	match(t, testCall(t, dir, "Get-Content big.ts -TotalCount 5; rg -n nothing big.ts", "pwsh", "codex", nil).Command, `LASTEXITCODE = 0`)
	match(t, testCall(t, dir, "cat x.ts; grep -n nothing x.ts", "bash", "opencode", nil).Command, `\{ grep -n nothing x\.ts \|\| \[ \$\? -eq 1 \]; \}$`)
	if d := testCall(t, dir, "cat x.ts; grep -n nothing x.ts", "bash", "claude", nil); strings.Contains(d.Command, "|| [") {
		t.Error(d)
	}
	if d := testCall(t, dir, "git status && rg -n nothing big.ts", "pwsh", "codex", nil); strings.Contains(d.Command, "LASTEXITCODE") {
		t.Error(d)
	}
}

func TestRgLinesCappedOnlyWhereAPersonReads(t *testing.T) {
	dir := t.TempDir()
	match(t, testCall(t, dir, "rg -n foo src | Select-Object -First 5", "pwsh", "codex", nil).Command, `^rg --max-columns=400`)
	match(t, testCall(t, dir, "rg -n foo src | rg -v bar", "pwsh", "codex", nil).Command, `^rg -n foo src \|`)
}

func TestUnquotedWindowsPathsInBash(t *testing.T) {
	match(t, testCall(t, t.TempDir(), `ls C:\Users\Jk101\Projects`, "bash", "claude", nil).Command, `C:/Users/Jk101/Projects`)
}

func TestPathologicalInputFinishesFast(t *testing.T) {
	dir := t.TempDir()
	t0 := time.Now()
	testCall(t, dir, "ls C:\\"+strings.Repeat("a\\", 5000)+" x", "bash", "claude", nil)
	testCall(t, dir, "rg "+strings.Repeat(`"`, 3)+strings.Repeat("x ", 5000), "pwsh", "codex", nil)
	testCall(t, dir, "for x in "+strings.Repeat("a ", 5000)+"; do sleep 1; done", "bash", "claude", nil)
	if d := time.Since(t0); d > 300*time.Millisecond {
		t.Error("took", d)
	}
}

func TestCodexRechecksBecomeOneLongWait(t *testing.T) {
	d := decide(Call{Agent: "codex", Tool: "write_stdin", Input: map[string]any{"session_id": 7.0, "chars": "", "yield_time_ms": 1000.0}})
	if d.Input["yield_time_ms"] != 30000 {
		t.Error(d)
	}
	if d := decide(Call{Agent: "codex", Tool: "write_stdin", Input: map[string]any{"session_id": 7.0, "chars": "y\n"}}); d.Kind != "pass" {
		t.Error(d)
	}
	if d := decide(Call{Agent: "codex", Tool: "wait", Input: map[string]any{"cell_id": 1.0, "terminate": true, "yield_time_ms": 1000.0}}); d.Kind != "pass" {
		t.Error(d)
	}
}

func TestCodexTrustHashMatchesCodex(t *testing.T) {
	group := map[string]any{"matcher": "Bash|shell|exec_command", "hooks": []any{map[string]any{"type": "command", "command": "node C:/Users/Jk101/Projects/JonsOCsetup/scripts/fresh-services.mjs --hook", "timeout": 120.0}}}
	if h := codexTrustHash("post_tool_use", group); h != "sha256:fcd47f0b5a3400c3313364a66cc1fe27b8ce9abc49b2aab9c0ada294a5906fe3" {
		t.Error(h)
	}
}
