package main

// The low-disk trigger. The hook already runs on every agent tool call, so it is where a full system
// drive is noticed without any timer: one GetDiskFreeSpaceEx call (microseconds) per call. When free
// space falls below the threshold Jon set in Owl (`storage.reclaim_free_gb`, read from Owl's own
// settings.json, 80 GB when unset), the hook starts `owl storage reclaim --apply --json` detached and
// outside the caller's job, so it outlives the call, and says so in one `degraded:` line. Owl takes a
// lock so only one reclaim runs; the hook also starts at most one per reclaimEvery, so a long reclaim
// is not restarted on every call. A reclaim frees disk, so it never waits for a heavy-job slot
// (freesDisk): on 2026-11-08 cleanups queued behind the very builds that were filling the disk.
//
// FAIRSHARE_OWL_SETTINGS and FAIRSHARE_OWL point the trigger at another settings file and owl.exe,
// for tests.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"github.com/Noisemaker111/fairshare/internal/windows"
)

const (
	defaultReclaimFreeGB = 80
	reclaimEvery         = 20 * time.Minute
	createBreakawayJob   = 0x01000000
)

var procGetDiskFreeSpaceExW = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetDiskFreeSpaceExW")

func owlHome() string { return filepath.Join(os.Getenv("LOCALAPPDATA"), "Owl") }

func owlSettingsFile() string {
	if p := os.Getenv("FAIRSHARE_OWL_SETTINGS"); p != "" {
		return p
	}
	return filepath.Join(owlHome(), "settings.json")
}

func owlExe() string {
	if p := os.Getenv("FAIRSHARE_OWL"); p != "" {
		return p
	}
	return filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "Owl", "v2", "owl.exe")
}

// reclaimFreeBytes is the free space Owl keeps on the system drive, from Owl's settings.
func reclaimFreeBytes() int64 {
	gb := int64(defaultReclaimFreeGB)
	if raw, err := os.ReadFile(owlSettingsFile()); err == nil {
		var s struct {
			StorageClean struct {
				ReclaimFreeGB *int64 `json:"reclaim_free_gb"`
			} `json:"storage_clean"`
		}
		if json.Unmarshal(raw, &s) == nil && s.StorageClean.ReclaimFreeGB != nil && *s.StorageClean.ReclaimFreeGB > 0 {
			gb = *s.StorageClean.ReclaimFreeGB
		}
	}
	return gb << 30
}

func systemDrive() string {
	if d := os.Getenv("SystemDrive"); d != "" {
		return d + `\`
	}
	return `C:\`
}

// freeBytes is the free space the calling user can use on the drive holding path.
func freeBytes(path string) (int64, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var free, total, all uint64
	r, _, err := procGetDiskFreeSpaceExW.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&free)), uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&all)))
	if r == 0 {
		return 0, err
	}
	return int64(free), nil
}

func reclaimMarker() string { return filepath.Join(eventsDir(), "reclaim-started") }

// lowDiskCheck starts a reclaim when the system drive is under Owl's threshold and none was started
// within reclaimEvery. It returns the line to show the agent, or "" when nothing was started.
func lowDiskCheck(now time.Time) string {
	free, err := freeBytes(systemDrive())
	if err != nil {
		return ""
	}
	want := reclaimFreeBytes()
	if free >= want {
		return ""
	}
	if info, err := os.Stat(reclaimMarker()); err == nil && now.Sub(info.ModTime()) < reclaimEvery {
		return ""
	}
	os.MkdirAll(eventsDir(), 0o755)
	if err := os.WriteFile(reclaimMarker(), []byte(now.UTC().Format(time.RFC3339)), 0o644); err != nil {
		return ""
	}
	exe := owlExe()
	pid, err := spawnDetached(fmt.Sprintf(`"%s" storage reclaim --apply --json`, exe))
	ev := map[string]any{"free_gb": free >> 30, "target_gb": want >> 30, "owl": exe}
	if err != nil {
		ev["error"] = err.Error()
		logEvent("lowdisk", ev)
		return fmt.Sprintf("degraded: low disk (%d GB free on %s, below %d GB) and owl storage reclaim could not start: %v", free>>30, systemDrive(), want>>30, err)
	}
	ev["pid"] = pid
	logEvent("lowdisk", ev)
	return fmt.Sprintf("degraded: low disk, reclaim started (%d GB free on %s, below %d GB; owl storage reclaim --apply runs as pid %d)", free>>30, systemDrive(), want>>30, pid)
}

// spawnDetached starts cmdline with no console, outside the caller's job when the job allows it, so
// it keeps running after the hook and the agent's command exit.
func spawnDetached(cmdline string) (uint32, error) {
	si := new(windows.StartupInfo)
	si.Cb = uint32(unsafe.Sizeof(*si))
	cl, err := windows.UTF16PtrFromString(cmdline)
	if err != nil {
		return 0, err
	}
	var last error
	for _, flags := range []uint32{windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP | createBreakawayJob, windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP} {
		pi := new(windows.ProcessInformation)
		if err := windows.CreateProcess(nil, cl, nil, nil, false, flags, nil, nil, si, pi); err != nil {
			last = err
			continue
		}
		windows.CloseHandle(pi.Thread)
		windows.CloseHandle(pi.Process)
		return pi.ProcessId, nil
	}
	return 0, last
}

// freesDisk reports whether cmd is an Owl reclaim, which must never queue behind heavy work.
func freesDisk(cmd string) bool {
	fields := strings.Fields(strings.ToLower(heavyKey(cmd)))
	for i := 0; i+2 < len(fields); i++ {
		exe := strings.TrimSuffix(filepath.Base(strings.ReplaceAll(fields[i], `\`, "/")), ".exe")
		if exe == "owl" && fields[i+1] == "storage" && fields[i+2] == "reclaim" {
			return true
		}
	}
	return false
}
