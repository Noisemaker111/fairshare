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

import (
	"encoding/json"
	"os"
	"path/filepath"
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
// whatever happens, then while an agent session started since it began is still running, and
// never past urgent_most_minutes after its latest urgent launch.
func (h urgentHold) live(now time.Time, sessionSince bool, s Settings) bool {
	if h.Since.IsZero() || !now.Before(h.Last.Add(minutes(s.UrgentMostMinutes))) {
		return false
	}
	return now.Before(h.Last.Add(minutes(s.UrgentLeastMinutes))) || sessionSince
}

// join adds an urgent launch let in at now: a hold that stands keeps its start, so the urgent
// sessions already let in under it never yield to the new one.
func (h urgentHold) join(now time.Time, label string, standing bool) urgentHold {
	if !standing {
		h = urgentHold{Since: now}
	}
	h.Last = now
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
