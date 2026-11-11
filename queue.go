package main

// The line. A launch that is refused for room is not lost: it waits here with its whole request,
// in the order it came, and when the machine measures room for it the meter holds that room and
// tells the thread that asked. That thread sends the same launch again and it goes straight in.
//
// fairshare does not start the thread itself. A launch passes through hooks that place it, pick
// its model and check its brief, and the command line that starts threads cannot say everything a
// launch can (an existing worktree, a model's effort, who launched it), so a replay from here would
// start a different thread from the one asked for. The thread that asked sends the identical
// request; fairshare owns the order, the wait and the room.
//
// Why: on 2026-11-11 the first refusals were right and the launches were then simply gone, three
// of them kept by hand in a list until someone retried.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// queued is one launch waiting in the line.
type queued struct {
	ID int `json:"id"`
	// Key names the launch: the same launch sent again carries the same key.
	Key      string `json:"key"`
	Launcher string `json:"launcher"`
	Title    string `json:"title"`
	Threads  int    `json:"threads"`
	Count    int    `json:"count"`
	Checkout bool   `json:"checkout"`
	// Need is what it counted as when it joined, and again when it was offered room.
	Need     Need      `json:"need"`
	QueuedAt time.Time `json:"queued_at"`
	// State is "waiting", or "offered" once room is held for it.
	State     string    `json:"state"`
	OfferedAt time.Time `json:"offered_at,omitzero"`
	Offers    int       `json:"offers"`
	// Request is the file holding the launch exactly as it was sent.
	Request string `json:"request,omitempty"`
}

func queueFile() string { return filepath.Join(homeDir(), "queue.json") }

// withLock runs fn as the only fairshare process changing the line or the grants.
func withLock(fn func() error) error {
	if err := os.MkdirAll(homeDir(), 0o755); err != nil {
		return err
	}
	lock := filepath.Join(homeDir(), "queue.lock")
	for try := 0; ; try++ {
		f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			f.Close()
			break
		}
		if st, serr := os.Stat(lock); serr == nil && time.Since(st.ModTime()) > 30*time.Second {
			os.Remove(lock) // a holder that died
			continue
		}
		if try >= 150 {
			return fmt.Errorf("the line's lock %s is held", lock)
		}
		time.Sleep(20 * time.Millisecond)
	}
	defer os.Remove(lock)
	return fn()
}

func loadQueue() []queued {
	var q []queued
	if b, err := os.ReadFile(queueFile()); err == nil {
		json.Unmarshal(b, &q)
	}
	return q
}

func saveQueue(q []queued) error {
	if q == nil {
		q = []queued{}
	}
	b, err := json.MarshalIndent(q, "", "  ")
	if err != nil {
		return err
	}
	tmp := queueFile() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, queueFile())
}

// lineNeed is what the launches in the line count as: all of them toward a new arrival (so it
// passes the line only when there is room for everyone ahead of it too), only the offered ones
// toward the head of the line.
func lineNeed(q []queued, offeredOnly bool) Need {
	var n Need
	for _, e := range q {
		if !offeredOnly || e.State == "offered" {
			n = n.plus(e.Need)
		}
	}
	return n
}

func position(q []queued, key string) (int, *queued) {
	for i := range q {
		if q[i].Key == key {
			return i + 1, &q[i]
		}
	}
	return 0, nil
}

func nextID(q []queued) int {
	id := 0
	for _, e := range q {
		id = max(id, e.ID)
	}
	return id + 1
}

// notice is what the meter hands the notify command.
type notice struct {
	// Event is "offer" (room is held for the launch) or "dropped" (its offers ran out).
	Event       string  `json:"event"`
	Entry       queued  `json:"entry"`
	HoldMinutes float64 `json:"hold_minutes"`
	Waiting     int     `json:"waiting"`
}

// advance moves the line on one look: offers that were not taken in time go to the back (and
// leave after queue_offers of them), and each launch at the front that the machine now has room
// for gets that room held. It returns what to tell whom. Pure but for the clock it is given.
func advance(q []queued, r Reading, s Settings, now time.Time, granted Need) ([]queued, []notice) {
	var notices []notice
	hold := time.Duration(s.QueueHoldMinutes * float64(time.Minute))
	var kept, lapsed []queued
	for _, e := range q {
		switch {
		case now.Sub(e.QueuedAt) > time.Duration(s.QueueMaxHours*float64(time.Hour)):
			notices = append(notices, notice{Event: "dropped", Entry: e})
		case e.State == "offered" && now.Sub(e.OfferedAt) > hold:
			e.State, e.Offers = "waiting", e.Offers+1
			if e.Offers >= s.QueueOffers {
				notices = append(notices, notice{Event: "dropped", Entry: e})
			} else {
				lapsed = append(lapsed, e)
			}
		default:
			kept = append(kept, e)
		}
	}
	q = append(kept, lapsed...)
	// Strictly in order: the first launch that does not fit stops the line, so a small launch
	// behind it cannot take the room it is waiting for.
	for i := range q {
		if q[i].State == "offered" {
			continue
		}
		v := decide(r, s, Ask{Threads: q[i].Threads, Count: q[i].Count, Disk: q[i].Checkout}, granted.plus(lineNeed(q, true)))
		if !v.OK {
			break
		}
		q[i].State, q[i].OfferedAt = "offered", now
		q[i].Need = Need{v.CPU.Need, v.Mem.Need, v.Disk.Need}
		notices = append(notices, notice{Event: "offer", Entry: q[i], HoldMinutes: s.QueueHoldMinutes})
	}
	for i := range notices {
		notices[i].Waiting = len(q)
	}
	return q, notices
}

// tell runs the notify command with one notice on its stdin.
func tell(s Settings, n notice) error {
	if len(s.QueueNotify) == 0 {
		return fmt.Errorf("queue_notify is not set in %s, so nobody is told", settingsFile())
	}
	b, err := json.Marshal(n)
	if err != nil {
		return err
	}
	cmd := exec.Command(s.QueueNotify[0], s.QueueNotify[1:]...)
	cmd.Stdin = strings.NewReader(string(b))
	hideWindow(cmd)
	done := make(chan error, 1)
	var out []byte
	go func() {
		var err error
		out, err = cmd.CombinedOutput()
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("%s: %v: %s", s.QueueNotify[0], err, strings.TrimSpace(string(out[:min(len(out), 300)])))
		}
		return nil
	case <-time.After(60 * time.Second):
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		return fmt.Errorf("%s did not finish in 60 s", s.QueueNotify[0])
	}
}

// moveLine is what the meter does about the line after each look.
func moveLine(r Reading, s Settings, now time.Time) {
	if _, err := os.Stat(queueFile()); err != nil {
		return // no line: the common case costs one stat
	}
	var notices []notice
	err := withLock(func() error {
		q := loadQueue()
		if len(q) == 0 {
			return nil
		}
		settle := time.Duration(s.SettleMinutes * float64(time.Minute))
		var after []queued
		after, notices = advance(q, r, s, now, pendingNeed(now, settle))
		return saveQueue(after)
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "degraded: the line was not moved:", err)
		return
	}
	for _, n := range notices {
		rec := map[string]any{"at": now.UTC(), "event": n.Event, "id": n.Entry.ID, "title": n.Entry.Title, "launcher": n.Entry.Launcher, "offers": n.Entry.Offers}
		if err := tell(s, n); err != nil {
			rec["degraded"] = err.Error()
			fmt.Fprintln(os.Stderr, "degraded: the launcher was not told:", err)
		}
		appendLine("decisions.jsonl", rec, 8<<20)
	}
}

func cmdQueue(args []string) error {
	if len(args) >= 2 && args[0] == "drop" {
		return withLock(func() error {
			q := loadQueue()
			var kept []queued
			for _, e := range q {
				if fmt.Sprint(e.ID) != args[1] {
					kept = append(kept, e)
				}
			}
			if len(kept) == len(q) {
				return fmt.Errorf("no launch %s in the line", args[1])
			}
			fmt.Printf("fairshare: launch %s left the line\n", args[1])
			return saveQueue(kept)
		})
	}
	if len(args) >= 1 && args[0] == "move" {
		// What the meter does after every look, once, by hand or from a timer on a machine with
		// no meter.
		s, err := loadSettings()
		if err != nil {
			return err
		}
		r, err := currentReading(s)
		if err != nil {
			return err
		}
		moveLine(r, s, time.Now())
		return nil
	}
	q := loadQueue()
	if len(args) > 0 && args[0] == "--json" {
		if q == nil {
			q = []queued{}
		}
		return printJSON(q)
	}
	if len(q) == 0 {
		fmt.Println("Nothing is waiting for room.")
		return nil
	}
	fmt.Printf("%d waiting for room, in order:\n", len(q))
	for i, e := range q {
		state := fmt.Sprintf("waiting %.0f min", time.Since(e.QueuedAt).Minutes())
		if e.State == "offered" {
			state = fmt.Sprintf("room held for it since %s, its launcher was told", e.OfferedAt.Local().Format("15:04"))
		}
		fmt.Printf("  %d. #%d %s (%s; from %s)\n", i+1, e.ID, e.Title, state, e.Launcher)
	}
	return nil
}
