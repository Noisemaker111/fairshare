package main

// How many processors to keep for the person at the PC, worked out from what their own programs
// are measured using instead of a fixed number. A fixed eight was wrong both ways: with nobody at
// the PC a third of it sat idle while agent work queued, and a game that wants ten got eight.
//
// The reserve is the most the user's own programs used in the last few minutes, with headroom, in
// whole physical cores (a thread on a core's second half still slows the first). It grows the
// moment their programs use more or have to wait for a processor, and shrinks only after the whole
// window has been quieter. Two things it remembers so it is right before the load shows: what each
// full-screen program needed the last time it ran, and what the desk needed before the user walked
// away, which is put back on the first key press.

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"
)

type userSample struct {
	at      time.Time
	cores   float64 // processors the user's own programs used
	waiting float64 // their threads that wanted a processor and had none
}

// reserver keeps what the reserve is worked out from.
type reserver struct {
	samples []userSample
	// apps is the most processors each full-screen program has been seen to use.
	apps map[string]float64
	// deskPeak is the window's peak the last time someone was at the PC.
	deskPeak  float64
	lastState string
	dirty     bool
}

func appsFile() string { return filepath.Join(homeDir(), "apps.json") }

func loadApps() map[string]float64 {
	apps := map[string]float64{}
	if b, err := os.ReadFile(appsFile()); err == nil {
		json.Unmarshal(b, &apps)
	}
	return apps
}

func (rv *reserver) save() {
	if !rv.dirty {
		return
	}
	if b, err := json.MarshalIndent(rv.apps, "", "  "); err == nil && os.WriteFile(appsFile()+".tmp", b, 0o644) == nil {
		os.Rename(appsFile()+".tmp", appsFile())
	}
	rv.dirty = false
}

// want is the logical processors to keep for the user now, and why in plain words. perCore is the
// logical processors of one physical core.
func (rv *reserver) want(now time.Time, userCores, userWaiting float64, p Presence, s Settings, logical, perCore int) (int, string) {
	if rv.apps == nil {
		rv.apps = map[string]float64{}
	}
	if perCore < 1 {
		perCore = 1
	}
	here := p.State == "active" || p.State == "full-screen"
	if here && rv.lastState == "away" && rv.deskPeak > userCores {
		// Back at the PC: what the desk needed before, at once, until the window shows otherwise.
		userCores = rv.deskPeak
	}
	rv.lastState = p.State
	rv.samples = append(rv.samples, userSample{now, userCores, userWaiting})
	cut := now.Add(-time.Duration(s.ReserveWindowSeconds * float64(time.Second)))
	i := 0
	for i < len(rv.samples)-1 && rv.samples[i].at.Before(cut) {
		i++
	}
	rv.samples = rv.samples[i:]

	var peak float64
	for _, u := range rv.samples {
		peak = math.Max(peak, u.cores)
	}
	if here {
		rv.deskPeak = peak
	}
	need, why := peak*s.ReserveHeadroom, fmt.Sprintf("your programs peaked at %.1f processors in the last %s", peak, span(s.ReserveWindowSeconds))
	if p.State == "full-screen" && p.App != "" {
		if peak > rv.apps[p.App] {
			rv.apps[p.App], rv.dirty = round2(peak), true
		}
		known := rv.apps[p.App]
		need = math.Max(peak, known) * s.ReserveHeadroomFullScreen
		why = fmt.Sprintf("%s is full screen and has used up to %.1f processors", p.App, math.Max(peak, known))
	}
	// Their programs waiting for a processor is the direct sign the reserve is too small.
	if n := len(rv.samples); n >= 2 && (rv.samples[n-1].waiting+rv.samples[n-2].waiting)/2 >= 1 {
		need += float64(perCore)
		why += ", and they were waiting for a processor"
	}
	if p.State == "away" {
		why = fmt.Sprintf("nobody at the PC for %.0f minutes", p.IdleMinutes)
	}
	n := int(math.Ceil(need/float64(perCore)-1e-9)) * perCore
	least := int(math.Ceil(float64(s.ReserveMinProcessors)/float64(perCore))) * perCore
	most := int(float64(logical)*s.ReserveMaxShare) / perCore * perCore
	n = max(least, min(n, max(most, least)))
	return n, why
}
