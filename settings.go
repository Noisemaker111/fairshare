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
	"time"
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
	// ThreadCores and ThreadMemGB are what a piece of work counts as while nothing like it has been
	// measured anywhere: where the estimate starts, not a least once it is measured (learn.go).
	// ThreadDiskGB is what a checkout counts as.
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
	// ReserveHeadroomFullScreen multiplies the most a running game's own processes used in the last
	// ReserveWindowSeconds. ReserveHeadroom is no longer used: with no game the reserve is
	// ReserveMinProcessors (Jon, 2026-11-11).
	ReserveHeadroom           float64 `json:"reserve_headroom"`
	ReserveHeadroomFullScreen float64 `json:"reserve_headroom_full_screen"`
	ReserveWindowSeconds      float64 `json:"reserve_window_seconds"`
	// OwlPath is Owl's program, which applies the reserve. Unset: its usual place.
	OwlPath string `json:"owl_path,omitempty"`
	// QueueNotify is the command the meter runs to tell a thread that room is held for its
	// launch (one JSON notice on its stdin). QueueHoldMinutes is how long the room stays held
	// before the next in line is asked; after QueueOffers untaken offers, or QueueMaxHours in the
	// line, a launch leaves it.
	QueueNotify []string `json:"queue_notify,omitempty"`
	// QueuePlace is the command the meter runs to pick a machine for a waiting launch that can
	// run on more than this one: {"entry", "local", "usable"} on its stdin, {"machine": "<name>"}
	// or {"machine": ""} on its stdout. It is asked at most every QueuePlaceSeconds.
	QueuePlace []string `json:"queue_place,omitempty"`
	// QueueOrder is the command that says which waiting launches go first: the line as JSON on
	// its stdin, the ids that go to the front, in order, as a JSON list on its stdout. Launches
	// it does not name keep their order behind those. Asked at most every QueuePlaceSeconds.
	QueueOrder        []string `json:"queue_order,omitempty"`
	QueuePlaceSeconds float64  `json:"queue_place_seconds"`
	QueueHoldMinutes  float64  `json:"queue_hold_minutes"`
	QueueOffers       int      `json:"queue_offers"`
	QueueMaxHours     float64  `json:"queue_max_hours"`
	// Games are programs (image names) that make the reserve game-sized while they run, full screen
	// or not; a program seen full screen is learned as one too, unless NotGames names it (a browser
	// playing a video full screen is not a game).
	Games    []string `json:"games"`
	NotGames []string `json:"not_games"`
	// DiskPath is the folder whose disk is measured. Unset: the home folder's disk.
	DiskPath string `json:"disk_path,omitempty"`
	// UrgentWords make a launch urgent when its title or repo holds one (case does not matter);
	// --urgent does too. An urgent launch is judged against the work that would not step aside
	// for it, and once let in, every agent session older than it is held at idle priority by Owl
	// for UrgentLeastMinutes, then while a session started since is running, and never longer
	// than UrgentMostMinutes after the latest urgent launch.
	UrgentWords        []string `json:"urgent_words"`
	UrgentLeastMinutes float64  `json:"urgent_least_minutes"`
	UrgentMostMinutes  float64  `json:"urgent_most_minutes"`
	// Runners are the programs that are one agent session each, named with or without ".exe": the
	// work under each is measured as that thread's, and one started since an urgent hold began
	// keeps it standing.
	Runners []string `json:"runners"`
	// RepoDays and RepoMinMinutes are no longer used: what a repo's work takes is learned by
	// learn.go, which has no window and no threshold.
	RepoDays       float64 `json:"repo_days"`
	RepoMinMinutes float64 `json:"repo_min_minutes"`
	// LearnTrustMinutes is how many measured minutes of a kind of work weigh as much as everything
	// wider that is known about it; LearnHalfLifeDays how fast old minutes fade.
	LearnTrustMinutes float64 `json:"learn_trust_minutes"`
	LearnHalfLifeDays float64 `json:"learn_half_life_days"`
	// LearnTrustRuns is how many finished runs of a command weigh as much as LearnTrustMinutes.
	LearnTrustRuns float64 `json:"learn_trust_runs"`
	// LearnRecentMinutes is the half-life of a running piece of work's own minutes: what it did
	// lately says more about its next minutes than what it did an hour ago.
	LearnRecentMinutes float64 `json:"learn_recent_minutes"`
	// LearnMargin is how much of the open work's swing (the spread of its measured minutes, taken
	// together) is kept free on top of what it is expected to use. 0 keeps none.
	LearnMargin float64 `json:"learn_margin"`
	// LearnMemMargin is the same for memory. It is more than LearnMargin because the two do not
	// fail alike: work short of processors runs slower, work short of memory is ended.
	LearnMemMargin float64 `json:"learn_mem_margin"`
}

func (s Settings) halfLife() time.Duration {
	return time.Duration(s.LearnHalfLifeDays * 24 * float64(time.Hour))
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
		Games:                     []string{"7daystodie.exe"},
		NotGames:                  []string{"chrome.exe", "msedge.exe", "firefox.exe", "vlc.exe", "explorer.exe", "applicationframehost.exe", "t3 code.exe", "t3 code (nightly).exe", "owl.exe"},

		QueueHoldMinutes:  10,
		QueuePlaceSeconds: 30,
		QueueOffers:       6,
		QueueMaxHours:     24,

		UrgentWords:        []string{"urgent", "production", "prod fix", "hotfix", "outage"},
		UrgentLeastMinutes: 20,
		UrgentMostMinutes:  120,
		Runners:            []string{"claude.exe", "codex.exe", "opencode.exe"},
		RepoDays:           7,
		RepoMinMinutes:     30,

		LearnTrustMinutes:  10,
		LearnHalfLifeDays:  3,
		LearnTrustRuns:     1,
		LearnRecentMinutes: 30,
		LearnMargin:        1,
		LearnMemMargin:     2,
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
		{"reserve_headroom", s.ReserveHeadroom}, {"reserve_headroom_full_screen", s.ReserveHeadroomFullScreen}, {"reserve_window_seconds", s.ReserveWindowSeconds},
		{"queue_hold_minutes", s.QueueHoldMinutes}, {"urgent_least_minutes", s.UrgentLeastMinutes}, {"urgent_most_minutes", s.UrgentMostMinutes}, {"repo_days", s.RepoDays}, {"repo_min_minutes", s.RepoMinMinutes}, {"queue_place_seconds", s.QueuePlaceSeconds}, {"queue_max_hours", s.QueueMaxHours}, {"queue_offers", float64(s.QueueOffers)},
		{"learn_trust_minutes", s.LearnTrustMinutes}, {"learn_half_life_days", s.LearnHalfLifeDays}, {"learn_recent_minutes", s.LearnRecentMinutes}, {"learn_trust_runs", s.LearnTrustRuns}} {
		if c.v <= 0 {
			return fmt.Errorf("%s is %v; it must be more than 0", c.name, c.v)
		}
	}
	if s.LearnMargin < 0 {
		return fmt.Errorf("learn_margin is %v; it cannot be negative", s.LearnMargin)
	}
	if s.LearnMemMargin < 0 {
		return fmt.Errorf("learn_mem_margin is %v; it cannot be negative", s.LearnMemMargin)
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
