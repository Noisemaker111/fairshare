package main

// What the meter saw, one line a minute, summed up: how full the machine ran, how long work waited
// for a processor, and how often the processors kept for the user sat unused while it waited. The
// last number is the evidence for or against letting agent work onto them while nobody is there.

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type historyLineRead struct {
	At         time.Time `json:"at"`
	Capacity   float64   `json:"capacity"`
	Busy       float64   `json:"busy"`
	Waiting    float64   `json:"waiting"`
	UserBusy   float64   `json:"user_busy"`
	Reserved   int       `json:"reserved"`
	MemAvailGB float64   `json:"mem_avail_gb"`
	DiskFreeGB float64   `json:"disk_free_gb"`
	Presence   string    `json:"presence"`
}

// Summary is the history over a span of time.
type Summary struct {
	Hours   float64 `json:"hours_asked"`
	Minutes int     `json:"minutes_measured"`
	// MeanBusy and MeanCapacity are processors; FullShare is the share of minutes in which work
	// wanted more processors than it may use.
	MeanBusy     float64 `json:"mean_busy"`
	MeanCapacity float64 `json:"mean_capacity"`
	FullShare    float64 `json:"full_share"`
	// WaitedMinutes are minutes in which at least one thread on average waited for a processor,
	// and ReserveIdleMinutes those of them in which half or more of the reserved processors
	// were unused.
	WaitedMinutes      int            `json:"waited_minutes"`
	ReserveIdleMinutes int            `json:"reserve_idle_while_waiting_minutes"`
	LowestMemGB        float64        `json:"lowest_mem_avail_gb"`
	LowestDiskGB       float64        `json:"lowest_disk_free_gb"`
	Presence           map[string]int `json:"minutes_by_presence"`
}

func summarize(lines []historyLineRead, hours float64) Summary {
	s := Summary{Hours: hours, Presence: map[string]int{}}
	var busy, capacity float64
	full := 0
	for _, l := range lines {
		if s.Minutes == 0 || l.MemAvailGB < s.LowestMemGB {
			s.LowestMemGB = l.MemAvailGB
		}
		if s.Minutes == 0 || l.DiskFreeGB < s.LowestDiskGB {
			s.LowestDiskGB = l.DiskFreeGB
		}
		s.Minutes++
		busy += l.Busy
		capacity += l.Capacity
		s.Presence[l.Presence]++
		if l.Capacity > 0 && l.Busy >= fullShare*l.Capacity {
			full++
		}
		if l.Waiting >= 1 {
			s.WaitedMinutes++
			if l.Reserved > 0 && l.UserBusy <= float64(l.Reserved)/2 {
				s.ReserveIdleMinutes++
			}
		}
	}
	if s.Minutes > 0 {
		s.MeanBusy, s.MeanCapacity = round2(busy/float64(s.Minutes)), round2(capacity/float64(s.Minutes))
		s.FullShare = round2(float64(full) / float64(s.Minutes))
	}
	return s
}

func readHistory(since time.Time) ([]historyLineRead, error) {
	var out []historyLineRead
	for _, name := range []string{"history.jsonl.1", "history.jsonl"} {
		f, err := os.Open(filepath.Join(homeDir(), name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			var l historyLineRead
			if json.Unmarshal(sc.Bytes(), &l) == nil && !l.At.Before(since) {
				out = append(out, l)
			}
		}
		f.Close()
	}
	return out, nil
}

func cmdHistory(args []string) error {
	fs := flag.NewFlagSet("history", flag.ExitOnError)
	hours := fs.Float64("hours", 24, "how far back")
	asJSON := fs.Bool("json", false, "print as JSON")
	fs.Parse(args)
	lines, err := readHistory(time.Now().Add(-time.Duration(*hours * float64(time.Hour))))
	if err != nil {
		return err
	}
	s := summarize(lines, *hours)
	if *asJSON {
		return printJSON(s)
	}
	if s.Minutes == 0 {
		fmt.Printf("The meter has recorded nothing in the last %.0f hours.\n", *hours)
		return nil
	}
	fmt.Printf("Last %.0f hours, %d minutes measured:\n", *hours, s.Minutes)
	fmt.Printf("  agent processors in use      %.1f of %.0f on average\n", s.MeanBusy, s.MeanCapacity)
	fmt.Printf("  minutes work wanted more     %.0f%%\n", s.FullShare*100)
	fmt.Printf("  minutes a thread waited      %d, of them %d with half or more of your processors unused\n", s.WaitedMinutes, s.ReserveIdleMinutes)
	fmt.Printf("  least memory free            %.1f GB\n", s.LowestMemGB)
	fmt.Printf("  least disk free              %.1f GB\n", s.LowestDiskGB)
	for _, state := range []string{"full-screen", "active", "away", "none"} {
		if n := s.Presence[state]; n > 0 {
			fmt.Printf("  minutes %-20s %d\n", state, n)
		}
	}
	return nil
}
