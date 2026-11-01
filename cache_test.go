package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one"), 0o644)
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("ignored/\n"), 0o644)
	run("add", ".")
	run("commit", "-qm", "init")
	return dir
}

func TestCacheKeyFollowsContentNotPath(t *testing.T) {
	a, b := gitRepo(t), gitRepo(t) // same content, different folders
	key := func(dir string) string {
		k, err := CacheKey(dir, []string{"bun", "test"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	base := key(a)
	if key(b) != base {
		t.Fatal("two checkouts of the same content got different keys")
	}
	if k, _ := CacheKey(a, []string{"bun", "lint"}, nil); k == base {
		t.Fatal("a different command got the same key")
	}
	os.MkdirAll(filepath.Join(a, "ignored"), 0o755)
	os.WriteFile(filepath.Join(a, "ignored", "out.js"), []byte("x"), 0o644)
	if key(a) != base {
		t.Fatal("an ignored build output changed the key")
	}
	os.WriteFile(filepath.Join(a, "a.txt"), []byte("two"), 0o644)
	edited := key(a)
	if edited == base {
		t.Fatal("editing a tracked file did not change the key")
	}
	os.WriteFile(filepath.Join(a, "new.txt"), []byte("n"), 0o644)
	if key(a) == edited {
		t.Fatal("a new untracked file did not change the key")
	}
	os.Remove(filepath.Join(a, "new.txt"))
	os.WriteFile(filepath.Join(a, "a.txt"), []byte("one"), 0o644)
	if key(a) != base {
		t.Fatal("undoing the edits did not give back the original key")
	}
}

func TestWithCacheRunsOnceForConcurrentCallers(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	var runs atomic.Int32
	var wg sync.WaitGroup
	codes := make([]int, 16)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = WithCache("ab"+"cd1", false, func(stdout, stderr io.Writer) int {
				runs.Add(1)
				time.Sleep(300 * time.Millisecond)
				io.WriteString(stdout, "ok\n")
				return 3
			})
		}()
	}
	wg.Wait()
	if n := runs.Load(); n != 1 {
		t.Fatalf("command ran %d times for 16 concurrent callers, want 1", n)
	}
	for i, c := range codes {
		if c != 3 {
			t.Fatalf("caller %d got exit %d, want the shared 3", i, c)
		}
	}
	// a failure is shared with callers that were waiting, but not kept for later ones
	WithCache("abcd1", false, func(io.Writer, io.Writer) int { runs.Add(1); return 0 })
	if n := runs.Load(); n != 2 {
		t.Fatalf("a later caller reused a failure (runs=%d)", n)
	}
	WithCache("abcd1", false, func(io.Writer, io.Writer) int { runs.Add(1); return 0 })
	if n := runs.Load(); n != 2 {
		t.Fatalf("a success was not reused (runs=%d)", n)
	}
}
