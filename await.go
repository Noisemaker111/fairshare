package main

// `fairshare await <file> [maxS] [quietS]` waits for a Claude background task's output file to
// finish. It returns when the exit marker appears, after quietS seconds with no new output (a
// server never exits), or after maxS seconds.

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"golang.org/x/sys/windows"
)

func awaitTask(file string, maxWait, quiet time.Duration) string {
	if taskFinished(file) {
		return "finished"
	}
	note, err := windows.FindFirstChangeNotification(filepath.Dir(file), false, windows.FILE_NOTIFY_CHANGE_SIZE|windows.FILE_NOTIFY_CHANGE_LAST_WRITE|windows.FILE_NOTIFY_CHANGE_FILE_NAME)
	if err != nil {
		note = 0
	} else {
		defer windows.FindCloseChangeNotification(note)
	}
	size := func() int64 {
		st, err := os.Stat(file)
		if err != nil {
			return -1
		}
		return st.Size()
	}
	start, lastSize, lastChange := time.Now(), size(), time.Now()
	for {
		if taskFinished(file) {
			return "finished"
		}
		now := time.Now()
		if now.Sub(start) >= maxWait {
			return "max"
		}
		if now.Sub(lastChange) >= quiet {
			return "quiet"
		}
		// The notification wakes us at once on a write; the 2-second cap catches writes Windows
		// only reports once its cache flushes.
		wait := min(2*time.Second, maxWait-now.Sub(start), quiet-now.Sub(lastChange))
		if note != 0 {
			if ev, _ := windows.WaitForSingleObject(note, uint32(wait.Milliseconds())); ev == windows.WAIT_OBJECT_0 {
				windows.FindNextChangeNotification(note)
			}
		} else {
			time.Sleep(wait)
		}
		if s := size(); s != lastSize {
			lastSize, lastChange = s, time.Now()
		}
	}
}

func cmdAwait(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: fairshare await <file> [maxS=300] [quietS=20]")
		return 2
	}
	maxS, quietS := 300, 20
	if len(args) > 1 {
		maxS, _ = strconv.Atoi(args[1])
	}
	if len(args) > 2 {
		quietS, _ = strconv.Atoi(args[2])
	}
	t0 := time.Now()
	why := awaitTask(args[0], time.Duration(maxS)*time.Second, time.Duration(quietS)*time.Second)
	logEvent("await", map[string]any{"file": clip(args[0], 300), "result": why, "waited_ms": time.Since(t0).Milliseconds()})
	if why != "finished" {
		fmt.Printf("[fairshare: task still running after waiting (%s); output so far:]\n", why)
	}
	return 0
}
