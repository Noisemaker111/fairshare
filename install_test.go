package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
		{"matcher":"Read","hooks":[{"type":"command","command":"C:/old/fairshare/bin/fairshare.exe hook claude","timeout":330}]},
		{"matcher":"Edit","hooks":[{"type":"command","command":"other-tool check"}]}]}}`), 0o644)
	codexHooks := filepath.Join(home, ".codex", "hooks.json")
	os.WriteFile(codexHooks, []byte(`{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"node \"C:/p/agent-guard/guard.mjs\" codex","timeout":10}]}]}}`), 0o644)
	config := filepath.Join(home, ".codex", "config.toml")
	// agent-guard's marked entry, and the unmarked one Codex writes itself for the same key: two
	// tables with one key stop Codex from starting at all.
	os.WriteFile(config, []byte("model = \"gpt\"\n\n[hooks.state]\n\n[hooks.state.'"+codexHooks+":pre_tool_use:0:0']\ntrusted_hash = \"sha256:old\"\n# agent-guard\n\n"+
		"[hooks.state.'"+codexHooks+":pre_tool_use:0:0']\ntrusted_hash = \"sha256:codexown\"\n\n[hooks.state.\"other:hooks.json:stop:0:0\"]\ntrusted_hash = \"sha256:keep\"\nenabled = false\n"), 0o644)
	// T3 Code starts Claude and Codex from homes of their own (Jon's "Claude Hub" and "Codex Hub");
	// neither has been set up yet, so install makes them. Other providers' homes are not ours.
	os.MkdirAll(filepath.Join(home, ".t3", "userdata"), 0o755)
	os.WriteFile(filepath.Join(home, ".t3", "userdata", "settings.json"), []byte(`{"providerInstances":{
		"claudeAgent_claude_hub":{"driver":"claudeAgent","config":{"homePath":"~/.claude-hub"}},
		"codex_codex_hub":{"driver":"codex","config":{"homePath":`+strconv.Quote(filepath.Join(home, ".codex-hub"))+`}},
		"grok_grok_hub":{"driver":"grok","config":{"homePath":`+strconv.Quote(filepath.Join(home, ".grok-hub"))+`}},
		"opencode":{"driver":"opencode","enabled":true}}}`), 0o644)
	hubSettings := filepath.Join(home, ".claude-hub", "settings.json")
	hubHooks, hubConfig := filepath.Join(home, ".codex-hub", "hooks.json"), filepath.Join(home, ".codex-hub", "config.toml")

	run := func(args ...string) {
		c := exec.Command(bin, args...)
		// CLAUDE_CONFIG_DIR too: a test run from a T3 chat inherits the PC's real Claude Hub
		c.Env = append(os.Environ(), "USERPROFILE="+home, "HOME="+home, "CODEX_HOME="+filepath.Join(home, ".codex"), "CLAUDE_CONFIG_DIR="+filepath.Join(home, ".claude"), "LOCALAPPDATA="+filepath.Join(home, "local"))
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
	// Claude Code gets the exec form, which it starts without a shell: one entry per matcher,
	// replacing the shell-form entry an older install wrote.
	exe := strings.TrimSuffix(command, " hook codex")
	ourMatchers := func(file, s string) string {
		var claude struct {
			Hooks struct {
				PreToolUse []struct {
					Matcher string
					Hooks   []struct {
						Command string
						Args    []string
					}
				}
			}
		}
		json.Unmarshal([]byte(s), &claude)
		var direct []string
		for _, g := range claude.Hooks.PreToolUse {
			for _, hk := range g.Hooks {
				if len(hk.Args) > 0 || strings.Contains(strings.ToLower(hk.Command), "fairsh") {
					if hk.Command != exe || strings.Join(hk.Args, " ") != "hook claude" {
						t.Errorf("%s: claude hook for %s: %q %q", file, g.Matcher, hk.Command, hk.Args)
					}
					direct = append(direct, g.Matcher)
				}
			}
		}
		return strings.Join(direct, ",")
	}
	direct := []string{ourMatchers(settings, s)}
	if direct[0] != "Bash|PowerShell,Read" || strings.Contains(s, "agent-guard") || !strings.Contains(s, "other-tool check") || !strings.Contains(s, `"model": "x"`) {
		t.Error("claude settings:", s)
	}
	if strings.Contains(h, "agent-guard") {
		t.Error("codex hooks:", h)
	}
	// T3's Claude Hub and Codex Hub get the same hooks; Codex's is trusted in its own config.
	if got := ourMatchers(hubSettings, read(hubSettings)); got != "Bash|PowerShell,Read" {
		t.Error("Claude Hub settings:", read(hubSettings))
	}
	if hh := read(hubHooks); strings.Count(hh, command) != 1 {
		t.Error("Codex Hub hooks:", hh)
	}
	if hc := read(hubConfig); strings.Count(hc, "# fairshare") != 1 || !strings.Contains(hc, hubHooks+":pre_tool_use:0:0") {
		t.Error("Codex Hub config:", hc)
	}
	if _, err := os.Stat(filepath.Join(home, ".grok-hub")); err == nil {
		t.Error("install wrote into another provider's home")
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
	// Claude Code starts its entry directly, no shell: the program and args as written must run.
	started := exec.Command(exe, "hook", "claude")
	started.Stdin = strings.NewReader(`{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"bun run verify:fast"},"cwd":"C:\\"}`)
	started.Env = append(os.Environ(), "FAIRSHARE_EVENTS="+filepath.Join(home, "events.jsonl"))
	if out, err := started.CombinedOutput(); err != nil || !strings.Contains(string(out), " exec bash ") {
		t.Errorf("Claude's exec-form hook %q did not run: %v %s", exe, err, out)
	}
	if _, err := os.Stat(filepath.Join(home, "local", "fairshare", "bin", "fairshare.exe")); err != nil {
		t.Error("not copied:", err)
	}

	run("install", "--uninstall", "--hooks-only")
	s, h, c = read(settings), read(codexHooks), read(config)
	if strings.Contains(s, "fairshare") || !strings.Contains(s, "other-tool check") || strings.Contains(h, "fairshare") || strings.Contains(c, "# fairshare") {
		t.Error("after uninstall:", s, h, c)
	}
	if hs, hh, hc := read(hubSettings), read(hubHooks), read(hubConfig); strings.Contains(hs, "fairshare") || strings.Contains(hh, "fairshare") || strings.Contains(hc, "# fairshare") {
		t.Error("hubs after uninstall:", hs, hh, hc)
	}
	if _, err := os.Stat(plugin); err == nil {
		t.Error("OpenCode plugin left behind")
	}
}
