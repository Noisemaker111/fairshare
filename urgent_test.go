package main

import (
	"strings"
	"testing"
	"time"
)

// gates is the PC of 2026-11-11 10:40: two Owl Rust ports' gates fill 17 of the 18 agent
// processors (6 kept for Jon), all of it agent work Owl would hold back, and Jon's own and
// Windows' work on the agent processors is 1.
func gates(state string) Reading {
	r := pc(17, 16, 30, 60, state)
	r.Processors, r.Reserved, r.Capacity = 24, 6, 18
	r.FirmShort, r.FirmLong = 1, 1.2
	return r
}

func TestAnUrgentLaunchIsJudgedByTheWorkThatWouldNotStepAside(t *testing.T) {
	s := defaultSettings()
	afg := []level{{Name: "appforgutters threads here", Minutes: 400, Cores: 0.9, MeanSq: 0.81, MemGB: 1.5, Own: true}}
	cases := []struct {
		name string
		r    Reading
		ask  Ask
		ok   bool
		says string
	}{
		{"the production fix of 2026-11-11 was refused", gates("active"), Ask{Threads: 4, Learned: afg}, false, "no room"},
		{"marked urgent it gets room", gates("active"), Ask{Threads: 4, Learned: afg, Urgent: true}, true, "held at idle priority"},
		{"urgent counts its repo's own sessions", gates("active"), Ask{Threads: 4, Learned: afg, Urgent: true}, true, "appforgutters threads here were measured at 0.9"},
		{"urgent does not pass work that will not step aside", func() Reading { r := gates("active"); r.FirmShort = 16.5; return r }(), Ask{Urgent: true}, false, "Even with other agent work held back"},
		{"urgent still keeps memory free", func() Reading { r := gates("active"); r.MemAvailGB = 8.5; return r }(), Ask{Urgent: true}, false, "GB is kept free"},
		{"urgent still leaves the reserve to Jon", func() Reading {
			r := gates("full-screen")
			r.Reserved, r.Capacity, r.FirmShort, r.FirmLong = 12, 12, 8, 8
			return r
		}(), Ask{Urgent: true, Explicit: Need{Cores: 1}}, true, "of the 12 processors agent work may use (limit 9.0)"},
		{"and is refused once the reserve leaves too little", func() Reading {
			r := gates("full-screen")
			r.Reserved, r.Capacity, r.FirmShort, r.FirmLong = 12, 12, 8.5, 8.5
			return r
		}(), Ask{Urgent: true, Explicit: Need{Cores: 1}}, false, "the limit is 9.0 (75%, because something is full screen)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := decide(c.r, s, c.ask, Need{})
			if v.OK != c.ok {
				t.Fatalf("ok = %v, want %v: %s", v.OK, c.ok, v.Reason)
			}
			if !strings.Contains(v.Reason, c.says) {
				t.Fatalf("reason lacks %q: %s", c.says, v.Reason)
			}
		})
	}
}

func TestFirmCoresAreWhatDoesNotStepAside(t *testing.T) {
	// No hold yet: a hold would put every agent session behind the urgent one.
	if got := firmCores(17, 16, 0, false); got != 1 {
		t.Fatalf("firm without a hold = %v, want 1", got)
	}
	// A hold stands: only the work Owl already holds at idle steps aside; the urgent session
	// (and anything let in since) is firm.
	if got := firmCores(17, 16, 12, true); got != 5 {
		t.Fatalf("firm under a hold = %v, want 5", got)
	}
	if got := firmCores(2, 5, 5, false); got != 0 {
		t.Fatalf("firm is never below 0, got %v", got)
	}
}

func TestWhatMakesALaunchUrgent(t *testing.T) {
	s := defaultSettings()
	for _, c := range []struct {
		flag        bool
		label, repo string
		want        bool
	}{
		{true, "anything", "", true},
		{false, "AppForGutters production fix: uploads failing", "appforgutters", true},
		{false, "Hotfix the false offline banner", "", true},
		{false, "Port the computer engine to Rust", "owl-go", false},
		{false, "Productionize nothing", "", false},
	} {
		if got, why := isUrgent(c.flag, c.label, c.repo, s); got != c.want {
			t.Errorf("%q: urgent %v (%s), want %v", c.label, got, why, c.want)
		}
	}
	s.UrgentWords = nil
	if got, _ := isUrgent(false, "production fix", "", s); got {
		t.Error("with no urgent words a title made a launch urgent")
	}
}

func TestAnUrgentHoldStandsWhileItsSessionsRunAndNoLonger(t *testing.T) {
	s := defaultSettings()
	t0 := time.Date(2026, 11, 11, 10, 40, 0, 0, time.UTC)
	h := urgentHold{}.join(t0, "AppForGutters production fix", false)
	if !h.live(t0.Add(10*time.Minute), false, s) {
		t.Fatal("a hold ended inside its least minutes")
	}
	if h.live(t0.Add(25*time.Minute), false, s) {
		t.Fatal("a hold with no session started since it began outlived its least minutes")
	}
	if !h.live(t0.Add(90*time.Minute), true, s) {
		t.Fatal("a hold ended while its session still ran")
	}
	if h.live(t0.Add(121*time.Minute), true, s) {
		t.Fatal("a hold outlived urgent_most_minutes")
	}
	// A second urgent launch under a standing hold keeps its start, so the first never yields.
	later := h.join(t0.Add(30*time.Minute), "second fix", true)
	if !later.Since.Equal(t0) || !later.Last.Equal(t0.Add(30*time.Minute)) || len(later.Labels) != 2 {
		t.Fatalf("joined hold = %+v", later)
	}
	if fresh := h.join(t0.Add(3*time.Hour), "next day", false); !fresh.Since.Equal(t0.Add(3*time.Hour)) || len(fresh.Labels) != 1 {
		t.Fatalf("a hold after one ended = %+v", fresh)
	}
}

func TestGrantsBeforeAnUrgentHoldStepAsideWithTheRest(t *testing.T) {
	t.Setenv("FAIRSHARE_HOME", t.TempDir())
	now := time.Now()
	settle := 5 * time.Minute
	takeGrant(now.Add(-3*time.Minute), settle, grant{Need: Need{Cores: 4.5}, Label: "owl port"})
	takeGrant(now.Add(-time.Minute), settle, grant{Need: Need{Cores: 1}, Label: "urgent fix"})
	if got := pendingSinceNeed(now, settle, now.Add(-2*time.Minute)); got.Cores != 1 {
		t.Fatalf("pending since the hold = %v, want 1", got.Cores)
	}
	if got := pendingNeed(now, settle, nil); got.Cores != 5.5 {
		t.Fatalf("pending = %v, want 5.5", got.Cores)
	}
}
