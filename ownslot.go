package main

// A repository can name whole checks that take a heavy-job slot themselves, for only the part of
// their work that runs on this PC. jgengine's `bun run lane:close` and `verify:fast` send their
// Linux steps to a cloud machine (boat.dev) and wait for one when both are busy; held around the
// whole command, a slot sat idle through that wait while Windows-only work queued behind it. Those
// commands run `fairshare slot` around their own local steps instead.
//
// `.fairshare.json` at the repository root:
//
//	{"ownSlot": ["bun run lane:close", "bun run verify:fast"]}
//
// An entry matches a command that is it, or starts with it and a space. The file is read from the
// command's own checkout, so a checkout from before the repository took its own slot still waits.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

type repoSettings struct {
	OwnSlot []string `json:"ownSlot"`
}

// ownSlot reports whether the repository at cwd says cmd takes its own heavy-job slot.
func ownSlot(cmd, cwd string) bool {
	root := gitRoot(cwd)
	if root == "" {
		return false
	}
	raw, err := os.ReadFile(filepath.Join(root, ".fairshare.json"))
	if err != nil {
		return false
	}
	var settings repoSettings
	if json.Unmarshal(raw, &settings) != nil {
		return false
	}
	cmd = strings.TrimSpace(cmd)
	for _, entry := range settings.OwnSlot {
		entry = strings.TrimSpace(entry)
		if entry != "" && (cmd == entry || strings.HasPrefix(cmd, entry+" ")) {
			return true
		}
	}
	return false
}
