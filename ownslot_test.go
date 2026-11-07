package main

import (
	"os"
	"path/filepath"
	"testing"
)

func repoWith(t *testing.T, settings string) string {
	t.Helper()
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, ".git"), 0o755)
	if settings != "" {
		os.WriteFile(filepath.Join(dir, ".fairshare.json"), []byte(settings), 0o644)
	}
	return dir
}

func TestARepoCanSayACheckTakesItsOwnSlot(t *testing.T) {
	dir := repoWith(t, `{"ownSlot": ["bun run lane:close", "bun run verify:fast"]}`)
	sub := filepath.Join(dir, "packages", "core")
	os.MkdirAll(sub, 0o755)
	for _, c := range []string{"bun run lane:close", "bun run lane:close -m \"Fixed: x\"", "bun run verify:fast --landing"} {
		if !ownSlot(c, dir) || !ownSlot(c, sub) {
			t.Error("should take its own slot:", c)
		}
	}
	for _, c := range []string{"bun run check", "bun run verify:fast-ish", "bun run lane:closeall"} {
		if ownSlot(c, dir) {
			t.Error("should not:", c)
		}
	}
	// A checkout without the file, or with a broken one, still waits.
	if ownSlot("bun run lane:close", repoWith(t, "")) || ownSlot("bun run lane:close", repoWith(t, "{not json")) {
		t.Error("no settings means the slot is held around the command")
	}
	if ownSlot("bun run lane:close", t.TempDir()) {
		t.Error("outside a repository there are no settings")
	}
}

func TestTheHookLeavesASelfSlottingCheckToItself(t *testing.T) {
	bg := map[string]any{"run_in_background": true}
	own := repoWith(t, `{"ownSlot": ["bun run lane:close"]}`)
	if d := testCall(t, own, "bun run lane:close", "bash", "claude", bg); slicesContains(d.Rules, "slot") {
		t.Error("the repository takes its own slot:", d)
	}
	if d := testCall(t, repoWith(t, ""), "bun run lane:close", "bash", "claude", bg); !slicesContains(d.Rules, "slot") {
		t.Error("an older checkout without the file still waits:", d)
	}
	if d := testCall(t, own, "bun run check", "bash", "claude", bg); !slicesContains(d.Rules, "slot") {
		t.Error("an unlisted check still waits:", d)
	}
}
