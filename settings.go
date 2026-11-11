package main

// Everything Jon may change is a number in settings.json under fairshare's home, read on every
// call. None of them is a count of threads: they are shares of a machine's real processors, memory
// and disk, so the same file is right on a 24-processor PC and an 8-processor Linux host.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// Settings are the numbers the room rule and the meter use.
type Settings struct {
	// CPUTarget is the share of the processors agent work may use that fairshare fills before it
	// says a machine is full, while someone is at the PC.
	CPUTarget float64 `json:"cpu_target"`
	// CPUTargetFullScreen is that share while something fills the screen in the foreground (a
	// game, a video).
	CPUTargetFullScreen float64 `json:"cpu_target_full_screen"`
	// CPUTargetAway is that share once nobody has touched the PC for AwayMinutes, and on a machine
	// with no screen.
	CPUTargetAway float64 `json:"cpu_target_away"`
	AwayMinutes   float64 `json:"away_minutes"`
	// MemReserveFraction of a machine's memory always stays free.
	MemReserveFraction float64 `json:"mem_reserve_fraction"`
	// DiskReserveGB always stays free on the disk work is checked out on.
	DiskReserveGB float64 `json:"disk_reserve_gb"`
	// ThreadCores, ThreadMemGB and ThreadDiskGB are the least a new thread is counted as until
	// its own load shows in the measurements. The machine's measured average per open thread is
	// used when that is more.
	ThreadCores  float64 `json:"thread_cores"`
	ThreadMemGB  float64 `json:"thread_mem_gb"`
	ThreadDiskGB float64 `json:"thread_disk_gb"`
	// SettleMinutes is how long a thread just let in is still counted at its estimate: a new
	// thread spends its first minutes reading and installing, so its real load does not show yet.
	SettleMinutes float64 `json:"settle_minutes"`
	// SampleSeconds is how often the meter looks at the PC.
	SampleSeconds float64 `json:"sample_seconds"`
	// ReservedProcessors pins how many of the first logical processors are kept for the user.
	// Unset, the meter works it out from what the user's own programs are measured using and
	// tells Owl, which holds agent work off them (reserve.go).
	ReservedProcessors *int `json:"reserved_processors,omitempty"`
	// ReserveMinProcessors is the least kept for the user, whoever is at the PC, so the desktop
	// answers at once when they come back. ReserveMaxShare is the most, as a share of the machine.
	ReserveMinProcessors int     `json:"reserve_min_processors"`
	ReserveMaxShare      float64 `json:"reserve_max_share"`
	// ReserveHeadroom multiplies the most the user's programs used in the last
	// ReserveWindowSeconds; ReserveHeadroomFullScreen does while something fills the screen.
	ReserveHeadroom           float64 `json:"reserve_headroom"`
	ReserveHeadroomFullScreen float64 `json:"reserve_headroom_full_screen"`
	ReserveWindowSeconds      float64 `json:"reserve_window_seconds"`
	// OwlPath is Owl's program, which applies the reserve. Unset: its usual place.
	OwlPath string `json:"owl_path,omitempty"`
	// DiskPath is the folder whose disk is measured. Unset: the home folder's disk.
	DiskPath string `json:"disk_path,omitempty"`
}

func defaultSettings() Settings {
	return Settings{
		CPUTarget:           0.90,
		CPUTargetFullScreen: 0.75,
		CPUTargetAway:       1.00,
		AwayMinutes:         10,
		MemReserveFraction:  0.125,
		DiskReserveGB:       15,
		ThreadCores:         1,
		ThreadMemGB:         2,
		ThreadDiskGB:        5,
		SettleMinutes:       5,
		SampleSeconds:       5,

		ReserveMinProcessors:      2,
		ReserveMaxShare:           0.5,
		ReserveHeadroom:           1.5,
		ReserveHeadroomFullScreen: 2,
		ReserveWindowSeconds:      180,
	}
}

// homeDir is where fairshare keeps its settings, the meter's reading and its history.
func homeDir() string {
	if h := os.Getenv("FAIRSHARE_HOME"); h != "" {
		return h
	}
	if runtime.GOOS == "windows" {
		return filepath.Join(os.Getenv("LOCALAPPDATA"), "fairshare")
	}
	if s := os.Getenv("XDG_STATE_HOME"); s != "" {
		return filepath.Join(s, "fairshare")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "fairshare")
}

func settingsFile() string { return filepath.Join(homeDir(), "settings.json") }

// loadSettings reads settings.json over the defaults. A missing file is the defaults; a file that
// does not parse or holds a number out of range is an error, never a silent default.
func loadSettings() (Settings, error) {
	s := defaultSettings()
	b, err := os.ReadFile(settingsFile())
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return defaultSettings(), fmt.Errorf("%s does not parse: %w", settingsFile(), err)
	}
	if err := s.check(); err != nil {
		return defaultSettings(), fmt.Errorf("%s: %w", settingsFile(), err)
	}
	return s, nil
}

func (s Settings) check() error {
	share := func(name string, v float64) error {
		if v <= 0 || v > 1 {
			return fmt.Errorf("%s is %v; it is a share between 0 and 1", name, v)
		}
		return nil
	}
	for _, c := range []struct {
		name string
		v    float64
	}{{"cpu_target", s.CPUTarget}, {"cpu_target_full_screen", s.CPUTargetFullScreen}, {"cpu_target_away", s.CPUTargetAway}} {
		if err := share(c.name, c.v); err != nil {
			return err
		}
	}
	if s.MemReserveFraction < 0 || s.MemReserveFraction >= 1 {
		return fmt.Errorf("mem_reserve_fraction is %v; it is a share from 0 up to 1", s.MemReserveFraction)
	}
	for _, c := range []struct {
		name string
		v    float64
	}{{"away_minutes", s.AwayMinutes}, {"thread_cores", s.ThreadCores}, {"thread_mem_gb", s.ThreadMemGB}, {"settle_minutes", s.SettleMinutes}, {"sample_seconds", s.SampleSeconds},
		{"reserve_headroom", s.ReserveHeadroom}, {"reserve_headroom_full_screen", s.ReserveHeadroomFullScreen}, {"reserve_window_seconds", s.ReserveWindowSeconds}} {
		if c.v <= 0 {
			return fmt.Errorf("%s is %v; it must be more than 0", c.name, c.v)
		}
	}
	if s.DiskReserveGB < 0 || s.ThreadDiskGB < 0 {
		return fmt.Errorf("disk_reserve_gb and thread_disk_gb cannot be negative")
	}
	if s.ReservedProcessors != nil && *s.ReservedProcessors < 0 {
		return fmt.Errorf("reserved_processors cannot be negative")
	}
	if s.ReserveMinProcessors < 0 {
		return fmt.Errorf("reserve_min_processors cannot be negative")
	}
	if s.ReserveMaxShare <= 0 || s.ReserveMaxShare >= 1 {
		return fmt.Errorf("reserve_max_share is %v; it is a share between 0 and 1", s.ReserveMaxShare)
	}
	return nil
}
