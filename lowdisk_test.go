package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestFreesDiskMatchesOnlyAnOwlReclaim(t *testing.T) {
	for cmd, want := range map[string]bool{
		"owl storage reclaim --apply": true,
		`"C:\Users\Jk101\AppData\Local\Programs\Owl\v2\owl.exe" storage reclaim --apply --json`: true,
		"./run/owl-reclaim.exe storage reclaim":                                                 false,
		"owl storage scan C:\\":                                                                 false,
		"go test ./... && owl storage reclaim":                                                  true,
		"echo storage reclaim":                                                                  false,
	} {
		if got := freesDisk(cmd); got != want {
			t.Errorf("freesDisk(%q) = %v, want %v", cmd, got, want)
		}
	}
	if _, listed := heavyListed(t.TempDir(), "owl storage reclaim --apply"); listed {
		t.Error("a reclaim was listed as heavy")
	}
}

func TestLowDiskStartsOneReclaimBelowOwlsThreshold(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LOCALAPPDATA", dir)
	t.Setenv("FAIRSHARE_EVENTS", filepath.Join(dir, "events.jsonl"))
	settings := filepath.Join(dir, "settings.json")
	t.Setenv("FAIRSHARE_OWL_SETTINGS", settings)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAIRSHARE_OWL", exe)
	ran := filepath.Join(dir, "ran.txt")
	t.Setenv("FAIRSHARE_TEST_FAKE_OWL", ran)

	free, err := freeBytes(systemDrive())
	if err != nil || free <= 0 {
		t.Fatalf("free space: %d %v", free, err)
	}
	above := free>>30 + 1000
	os.WriteFile(settings, []byte(`{"storage_clean":{"reclaim_free_gb":`+strconv.FormatInt(above, 10)+`}}`), 0o644)
	note := lowDiskCheck(time.Now())
	if !strings.HasPrefix(note, "degraded: low disk, reclaim started") {
		t.Fatalf("note %q", note)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		if b, err := os.ReadFile(ran); err == nil {
			if string(b) != "storage reclaim --apply --json" {
				t.Fatalf("owl ran with %q", b)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the reclaim never ran")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if again := lowDiskCheck(time.Now()); again != "" {
		t.Errorf("a second reclaim started within %s: %q", reclaimEvery, again)
	}
	if later := lowDiskCheck(time.Now().Add(reclaimEvery + time.Minute)); later == "" {
		t.Error("no reclaim after the quiet window")
	}

	os.WriteFile(settings, []byte(`{"storage_clean":{"reclaim_free_gb":1}}`), 0o644)
	os.Remove(reclaimMarker())
	if note := lowDiskCheck(time.Now()); note != "" {
		t.Errorf("started a reclaim above the threshold: %q", note)
	}
	os.Remove(settings)
	if got := reclaimFreeBytes(); got != defaultReclaimFreeGB<<30 {
		t.Errorf("unset threshold %d", got)
	}
}
