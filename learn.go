package main

// What work uses, learned from what it was measured using. Nothing here is a number somebody
// chose for a repo or a command: a piece of work is sized by the minutes the meter recorded for
// work like it, the nearest kind first.
//
//   - a thread: this repo's threads on this machine, then every thread on this machine;
//   - a run (a gate, a build): this command in this repo, then this repo's runs, then every run;
//   - either: what another machine of the pool measured for it, while this one has measured little.
//
// Each level pulls the estimate toward itself by how many minutes it holds against
// learn_trust_minutes, so ten minutes of a new command already count for half and an hour for
// nearly all. A run that finished is a whole sample of its command however short it was, so
// learn_trust_runs finished runs (one) weigh as much as those minutes: a 30-second lint counts
// for half after its first run and three quarters after its third, not after twenty. Minutes fade with a half-life of learn_half_life_days, so a repo that changed is
// re-learned and nothing has to be reset by hand. Besides the average, the spread of the minutes is
// kept: a thread that waits on its model and then builds for five minutes is not a thread that
// uses the same average steadily, and the room rule keeps room for the difference (room.go).
//
// Why: Jon, 2026-11-11: a thread counted as one number per repo (appforgutters 1, owl-go 4.5) and
// a gate as `--cores 4` typed by its caller; "every thread and every process is different".

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// profile is what one kind of work was measured using. Every sum fades at the same rate, so the
// averages are of recent minutes.
type profile struct {
	At      time.Time `json:"at"`
	Minutes float64   `json:"minutes"`
	// Cores is the sum of minutes times processors, Sq of minutes times processors squared, and
	// MemGB of minutes times memory.
	Cores float64 `json:"cores"`
	Sq    float64 `json:"sq"`
	MemGB float64 `json:"mem_gb"`
	// MemPeak is the most memory one stretch of it held, fading like the rest.
	MemPeak float64 `json:"mem_peak"`
	// Works is how many pieces of it ended and Seconds how long they ran in all.
	Works   float64 `json:"works"`
	Seconds float64 `json:"seconds"`
}

// fade ages the profile to now: every sum halves each halfLife.
func (p *profile) fade(now time.Time, halfLife time.Duration) {
	if age := now.Sub(p.At); age > 0 && halfLife > 0 && !p.At.IsZero() {
		f := math.Pow(0.5, float64(age)/float64(halfLife))
		p.Minutes, p.Cores, p.Sq, p.MemGB, p.MemPeak, p.Works, p.Seconds = p.Minutes*f, p.Cores*f, p.Sq*f, p.MemGB*f, p.MemPeak*f, p.Works*f, p.Seconds*f
	}
	if now.After(p.At) {
		p.At = now
	}
}

func (p profile) mean() float64 {
	if p.Minutes <= 0 {
		return 0
	}
	return p.Cores / p.Minutes
}

// meanSq is the average of the squares, from which the spread comes.
func (p profile) meanSq() float64 {
	if p.Minutes <= 0 {
		return 0
	}
	return p.Sq / p.Minutes
}

// mem is the memory to keep for it: the most a recent stretch held, never under its average.
func (p profile) mem() float64 {
	if p.Minutes <= 0 {
		return 0
	}
	return math.Max(p.MemGB/p.Minutes, p.MemPeak)
}

func spread(mean, meanSq float64) float64 { return math.Sqrt(math.Max(0, meanSq-mean*mean)) }

// learned is every profile, by learnKey.
type learned map[string]*profile

// learnKey names a level: a kind of work ("thread", "run", "job"), in a repo, and for a run its
// command. An empty repo is every repo and an empty what every command.
func learnKey(kind, repo, what string) string {
	return kind + "|" + strings.ToLower(strings.TrimSpace(repo)) + "|" + what
}

var shaLike = regexp.MustCompile(`\b([0-9a-f]{7,40}|[0-9]{5,})\b`)

// runName is what a command is called for learning: its words as typed, without the shell in
// front of them and without the numbers and commit ids that change from run to run.
func runName(argv []string) string {
	if len(argv) >= 3 {
		switch strings.TrimSuffix(strings.ToLower(filepath.Base(strings.ReplaceAll(argv[0], `\`, "/"))), ".exe") {
		case "bash", "sh", "zsh", "dash":
			if strings.HasPrefix(argv[1], "-") && strings.HasSuffix(argv[1], "c") {
				argv = argv[2:]
			}
		}
	}
	text := strings.Join(strings.Fields(strings.ToLower(strings.Join(argv, " "))), " ")
	text = shaLike.ReplaceAllString(text, "#")
	if len(text) > 100 {
		text = text[:100]
	}
	return text
}

// stretch is what one piece of work used over one stretch of time.
type stretch struct {
	Kind, Repo, What string
	Minutes          float64
	Cores            float64 // processors, averaged over it
	MemGB            float64 // the most memory it held during it
	// Ended says the work ended with this stretch, Seconds how long it ran in all, and Began
	// when it began.
	Ended   bool
	Seconds float64
	Began   time.Time
}

// keys are the levels a piece of work is learned at, the widest first.
func keys(kind, repo, what string) []string {
	out := []string{learnKey(kind, "", "")}
	if strings.TrimSpace(repo) != "" {
		out = append(out, learnKey(kind, repo, ""))
		if kind == "run" && what != "" {
			out = append(out, learnKey(kind, repo, what))
		}
	}
	return out
}

// add records one stretch at every level it belongs to.
func (l learned) add(now time.Time, sp stretch, s Settings) {
	if sp.Kind == "" || (sp.Minutes <= 0 && !sp.Ended) {
		return
	}
	for _, k := range keys(sp.Kind, sp.Repo, sp.What) {
		p := l[k]
		if p == nil {
			p = &profile{At: now}
			l[k] = p
		}
		p.fade(now, s.halfLife())
		if sp.Minutes > 0 {
			p.Minutes += sp.Minutes
			p.Cores += sp.Minutes * sp.Cores
			p.Sq += sp.Minutes * sp.Cores * sp.Cores
			p.MemGB += sp.Minutes * sp.MemGB
			p.MemPeak = math.Max(p.MemPeak, sp.MemGB)
		}
		if sp.Ended {
			p.Works++
			p.Seconds += sp.Seconds
		}
	}
}

// forget ages every profile to now and drops what has faded to nothing, so the file does not
// grow with every command ever run.
func (l learned) forget(now time.Time, s Settings) {
	for k, p := range l {
		p.fade(now, s.halfLife())
		if p.Minutes < 0.01 && p.Works < 0.01 {
			delete(l, k)
		}
	}
}

// level is one source of evidence about a piece of work, as the estimate reads it.
type level struct {
	// Name says whose minutes these are: "owl-go threads here".
	Name    string  `json:"name"`
	Minutes float64 `json:"minutes"`
	Cores   float64 `json:"cores"`
	// MeanSq is the average of the squared processors, which holds the spread.
	MeanSq  float64 `json:"mean_sq"`
	MemGB   float64 `json:"mem_gb"`
	Seconds float64 `json:"seconds,omitempty"`
	// Weigh is how much the level counts for, in minutes: its minutes, and for a run its finished
	// runs too. Unset, its minutes.
	Weigh float64 `json:"weigh,omitempty"`
	// Own says the minutes were measured on this machine for this very repo or command.
	Own bool `json:"own,omitempty"`
}

func (lv level) weigh() float64 {
	if lv.Weigh > 0 {
		return lv.Weigh
	}
	return lv.Minutes
}

func kindWords(kind string) string {
	switch kind {
	case "run":
		return "runs"
	case "job":
		return "jobs outside a thread"
	}
	return "threads"
}

// levels is the evidence about a piece of work, the widest first: every such kind here, what
// another machine of the pool measured for it, this repo's here, and this command's here.
func (l learned) levels(kind, repo, what string, seen *Sizing, now time.Time, s Settings) []level {
	repo = strings.ToLower(strings.TrimSpace(repo))
	var out []level
	at := func(key, name string, own bool) {
		p := l[key]
		if p == nil {
			return
		}
		q := *p
		q.fade(now, s.halfLife())
		if q.Minutes <= 0 {
			return
		}
		lv := level{Name: name, Minutes: round2(q.Minutes), Cores: q.mean(), MeanSq: q.meanSq(), MemGB: q.mem(), Own: own}
		if q.Works > 0 && kind == "run" {
			lv.Seconds = q.Seconds / q.Works
			lv.Weigh = q.Minutes + q.Works*s.LearnTrustMinutes/s.LearnTrustRuns
		}
		out = append(out, lv)
	}
	at(learnKey(kind, "", ""), kindWords(kind)+" here", false)
	if seen != nil && seen.Minutes > 0 {
		out = append(out, level{Name: seen.From, Minutes: seen.Minutes, Cores: seen.Cores, MeanSq: seen.Swing*seen.Swing + seen.Cores*seen.Cores,
			MemGB: seen.MemGB, Seconds: seen.Seconds})
	}
	if repo != "" {
		at(learnKey(kind, repo, ""), repo+" "+kindWords(kind)+" here", true)
		if kind == "run" && what != "" {
			at(learnKey(kind, repo, what), "`"+what+"` in "+repo+" here", true)
		}
	}
	return out
}

// Sizing is what a piece of work is expected to use, and what that was learned from.
type Sizing struct {
	// Cores is the processors it is expected to use and Swing how far its minutes stray from that.
	Cores float64 `json:"cores"`
	Swing float64 `json:"swing"`
	MemGB float64 `json:"mem_gb"`
	// Minutes is how much was measured at the nearest level that had any, and From names it; an
	// empty From means nothing like it has been measured. FromCores, FromSwing and FromMemGB are
	// what that level itself measured, which the estimate is pulled toward and not always at yet.
	Minutes   float64 `json:"minutes"`
	From      string  `json:"from,omitempty"`
	FromCores float64 `json:"from_cores,omitempty"`
	FromSwing float64 `json:"from_swing,omitempty"`
	FromMemGB float64 `json:"from_mem_gb,omitempty"`
	// Seconds is how long such a run lasts, when runs of it have ended.
	Seconds float64 `json:"seconds,omitempty"`
}

// size is the estimate: it starts from base (the settings' floor, or a caller's own guess) and
// each level, the widest first, pulls it toward what that level measured by minutes / (minutes +
// trust). The nearest level is the last to pull, so with enough of its own minutes it decides.
// While the levels disagree the estimate's swing holds their disagreement too: work that is not
// known yet is given room for being either.
func size(base Need, levels []level, trust float64) Sizing {
	mean, meanSq, mem := base.Cores, base.Cores*base.Cores, base.MemGB
	sz := Sizing{}
	for _, lv := range levels {
		if lv.Minutes <= 0 {
			continue
		}
		w := lv.weigh() / (lv.weigh() + trust)
		mean = w*lv.Cores + (1-w)*mean
		meanSq = w*lv.MeanSq + (1-w)*meanSq
		mem = w*lv.MemGB + (1-w)*mem
		sz.Minutes, sz.From = lv.Minutes, lv.Name
		sz.FromCores, sz.FromSwing, sz.FromMemGB = round2(lv.Cores), round2(spread(lv.Cores, lv.MeanSq)), round2(lv.MemGB)
		if lv.Seconds > 0 {
			sz.Seconds = lv.Seconds
		}
	}
	sz.Cores, sz.Swing, sz.MemGB = mean, spread(mean, meanSq), mem
	return sz
}

// measured is what this machine itself measured for exactly this work, for the pool to pass to
// the machines that have not: the nearest level that is this machine's own, unblended.
func measured(levels []level, machine string) *Sizing {
	for i := len(levels) - 1; i >= 0; i-- {
		if lv := levels[i]; lv.Own && lv.Minutes > 0 {
			return &Sizing{Cores: round2(lv.Cores), Swing: round2(spread(lv.Cores, lv.MeanSq)), MemGB: round2(lv.MemGB), Minutes: lv.Minutes,
				Seconds: round2(lv.Seconds), From: strings.TrimSuffix(lv.Name, " here") + " on " + machine}
		}
	}
	return nil
}

func sizedWords(sz Sizing) string {
	if sz.From == "" {
		return "nothing like it has been measured here yet, so it starts at the settings' floor"
	}
	text := fmt.Sprintf("%s were measured at %.1f processors", sz.From, sz.FromCores)
	if strings.HasPrefix(sz.From, "`") {
		text = fmt.Sprintf("%s was measured at %.1f processors", sz.From, sz.FromCores)
	}
	if sz.FromSwing >= 0.1 {
		text += fmt.Sprintf(", give or take %.1f,", sz.FromSwing)
	}
	text += fmt.Sprintf(" and %.1f GB over %.0f minutes", sz.FromMemGB, sz.Minutes)
	if sz.Seconds >= 1 {
		text += fmt.Sprintf(" (a run lasts about %s)", lasts(sz.Seconds))
	}
	return text
}

func lasts(seconds float64) string {
	if seconds >= 90 {
		return fmt.Sprintf("%.0f minutes", seconds/60)
	}
	return fmt.Sprintf("%.0f seconds", seconds)
}

func learnedFile() string { return filepath.Join(homeDir(), "learned.json") }

// loadLearned reads what was learned. The first time, it starts from the per-repo averages the
// meter kept before (repos.json), so a week of measurements is not thrown away.
func loadLearned() learned {
	l := learned{}
	b, err := os.ReadFile(learnedFile())
	if err == nil {
		json.Unmarshal(b, &l)
		for k, p := range l {
			if p == nil {
				delete(l, k)
			}
		}
		return l
	}
	var old map[string]map[string]struct {
		Minutes float64 `json:"minutes"`
		Cores   float64 `json:"cores"`
		MemGB   float64 `json:"mem_gb"`
	}
	if b, err := os.ReadFile(filepath.Join(homeDir(), "repos.json")); err == nil && json.Unmarshal(b, &old) == nil {
		for repo, days := range old {
			for day, d := range days {
				at, err := time.ParseInLocation("2006-01-02", day, time.Local)
				if err != nil || d.Minutes <= 0 {
					continue
				}
				for _, k := range keys("thread", repo, "") {
					p := l[k]
					if p == nil {
						p = &profile{}
						l[k] = p
					}
					// The old file kept sums only, so the spread within a day is not known.
					mean := d.Cores / d.Minutes
					p.Minutes, p.Cores, p.Sq, p.MemGB = p.Minutes+d.Minutes, p.Cores+d.Cores, p.Sq+d.Minutes*mean*mean, p.MemGB+d.MemGB
					if end := at.Add(24 * time.Hour); end.After(p.At) {
						p.At = end
					}
				}
			}
		}
		now := time.Now()
		for _, p := range l {
			if p.At.After(now) {
				p.At = now
			}
		}
	}
	return l
}

// saveLearned writes what was learned, without what has faded to nothing.
func saveLearned(l learned) error {
	if err := os.MkdirAll(homeDir(), 0o755); err != nil {
		return err
	}
	for k, p := range l {
		if p.Minutes < 0.01 && p.Works < 0.01 {
			delete(l, k)
		}
	}
	b, err := json.Marshal(l)
	if err != nil {
		return err
	}
	tmp := learnedFile() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, learnedFile())
}

// learnedRow is one line of `fairshare learned`.
type learnedRow struct {
	Kind    string  `json:"kind"`
	Repo    string  `json:"repo,omitempty"`
	What    string  `json:"what,omitempty"`
	Cores   float64 `json:"cores"`
	Swing   float64 `json:"swing"`
	MemGB   float64 `json:"mem_gb"`
	Minutes float64 `json:"minutes"`
	Works   float64 `json:"works"`
	Seconds float64 `json:"seconds,omitempty"`
}

// table lists what was learned: threads, then runs, then loose jobs; within each the widest
// level first, then the heaviest.
func (l learned) table(now time.Time, s Settings) []learnedRow {
	out := []learnedRow{}
	for k, p := range l {
		parts := strings.SplitN(k, "|", 3)
		if len(parts) != 3 {
			continue
		}
		q := *p
		q.fade(now, s.halfLife())
		if q.Minutes < 0.05 {
			continue
		}
		row := learnedRow{Kind: parts[0], Repo: parts[1], What: parts[2], Cores: round2(q.mean()), Swing: round2(spread(q.mean(), q.meanSq())),
			MemGB: round2(q.mem()), Minutes: round2(q.Minutes), Works: round2(q.Works)}
		if q.Works > 0 {
			row.Seconds = round2(q.Seconds / q.Works)
		}
		out = append(out, row)
	}
	order := map[string]int{"thread": 0, "run": 1, "job": 2}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Kind != b.Kind {
			return order[a.Kind] < order[b.Kind]
		}
		if (a.Repo == "") != (b.Repo == "") {
			return a.Repo == ""
		}
		if (a.What == "") != (b.What == "") {
			return a.What == ""
		}
		if a.Cores != b.Cores {
			return a.Cores > b.Cores
		}
		return a.Repo+a.What < b.Repo+b.What
	})
	return out
}
