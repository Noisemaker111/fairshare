package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Noisemaker111/fairshare/internal/windows"
)

// The cache makes a check that many sessions run on the same code cost one run. The key is the
// exact content of the working tree (committed tree plus every changed or new file, read without
// writing anything to the repo), the folder inside the repo, the command and chosen environment
// variables. The path of the worktree is not in the key, so 100 worktrees on the same code share.

const cacheVersion = "fairshare-cache-v1"

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"--no-optional-locks", "-C", dir}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %v %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// TreeState is the content identity of the working tree in dir: HEAD's tree plus the hash of every
// changed, new or deleted file that git does not ignore.
func TreeState(dir string) (root, prefix, state string, err error) {
	root, err = git(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return
	}
	root = strings.TrimSpace(root)
	if prefix, err = git(dir, "rev-parse", "--show-prefix"); err != nil {
		return
	}
	prefix = strings.TrimSpace(prefix)
	head, err := git(dir, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return
	}
	status, err := git(root, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		return
	}
	var lines []string
	fields := strings.Split(status, "\x00")
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if len(f) < 4 {
			continue
		}
		x, path := f[:2], f[3:]
		if x[0] == 'R' || x[0] == 'C' {
			i++ // the next field is the old name, which the new tree does not contain
		}
		sum := "deleted"
		if b, rerr := os.ReadFile(filepath.Join(root, filepath.FromSlash(path))); rerr == nil {
			h := sha256.Sum256(b)
			sum = hex.EncodeToString(h[:])
		} else if st, serr := os.Stat(filepath.Join(root, filepath.FromSlash(path))); serr == nil && st.IsDir() {
			sum = "dir" // a nested repository
		}
		lines = append(lines, path+"\x00"+sum)
	}
	slices.Sort(lines)
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00", strings.TrimSpace(head))
	for _, l := range lines {
		fmt.Fprintf(h, "%s\x00", l)
	}
	state = hex.EncodeToString(h.Sum(nil))
	return
}

// CacheKey identifies "this command on this exact code".
func CacheKey(dir string, argv, envKeys []string) (string, error) {
	_, prefix, state, err := TreeState(dir)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00", cacheVersion, state, prefix)
	for _, a := range argv {
		fmt.Fprintf(h, "arg=%s\x00", a)
	}
	for _, k := range slices.Sorted(slices.Values(envKeys)) {
		fmt.Fprintf(h, "env=%s=%s\x00", k, os.Getenv(k))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type cacheEntry struct {
	Exit     int       `json:"exit"`
	Seconds  float64   `json:"seconds"`
	Finished time.Time `json:"finished"`
}

func cacheDir(key string) string {
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "fairshare", "cache", key[:2], key)
}

func readEntry(dir string) (cacheEntry, bool) {
	var e cacheEntry
	b, err := os.ReadFile(filepath.Join(dir, "result.json"))
	if err != nil || json.Unmarshal(b, &e) != nil {
		return e, false
	}
	return e, true
}

func replay(dir string, e cacheEntry, shared bool) int {
	for _, s := range []struct {
		name string
		w    io.Writer
	}{{"stdout", os.Stdout}, {"stderr", os.Stderr}} {
		if f, err := os.Open(filepath.Join(dir, s.name)); err == nil {
			io.Copy(s.w, f)
			f.Close()
		}
	}
	how := "reused the result of the same check on the same code"
	if shared {
		how = "waited for the same check another session was already running"
	}
	fmt.Fprintf(os.Stderr, "fairshare: %s (exit %d, it took %.1fs, finished %s)\n", how, e.Exit, e.Seconds, e.Finished.Format("15:04:05"))
	return e.Exit
}

func lockFile(path string, wait bool) (windows.Handle, error) {
	p, _ := windows.UTF16PtrFromString(path)
	h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_ALWAYS, 0, 0)
	if err != nil {
		return 0, err
	}
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK)
	if !wait {
		flags |= windows.LOCKFILE_FAIL_IMMEDIATELY
	}
	ov := new(windows.Overlapped)
	if err := windows.LockFileEx(h, flags, 0, 1, 0, ov); err != nil {
		windows.CloseHandle(h)
		return 0, err
	}
	return h, nil // Windows drops the lock when this process ends, so a crash never wedges others
}

// WithCache runs run once per key across every session on the PC. A finished success is reused;
// a run already in progress is waited for and shared, success or failure; a failure is kept for
// later runs only with keepFailures, so a flaky failure is not repeated forever.
func WithCache(key string, keepFailures bool, run func(stdout, stderr io.Writer) int) int {
	arrived := time.Now()
	dir := cacheDir(key)
	if e, ok := readEntry(dir); ok && (e.Exit == 0 || keepFailures) {
		return replay(dir, e, false)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return run(os.Stdout, os.Stderr)
	}
	lock, err := lockFile(filepath.Join(dir, "lock"), false)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fairshare: the same check is already running in another session; waiting for its result")
		lock, err = lockFile(filepath.Join(dir, "lock"), true)
		if err != nil {
			return run(os.Stdout, os.Stderr)
		}
	}
	defer windows.CloseHandle(lock)
	if e, ok := readEntry(dir); ok && (e.Exit == 0 || keepFailures || e.Finished.After(arrived)) {
		return replay(dir, e, e.Finished.After(arrived))
	}
	outF, err1 := os.Create(filepath.Join(dir, "stdout"))
	errF, err2 := os.Create(filepath.Join(dir, "stderr"))
	if err := errors.Join(err1, err2); err != nil {
		return run(os.Stdout, os.Stderr)
	}
	t := time.Now()
	code := run(io.MultiWriter(os.Stdout, outF), io.MultiWriter(os.Stderr, errF))
	outF.Close()
	errF.Close()
	b, _ := json.Marshal(cacheEntry{Exit: code, Seconds: time.Since(t).Seconds(), Finished: time.Now()})
	tmp := filepath.Join(dir, "result.json.tmp")
	if os.WriteFile(tmp, b, 0o644) == nil {
		os.Rename(tmp, filepath.Join(dir, "result.json"))
	}
	return code
}
