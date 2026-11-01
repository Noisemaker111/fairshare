package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// install replaces agent-guard's hooks, keeps everyone else's, trusts its Codex entry once however
// often it runs, and uninstall takes only its own entries back out. All in a throwaway home.
func TestInstallReplacesAgentGuardAndIsIdempotent(t *testing.T) {
	bin := fairshareBin(t)
	home := filepath.Join(t.TempDir(), "a home") // a space, as in "C:\Users\First Last"
	os.MkdirAll(filepath.Join(home, ".claude"), 0o755)
	os.MkdirAll(filepath.Join(home, ".codex"), 0o755)
	settings := filepath.Join(home, ".claude", "settings.json")
	os.WriteFile(settings, []byte(`{"model":"x","hooks":{"PreToolUse":[
		{"matcher":"Bash|PowerShell","hooks":[{"type":"command","command":"node \"C:/p/agent-guard/guard.mjs\" claude","timeout":10}]},
		{"matcher":"Edit","hooks":[{"type":"command","command":"other-tool check"}]}]}}`), 0o644)
	codexHooks := filepath.Join(home, ".codex", "hooks.json")
	os.WriteFile(codexHooks, []byte(`{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"node \"C:/p/agent-guard/guard.mjs\" codex","timeout":10}]}]}}`), 0o644)
	config := filepath.Join(home, ".codex", "config.toml")
	// agent-guard's marked entry, and the unmarked one Codex writes itself for the same key: two
	// tables with one key stop Codex from starting at all.
	os.WriteFile(config, []byte("model = \"gpt\"\n\n[hooks.state]\n\n[hooks.state.'"+codexHooks+":pre_tool_use:0:0']\ntrusted_hash = \"sha256:old\"\n# agent-guard\n\n"+
		"[hooks.state.'"+codexHooks+":pre_tool_use:0:0']\ntrusted_hash = \"sha256:codexown\"\n\n[hooks.state.\"other:hooks.json:stop:0:0\"]\ntrusted_hash = \"sha256:keep\"\nenabled = false\n"), 0o644)

	run := func(args ...string) {
		c := exec.Command(bin, args...)
		c.Env = append(os.Environ(), "USERPROFILE="+home, "HOME="+home, "CODEX_HOME="+filepath.Join(home, ".codex"), "LOCALAPPDATA="+filepath.Join(home, "local"))
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatal(args, err, string(out))
		}
	}
	read := func(p string) string { b, _ := os.ReadFile(p); return string(b) }

	// --hooks-only: this throwaway install must not replace the PC's running adopt
	run("install", "--hooks-only")
	run("install", "--hooks-only")
	plugin := filepath.Join(home, ".config", "opencode", "plugins", "fairshare", "index.mjs")
	if !strings.Contains(read(plugin), "['hook', 'opencode']") {
		t.Error("OpenCode plugin not installed:", plugin)
	}
	s, h, c := read(settings), read(codexHooks), read(config)
	var hooks struct {
		Hooks struct {
			PreToolUse []struct{ Hooks []struct{ Command string } }
		}
	}
	json.Unmarshal([]byte(h), &hooks)
	if len(hooks.Hooks.PreToolUse) != 1 || len(hooks.Hooks.PreToolUse[0].Hooks) != 1 {
		t.Fatal("codex hooks:", h)
	}
	command := hooks.Hooks.PreToolUse[0].Hooks[0].Command
	if strings.Contains(s, "agent-guard") || strings.Count(s, strings.TrimSuffix(command, "codex")+"claude") != 2 || !strings.Contains(s, "other-tool check") || !strings.Contains(s, `"model": "x"`) {
		t.Error("claude settings:", s)
	}
	if strings.Contains(h, "agent-guard") {
		t.Error("codex hooks:", h)
	}
	if strings.Contains(c, "sha256:old") || strings.Contains(c, "sha256:codexown") || strings.Count(c, "pre_tool_use:0:0") != 1 || strings.Count(c, "# fairshare") != 1 ||
		!strings.Contains(c, `model = "gpt"`) || !strings.Contains(c, "sha256:keep") || strings.Contains(c, "\n\n\n") {
		t.Error("codex config:", c)
	}
	// Each agent starts the hook through its own shell (Codex: PowerShell); it must run in all of them.
	payload := `{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"git status | head -n 3"},"cwd":"C:\\"}`
	shells := [][]string{{"pwsh", "-NoProfile", "-Command", command}, {"cmd", "/d", "/c", command}}
	if bash, err := exec.LookPath("bash"); err == nil {
		shells = append(shells, []string{bash, "-c", command})
	}
	for _, sh := range shells {
		c := exec.Command(sh[0], sh[1:]...)
		c.Stdin = strings.NewReader(payload)
		c.Env = append(os.Environ(), "FAIRSHARE_EVENTS="+filepath.Join(home, "events.jsonl"))
		out, err := c.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "Select-Object -First 3") {
			t.Errorf("%s could not run the hook %q: %v %s", sh[0], command, err, out)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "local", "fairshare", "bin", "fairshare.exe")); err != nil {
		t.Error("not copied:", err)
	}

	run("install", "--uninstall", "--hooks-only")
	s, h, c = read(settings), read(codexHooks), read(config)
	if strings.Contains(s, "fairshare") || !strings.Contains(s, "other-tool check") || strings.Contains(h, "fairshare") || strings.Contains(c, "# fairshare") {
		t.Error("after uninstall:", s, h, c)
	}
	if _, err := os.Stat(plugin); err == nil {
		t.Error("OpenCode plugin left behind")
	}
}
