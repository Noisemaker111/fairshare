package main

// Urgent work goes first. A launch marked urgent (a production fix, say) is let in when the
// processors work that would not step aside leaves room for it, and once it is let in Owl holds
// every agent session that started before it at idle priority, so Windows runs the new session's
// work first and the older work only on what it leaves. Nothing is ended or paused, and the
// processors kept for the person at the PC stay theirs: Owl keeps all agent work off them, urgent
// work too.
//
// Why: on 2026-11-11 a production fix for AppForGutters (uploads failing, a false "offline") was
// refused room on the PC because two Owl Rust ports' gates used 17 of its 18 agent processors. It
// went to a boat host, which cannot deploy, and the deploy was run by hand.
//
// fairshare decides and tells; Owl's keeper does the holding (its yield_before setting, told for
// three minutes at a time and again every minute, so a meter that stops lets the hold run out).
// A hold exists only once Owl has taken it: an Owl that refuses the setting holds nothing, so
// nothing is recorded as held and the answer says degraded.
//
// Why: on 2026-11-11 the hold for a production deploy stood for 45 minutes and freed nothing. The
// Owl on the PC did not have yield_before yet and answered `{"error": ...}` with exit 0, which
// fairshare read as told; and any agent session started since the hold began kept it standing,
// whoever's it was.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"
)

// urgentHold is the hold in force: Since is when the first urgent launch of it was let in (every
// agent session older than that yields), Last the latest one.
type urgentHold struct {
	Since  time.Time `json:"since"`
	Last   time.Time `json:"last"`
	Labels []string  `json:"labels,omitempty"`
	// Repos are the repos its urgent launches were for: the hold stands while their work runs.
	Repos []string `json:"repos,omitempty"`
}

// runs says whether work an urgent launch of this hold started is still open: a thread or a run
// in one of its repos that began since the hold did. A launch that named no repo has no work
// that can be told from anyone else's, so its hold stands for its least minutes only.
func (h urgentHold) runs(work []LiveWork) bool {
	for _, w := range work {
		if w.Repo == "" || w.Since.Before(h.Since) {
			continue
		}
		for _, r := range h.Repos {
			if strings.EqualFold(strings.TrimSpace(r), w.Repo) {
				return true
			}
		}
	}
	return false
}

func urgentFile() string { return filepath.Join(homeDir(), "urgent.json") }

func loadUrgent() (urgentHold, bool) {
	var h urgentHold
	b, err := os.ReadFile(urgentFile())
	if err != nil || json.Unmarshal(b, &h) != nil || h.Since.IsZero() {
		return urgentHold{}, false
	}
	return h, true
}

func saveUrgent(h urgentHold) error {
	if err := os.MkdirAll(homeDir(), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	tmp := urgentFile() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, urgentFile())
}

// isUrgent says whether a launch is urgent: asked for with --urgent, or a title or repo that
// holds one of the settings' urgent words.
func isUrgent(flag bool, label, repo string, s Settings) (bool, string) {
	if flag {
		return true, "--urgent"
	}
	text := words(label + " " + repo)
	for _, w := range s.UrgentWords {
		if w := words(w); w != "  " && strings.Contains(text, w) {
			return true, "its title says" + strings.TrimSuffix(w, " ")
		}
	}
	return false, ""
}

// words is text in lower case with every run of other characters one space, and a space at each
// end, so a word is found only whole: "production" is not in "productionize".
func words(text string) string {
	return " " + strings.Join(strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}), " ") + " "
}

// live says whether a hold still stands: for urgent_least_minutes after its latest urgent launch
// whatever happens, then while the work its urgent launches started is still running (runs), and
// never past urgent_most_minutes after its latest urgent launch.
func (h urgentHold) live(now time.Time, sessionSince bool, s Settings) bool {
	if h.Since.IsZero() || !now.Before(h.Last.Add(minutes(s.UrgentMostMinutes))) {
		return false
	}
	return now.Before(h.Last.Add(minutes(s.UrgentLeastMinutes))) || sessionSince
}

// join adds an urgent launch let in at now: a hold that stands keeps its start, so the urgent
// sessions already let in under it never yield to the new one.
func (h urgentHold) join(now time.Time, label, repo string, standing bool) urgentHold {
	if !standing {
		h = urgentHold{Since: now}
	}
	h.Last = now
	if repo = strings.ToLower(strings.TrimSpace(repo)); repo != "" && !slices.Contains(h.Repos, repo) {
		h.Repos = append(h.Repos, repo)
	}
	if label != "" {
		h.Labels = append(h.Labels, label)
		if len(h.Labels) > 5 {
			h.Labels = h.Labels[len(h.Labels)-5:]
		}
	}
	return h
}

func minutes(m float64) time.Duration { return time.Duration(m * float64(time.Minute)) }

// firmCores is the processors in use one look counted toward an urgent launch: everything on the
// agent processors except agent work that steps aside for it. With a hold in force that is the
// work Owl already holds at idle; with none, a new hold would put every agent session there.
func firmCores(busy, workCores, idleCores float64, holding bool) float64 {
	yields := workCores
	if holding {
		yields = idleCores
	}
	return max(0, busy-yields)
}
