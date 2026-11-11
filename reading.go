package main

// A reading is what one machine has and what the work on it is taking, in the same words on every
// machine: logical processors, GB of memory, GB of disk. On Windows the meter builds it from a
// look every few seconds; on Linux the kernel already keeps the averages.

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Presence is what the person at the machine is doing, found out and never told.
type Presence struct {
	// State is "full-screen" (something fills the screen in the foreground: a game, a video),
	// "active" (keyboard or mouse used lately), "away", or "none" on a machine with no screen.
	State       string  `json:"state"`
	App         string  `json:"app,omitempty"`
	IdleMinutes float64 `json:"idle_minutes"`
}

// Program is one program's share of the low-priority work over the short window.
type Program struct {
	Name      string  `json:"name"`
	Cores     float64 `json:"cores"`
	MemGB     float64 `json:"mem_gb"`
	Processes int     `json:"processes"`
}

// Reading is one machine now.
type Reading struct {
	Machine string    `json:"machine"`
	OS      string    `json:"os"`
	At      time.Time `json:"at"`

	// Processors is every logical processor; Reserved of them are kept for the user and Capacity
	// is what agent work may use.
	Processors int     `json:"processors"`
	Reserved   int     `json:"reserved"`
	Capacity   float64 `json:"capacity"`
	// ReserveWhy says what the reserve was worked out from; UserCores is what the user's own
	// programs used in the last look.
	ReserveWhy string  `json:"reserve_why,omitempty"`
	UserCores  float64 `json:"user_cores"`

	// The two windows the averages below cover, in seconds.
	ShortSeconds float64 `json:"short_seconds"`
	LongSeconds  float64 `json:"long_seconds"`
	// Busy is processors in use on the agent processors; Waiting is threads that want a
	// processor and have none; Demand is both, the processors the work would use if it had them.
	BusyShort    float64 `json:"busy_short"`
	BusyLong     float64 `json:"busy_long"`
	WaitingShort float64 `json:"waiting_short"`
	WaitingLong  float64 `json:"waiting_long"`
	DemandShort  float64 `json:"demand_short"`
	DemandLong   float64 `json:"demand_long"`
	// UserBusy is processors in use on the reserved processors.
	UserBusy float64 `json:"user_busy"`

	MemTotalGB float64 `json:"mem_total_gb"`
	// MemAvailGB is the least that was free during the short window.
	MemAvailGB float64 `json:"mem_avail_gb"`
	// WorkCores and WorkMemGB are what the low-priority work (agent work, as Owl holds it) takes.
	WorkCores float64 `json:"work_cores"`
	WorkMemGB float64 `json:"work_mem_gb"`

	DiskPath   string  `json:"disk_path"`
	DiskFreeGB float64 `json:"disk_free_gb"`

	Presence Presence  `json:"presence"`
	Programs []Program `json:"programs,omitempty"`
	// Others are the busiest programs that are not low-priority work: the user's own apps and
	// Windows itself.
	Others []Program `json:"others,omitempty"`
	// Notes name anything the reading could not measure.
	Notes []string `json:"notes,omitempty"`
}

// sample is one look at the machine.
type sample struct {
	at         time.Time
	seconds    float64 // since the look before
	busy       float64 // processors in use on the agent processors over that time
	userBusy   float64 // and on the reserved ones
	waiting    float64 // threads ready for a processor at the instant of the look
	memAvailGB float64
	workCores  float64
	programs   map[string]float64 // CPU-seconds by program, low-priority work only
	others     map[string]float64 // and of everything else
}

// window keeps the looks of the long window.
type window struct {
	samples []sample
}

func (w *window) add(s sample, long time.Duration) {
	w.samples = append(w.samples, s)
	cut := s.at.Add(-long)
	i := 0
	for i < len(w.samples)-1 && w.samples[i].at.Before(cut) {
		i++
	}
	w.samples = w.samples[i:]
}

// means averages the looks no older than d, each weighted by the time it covers.
func (w *window) means(now time.Time, d time.Duration) (busy, waiting, userBusy, workCores, minMem float64, programs, others map[string]float64, seconds float64) {
	minMem = math.Inf(1)
	programs, others = map[string]float64{}, map[string]float64{}
	cut := now.Add(-d)
	for _, s := range w.samples {
		if !s.at.After(cut) || s.seconds <= 0 { // a look covers the time up to it
			continue
		}
		seconds += s.seconds
		busy += s.busy * s.seconds
		waiting += s.waiting * s.seconds
		userBusy += s.userBusy * s.seconds
		workCores += s.workCores * s.seconds
		minMem = math.Min(minMem, s.memAvailGB)
		for name, cpu := range s.programs {
			programs[name] += cpu
		}
		for name, cpu := range s.others {
			others[name] += cpu
		}
	}
	if seconds == 0 {
		return 0, 0, 0, 0, 0, programs, others, 0
	}
	return busy / seconds, waiting / seconds, userBusy / seconds, workCores / seconds, minMem, programs, others, seconds
}

// topPrograms turns CPU-seconds by program into the busiest few, in processors.
func topPrograms(cpu map[string]float64, seconds float64, mem map[string]float64, count map[string]int, n int) []Program {
	if seconds <= 0 {
		return nil
	}
	out := make([]Program, 0, len(cpu))
	for name, c := range cpu {
		if cores := c / seconds; cores >= 0.05 {
			out = append(out, Program{Name: name, Cores: round2(cores), MemGB: round2(mem[name]), Processes: count[name]})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Cores != out[j].Cores {
			return out[i].Cores > out[j].Cores
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

func stateFile() string { return filepath.Join(homeDir(), "reading.json") }

// writeReading replaces the reading file whole, so a reader never sees half of one.
func writeReading(r Reading) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	tmp := stateFile() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, stateFile())
}

func readReadingFile(path string) (Reading, error) {
	var r Reading
	b, err := os.ReadFile(path)
	if err != nil {
		return r, err
	}
	return r, json.Unmarshal(b, &r)
}
