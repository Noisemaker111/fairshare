package main

// `fairshare install [--uninstall]` copies fairshare to %LOCALAPPDATA%\fairshare\bin, registers
// that copy's hook with Claude Code and Codex (replacing any agent-guard entry), puts the OpenCode
// plugin in OpenCode's plugins folder, and keeps one adopt running for the whole PC (service.go).
// Idempotent; backs up each file it changes to <file>.fairshare.bak. Codex skips untrusted hooks,
// so this also records the trust hash Codex computes for the entry.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/sys/windows"
)

// codexTrustHash is sha256 over the sorted-key JSON of the normalized hook identity
// (codex-rs/hooks/src/engine/discovery.rs hook_hash + config/src/fingerprint.rs version_for_toml).
func codexTrustHash(eventName string, group map[string]any) string {
	var hooks []any
	for _, h := range group["hooks"].([]any) {
		h := h.(map[string]any)
		n := map[string]any{"type": "command", "command": h["command"], "timeout": 600.0, "async": truthy(h["async"])}
		if t, ok := h["timeout"]; ok && t != nil {
			n["timeout"] = t
		}
		if s, ok := h["statusMessage"].(string); ok && s != "" {
			n["statusMessage"] = s
		}
		hooks = append(hooks, n)
	}
	identity := map[string]any{"event_name": eventName, "hooks": hooks}
	if m, ok := group["matcher"]; ok && m != nil {
		identity["matcher"] = m
	}
	sum := sha256.Sum256([]byte(jsJSON(identity)))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// jsJSON is JSON.stringify with sorted keys: no HTML escaping, integers without a decimal point.
func jsJSON(v any) string {
	var b strings.Builder
	var write func(v any)
	write = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			b.WriteByte('{')
			for i, k := range keys {
				if i > 0 {
					b.WriteByte(',')
				}
				write(k)
				b.WriteByte(':')
				write(x[k])
			}
			b.WriteByte('}')
		case []any:
			b.WriteByte('[')
			for i, e := range x {
				if i > 0 {
					b.WriteByte(',')
				}
				write(e)
			}
			b.WriteByte(']')
		case int:
			fmt.Fprint(&b, x)
		default:
			var buf bytes.Buffer
			enc := json.NewEncoder(&buf)
			enc.SetEscapeHTML(false)
			enc.Encode(x)
			b.WriteString(strings.TrimRight(buf.String(), "\n"))
		}
	}
	write(v)
	return b.String()
}

func readJSONFile(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

func writeJSONFile(path string, m map[string]any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		return err
	}
	backup(path)
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

func backup(path string) {
	if b, err := os.ReadFile(path); err == nil {
		os.WriteFile(path+".fairshare.bak", b, 0o644)
	}
}

// ours is a hook entry this installer owns: fairshare's, or the agent-guard one it replaces.
func ours(h any, command string) bool {
	m, ok := h.(map[string]any)
	if !ok {
		return false
	}
	c, _ := m["command"].(string)
	lc := strings.ToLower(c)
	return strings.HasPrefix(c, command+" hook ") || strings.Contains(lc, "fairshare") && strings.Contains(c, " hook ") || strings.Contains(c, "agent-guard") && strings.Contains(c, "guard.mjs")
}

type hookEntry struct {
	matcher string
	timeout int
}

// register rewrites the PreToolUse groups in a hooks file and returns the index of the first new group.
func register(path, agent string, entries []hookEntry, uninstall bool, command string) (map[string]any, int, error) {
	j, err := readJSONFile(path)
	if err != nil {
		return nil, -1, err
	}
	hooks, _ := j["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
		j["hooks"] = hooks
	}
	var groups []any
	existing, _ := hooks["PreToolUse"].([]any)
	for _, g := range existing {
		gm, _ := g.(map[string]any)
		hs, _ := gm["hooks"].([]any)
		mine := false
		for _, h := range hs {
			if ours(h, command) {
				mine = true
			}
		}
		if !mine {
			groups = append(groups, g)
		}
	}
	index := -1
	var first map[string]any
	if !uninstall {
		for i, e := range entries {
			g := map[string]any{"matcher": e.matcher, "hooks": []any{map[string]any{"type": "command", "command": command + " hook " + agent, "timeout": e.timeout}}}
			groups = append(groups, g)
			if i == 0 {
				index, first = len(groups)-1, g
			}
		}
	}
	if len(groups) == 0 {
		delete(hooks, "PreToolUse")
	} else {
		hooks["PreToolUse"] = groups
	}
	return first, index, writeJSONFile(path, j)
}

// installedExe is where the hooks run fairshare from, so rebuilding a checkout never touches the
// copy every chat is using.
func installedExe() string {
	return filepath.Join(os.Getenv("LOCALAPPDATA"), "fairshare", "bin", "fairshare.exe")
}

// installCopy puts this program at installedExe. A copy that is running (a hook, adopt) cannot be
// overwritten on Windows but can be renamed, so the old one moves aside and is deleted once free.
func installCopy() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	dst := installedExe()
	if strings.EqualFold(filepath.Clean(exe), filepath.Clean(dst)) {
		return dst, nil
	}
	b, err := os.ReadFile(exe)
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	olds, _ := filepath.Glob(filepath.Join(dir, "fairshare.exe.old-*"))
	for _, o := range olds {
		os.Remove(o) // still running ones stay until next time
	}
	if err := os.WriteFile(dst+".new", b, 0o755); err != nil {
		return "", err
	}
	if _, err := os.Stat(dst); err == nil {
		if err := os.Rename(dst, fmt.Sprintf("%s.old-%d", dst, os.Getpid())); err != nil {
			return "", err
		}
	}
	return dst, os.Rename(dst+".new", dst)
}

// hookCommand is how the agents start the hook. Each agent runs it through a different shell
// (Claude Code: bash; Codex: PowerShell, where a line starting with a quoted path is a parse
// error), so it is a bare path with forward slashes, which bash, cmd and PowerShell all run. A
// path with a space uses its short form.
func hookCommand(exe string) (string, error) {
	p := exe
	if strings.ContainsAny(p, " \t") {
		buf := make([]uint16, windows.MAX_PATH)
		n, err := windows.GetShortPathName(windows.StringToUTF16Ptr(p), &buf[0], uint32(len(buf)))
		if err != nil || n == 0 || int(n) > len(buf) {
			return "", fmt.Errorf("%s has a space and no short name, so no shell could start it as a hook", exe)
		}
		p = windows.UTF16ToString(buf[:n])
	}
	if strings.ContainsAny(p, " \t'\"&;|<>()$`") {
		return "", fmt.Errorf("%s cannot be written as a command every shell runs", p)
	}
	return posix(p), nil
}

func cmdInstall(args []string) int {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	uninstallFlag := fs.Bool("uninstall", false, "take everything back out")
	hooksOnly := fs.Bool("hooks-only", false, "leave the PC-wide adopt task as it is")
	fs.Parse(args)
	uninstall := *uninstallFlag
	exe := installedExe()
	if !uninstall {
		var err error
		if exe, err = installCopy(); err != nil {
			fmt.Fprintln(os.Stderr, "fairshare install:", err)
			return 1
		}
		fmt.Println("installed", exe)
	}
	command, err := hookCommand(exe)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fairshare install:", err)
		return 1
	}
	home := homeDir()

	// Claude Code. A Read of a still-running task's output waits inside the hook, up to 300 s.
	claude := filepath.Join(home, ".claude", "settings.json")
	if _, _, err := register(claude, "claude", []hookEntry{{"Bash|PowerShell", 10}, {"Read", 330}}, uninstall, command); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println(map[bool]string{false: "registered in", true: "removed from"}[uninstall], claude)

	// Codex
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		codexHome = filepath.Join(home, ".codex")
	}
	hooksFile := filepath.Join(codexHome, "hooks.json")
	configFile := filepath.Join(codexHome, "config.toml")
	group, index, err := register(hooksFile, "codex", []hookEntry{{"Bash|write_stdin|wait", 10}}, uninstall, command)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	tomlBytes, _ := os.ReadFile(configFile)
	toml := string(tomlBytes)
	// Drop any trust entry written before by this or agent-guard, at any index.
	marked := regexp.MustCompile(`(?m)^\[hooks\.state\.'` + regexp.QuoteMeta(hooksFile) + `:pre_tool_use:\d+:\d+'\]\r?\ntrusted_hash = "[^"]*"\r?\n# (?:agent-guard|fairshare)\r?\n(?:\r?\n)*`)
	toml = marked.ReplaceAllString(toml, "")
	if !uninstall {
		// Also any table with the key about to be written, whoever wrote it: Codex writes its own
		// (unmarked) when a hook is trusted in its UI, and two tables with one key stop Codex starting.
		key := fmt.Sprintf("%s:pre_tool_use:%d:0", hooksFile, index)
		same := regexp.MustCompile(`(?m)^\[hooks\.state\.(?:'` + regexp.QuoteMeta(key) + `'|"` + regexp.QuoteMeta(strings.ReplaceAll(key, `\`, `\\`)) + `")\][^\n]*\n(?:[A-Za-z_"'][^\n]*\n|\r?\n)*`)
		toml = same.ReplaceAllString(toml, "")
		if !regexp.MustCompile(`(?m)^\[hooks\.state\]`).MatchString(toml) {
			toml = strings.TrimRight(toml, " \t\r\n") + "\n\n[hooks.state]\n"
		}
		entry := fmt.Sprintf("\n[hooks.state.'%s:pre_tool_use:%d:0']\ntrusted_hash = \"%s\"\n# fairshare\n", hooksFile, index, codexTrustHash("pre_tool_use", roundTrip(group)))
		loc := regexp.MustCompile(`\[hooks\.state\][^\n]*\n`).FindStringIndex(toml)
		toml = toml[:loc[1]] + entry + toml[loc[1]:]
	}
	backup(configFile)
	if err := os.WriteFile(configFile, []byte(toml), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println(map[bool]string{false: "registered and trusted in", true: "removed from"}[uninstall], hooksFile)

	// OpenCode loads every folder in its plugins directory.
	dir, err := installOpenCodePlugin(uninstall)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println(map[bool]string{false: "OpenCode plugin in", true: "removed"}[uninstall], dir)

	// One adopt for every chat on the PC, kept running by a scheduled task.
	if *hooksOnly {
		return 0
	}
	if err := installService(exe, uninstall); err != nil {
		fmt.Fprintln(os.Stderr, "fairshare install:", err)
		return 1
	}
	fmt.Println(map[bool]string{false: "adopt running for every chat; task", true: "adopt stopped; removed task"}[uninstall], adoptTask)
	return 0
}

// roundTrip gives a value the shape it has when read back from JSON (numbers as float64).
func roundTrip(m map[string]any) map[string]any {
	b, _ := json.Marshal(m)
	var out map[string]any
	json.Unmarshal(b, &out)
	return out
}
