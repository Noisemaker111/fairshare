package main

// How many processors to keep for the person at the PC. Jon's rule (2026-11-11): the reserve only
// matters while he plays a game; otherwise two, so the desktop never fails. A game running (a
// full-screen program, or a process named in games or seen full screen before) gets a reserve the
// size of what that game's own processes use, with headroom, in whole physical cores (a thread on a
// core's second half still slows the first). Windows' own background work (dwm, Defender, the search
// indexer, svchost) is not Jon and sizes nothing.
//
// Why: on 2026-11-11 fairshare kept 6 processors with nobody at the PC, because it counted dwm 1.4,
// MsMpEng 1.4 and SearchIndexer 0.9 as his programs and added headroom.
//
// While a game runs the reserve grows the moment the game uses more or its threads wait for a
// processor, and shrinks only after the whole window has been quieter. What each game needed the
// last time it ran is remembered, so it gets its processors on its first look.

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type userSample struct {
	at      time.Time
	cores   float64 // processors the game's own processes used
	waiting float64 // the user's threads that wanted a processor and had none
}

// reserver keeps what the reserve is worked out from.
type reserver struct {
	samples []userSample
	// apps is the most processors each game has been seen to use; a program seen full screen is
	// learned here, unless settings say it is not a game.
	apps  map[string]float64
	dirty bool
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

// isGame says whether a program (its image name) counts as a game: named in games, or seen full
// screen before and not named in not_games.
func (rv *reserver) isGame(name string, s Settings) bool {
	name = strings.ToLower(name)
	for _, g := range s.Games {
		if strings.EqualFold(g, name) {
			return true
		}
	}
	if _, seen := rv.apps[name]; seen {
		return !notGame(name, s)
	}
	return false
}

func notGame(name string, s Settings) bool {
	for _, g := range s.NotGames {
		if strings.EqualFold(g, name) {
			return true
		}
	}
	return false
}

// want is the logical processors to keep for the user now, and why in plain words. game is the
// game running ("" for none), gameCores what its own processes used in this look, userWaiting the
// user's threads waiting for a processor; perCore is the logical processors of one physical core.
func (rv *reserver) want(now time.Time, game string, gameCores, userWaiting float64, p Presence, s Settings, logical, perCore int) (int, string) {
	if rv.apps == nil {
		rv.apps = map[string]float64{}
	}
	if perCore < 1 {
		perCore = 1
	}
	least := int(math.Ceil(float64(s.ReserveMinProcessors)/float64(perCore))) * perCore
	if game == "" && p.State == "full-screen" && p.App != "" && !notGame(p.App, s) {
		game = strings.ToLower(p.App)
	}
	if game == "" {
		rv.samples = nil
		if p.State == "full-screen" {
			return least, fmt.Sprintf("%s is full screen and is not a game: %d kept so the desktop answers", p.App, least)
		}
		return least, fmt.Sprintf("no game running: %d kept so the desktop answers", least)
	}
	rv.samples = append(rv.samples, userSample{now, gameCores, userWaiting})
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
	if peak > rv.apps[game] {
		rv.apps[game], rv.dirty = round2(peak), true
	}
	known := math.Max(peak, rv.apps[game])
	need := known * s.ReserveHeadroomFullScreen
	why := fmt.Sprintf("%s is running and has used up to %.1f processors", game, known)
	// The user's threads waiting for a processor is the direct sign the reserve is too small.
	if n := len(rv.samples); n >= 2 && (rv.samples[n-1].waiting+rv.samples[n-2].waiting)/2 >= 1 {
		need += float64(perCore)
		why += ", and its threads were waiting for a processor"
	}
	n := int(math.Ceil(need/float64(perCore)-1e-9)) * perCore
	most := int(float64(logical)*s.ReserveMaxShare) / perCore * perCore
	n = max(least, min(n, max(most, least)))
	return n, why
}
