package main

// The line. A launch that is refused for room is not lost: it waits here with its whole request,
// in the order it came, and when a machine it can run on measures room for it the meter holds that
// room and tells the thread that asked. That thread sends the same launch again and it goes
// straight in, on the machine that was held.
//
// One line serves every machine. A launch names the machines it can run on ("pc", or the PC and
// the boat hosts that have its repo); a launch that names none can run only here. The line is in
// order per machine: a launch that cannot be placed keeps every machine it could use from being
// taken by the launches behind it, and a launch behind it that needs a different machine goes
// ahead. Why: on 2026-11-11 a thread that needed no Windows sat at place 5 behind four launches
// waiting for the PC while two boat hosts were nearly idle.
//
// fairshare does not start the thread itself. A launch passes through hooks that place it, pick
// its model and check its brief, and the command line that starts threads cannot say everything a
// launch can (an existing worktree, a model's effort, who launched it), so a replay from here would
// start a different thread from the one asked for. The thread that asked sends the identical
// request; fairshare owns the order, the wait and the room.
//
// Why at all: on 2026-11-11 the first refusals were right and the launches were then simply gone,
// three of them kept by hand in a list until someone retried.

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// here is this machine's name in a launch's list of machines.
const here = "pc"

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
	// Repo is the repo it works in: it counts as what that repo's threads are measured using, as
	// of the look that judges it (learned is filled in then).
	Repo    string `json:"repo,omitempty"`
	learned []level
	// Machines are the machines it can run on, by name; none means only this one.
	Machines []string `json:"machines,omitempty"`
	// Need is what it counted as on this machine when it joined, and again when it was offered room.
	Need     Need      `json:"need"`
	QueuedAt time.Time `json:"queued_at"`
	// State is "waiting", or "offered" once room is held for it on Machine.
	State     string    `json:"state"`
	Machine   string    `json:"machine,omitempty"`
	OfferedAt time.Time `json:"offered_at,omitzero"`
	Offers    int       `json:"offers"`
	// Request is the file holding the launch exactly as it was sent.
	Request string `json:"request,omitempty"`
}

// machines are the machines the launch can run on.
func (e queued) machines() []string {
	if len(e.Machines) == 0 {
		return []string{here}
	}
	return e.Machines
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

// lineNeed is what the launches in the line count as on this machine: toward a new arrival, every
// launch that could run here and every one with room held here (so it passes the line only when
// there is room for everyone ahead of it too); toward the head of the line, only room held here.
func lineNeed(q []queued, heldOnly bool) Need {
	var n Need
	for _, e := range q {
		held := e.State == "offered" && (e.Machine == "" || e.Machine == here)
		waiting := e.State != "offered" && slices.Contains(e.machines(), here)
		if held || (!heldOnly && waiting) {
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
	// Event is "offer" (room is held for the launch on Entry.Machine) or "dropped" (its offers
	// ran out).
	Event       string  `json:"event"`
	Entry       queued  `json:"entry"`
	HoldMinutes float64 `json:"hold_minutes"`
	Waiting     int     `json:"waiting"`
}

// placer picks a machine for a launch that can run on more than this one: local is this machine's
// answer for it (nil when this machine is not open to it), usable the machines no launch ahead of
// it is waiting for. It returns a machine's name, or "" for none.
type placer func(e queued, local *Verdict, usable []string) string

// advance moves the line on one look: offers that were not taken in time go to the back (and
// leave after queue_offers of them), and each launch that a machine now has room for gets that
// room held, in order per machine. It returns what to tell whom. place is nil on a look that
// does not ask the other machines.
func advance(q []queued, r Reading, s Settings, now time.Time, granted Need, place placer) ([]queued, []notice) {
	var notices []notice
	hold := time.Duration(s.QueueHoldMinutes * float64(time.Minute))
	var kept, lapsed []queued
	for _, e := range q {
		switch {
		case now.Sub(e.QueuedAt) > time.Duration(s.QueueMaxHours*float64(time.Hour)):
			notices = append(notices, notice{Event: "dropped", Entry: e})
		case e.State == "offered" && now.Sub(e.OfferedAt) > hold:
			e.State, e.Machine, e.Offers = "waiting", "", e.Offers+1
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
	// A machine a launch is still waiting for is closed to the launches behind it.
	closed := map[string]bool{}
	for i := range q {
		if q[i].State == "offered" {
			continue
		}
		var usable []string
		for _, m := range q[i].machines() {
			if !closed[m] {
				usable = append(usable, m)
			}
		}
		var local *Verdict
		if slices.Contains(usable, here) {
			v := decide(r, s, Ask{Threads: q[i].Threads, Count: q[i].Count, Disk: q[i].Checkout, Learned: q[i].learned}, granted.plus(lineNeed(q, true)))
			local = &v
		}
		if local != nil {
			// What it counts as now, not when it joined: the launches behind it are judged
			// with this.
			q[i].Need = Need{local.CPU.Need, local.Mem.Need, local.Disk.Need}
		}
		on := ""
		switch {
		case len(q[i].Machines) == 0:
			if local != nil && local.OK {
				on = here
			}
		case place != nil && len(usable) > 0:
			on = place(q[i], local, usable)
			if on == here && (local == nil || !local.OK) {
				on = "" // picked from an older look; this one has no room
			}
		}
		if on == "" {
			for _, m := range q[i].machines() {
				closed[m] = true
			}
			continue
		}
		q[i].State, q[i].Machine, q[i].OfferedAt = "offered", on, now
		if on == here && local != nil {
			q[i].Need = Need{local.CPU.Need, local.Mem.Need, local.Disk.Need}
		}
		notices = append(notices, notice{Event: "offer", Entry: q[i], HoldMinutes: s.QueueHoldMinutes})
	}
	for i := range notices {
		notices[i].Waiting = len(q)
	}
	return q, notices
}

// command runs one of the settings' commands with a JSON value on its stdin and returns what it
// printed.
func command(argv []string, in any, limit time.Duration) ([]byte, error) {
	b, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = strings.NewReader(string(b))
	hideWindow(cmd)
	done := make(chan error, 1)
	var out []byte
	go func() {
		var err error
		out, err = cmd.Output()
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			return out, fmt.Errorf("%s: %v: %s", argv[0], err, strings.TrimSpace(string(out[:min(len(out), 300)])))
		}
		return out, nil
	case <-time.After(limit):
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		return nil, fmt.Errorf("%s did not finish in %s", argv[0], limit)
	}
}

// tell runs the notify command with one notice on its stdin.
func tell(s Settings, n notice) error {
	if len(s.QueueNotify) == 0 {
		return fmt.Errorf("queue_notify is not set in %s, so nobody is told", settingsFile())
	}
	_, err := command(s.QueueNotify, n, 60*time.Second)
	return err
}

// askPlace is the placer that runs the settings' queue_place command.
func askPlace(s Settings) placer {
	if len(s.QueuePlace) == 0 {
		return nil
	}
	return func(e queued, local *Verdict, usable []string) string {
		out, err := command(s.QueuePlace, map[string]any{"entry": e, "local": local, "usable": usable}, 25*time.Second)
		var got struct {
			Machine string `json:"machine"`
		}
		if err != nil || json.Unmarshal(out, &got) != nil {
			fmt.Fprintln(os.Stderr, "degraded: no machine picked for", e.Title, ":", err)
			return ""
		}
		if !slices.Contains(usable, got.Machine) {
			return ""
		}
		return got.Machine
	}
}

var (
	moving    sync.Mutex
	lastPlace time.Time
	lastOrder time.Time
)

// reorder puts the launches named in front, in the order named, and leaves the rest in their
// order behind them. A launch whose room is already held is not moved.
func reorder(q []queued, front []int) []queued {
	rank := map[int]int{}
	for i, id := range front {
		if _, seen := rank[id]; !seen {
			rank[id] = i
		}
	}
	out := append([]queued(nil), q...)
	sort.SliceStable(out, func(a, b int) bool {
		ra, oka := rank[out[a].ID]
		rb, okb := rank[out[b].ID]
		if out[a].State == "offered" || out[b].State == "offered" {
			return false
		}
		if oka != okb {
			return oka
		}
		return oka && ra < rb
	})
	return out
}

// orderLine asks the queue_order command which launches go first and moves them there.
func orderLine(s Settings) {
	q := loadQueue()
	if len(q) < 2 {
		return
	}
	out, err := command(s.QueueOrder, q, 10*time.Second)
	var front []int
	if err != nil || json.Unmarshal(out, &front) != nil {
		fmt.Fprintln(os.Stderr, "degraded: the line was not put in order:", err)
		return
	}
	if len(front) == 0 {
		return
	}
	withLock(func() error {
		before := loadQueue()
		after := reorder(before, front)
		for i := range after {
			if after[i].ID != before[i].ID {
				return saveQueue(after)
			}
		}
		return nil
	})
}

// moveLine is what the meter does about the line after each look. Asking the other machines takes
// seconds, so it runs beside the meter's looks, one at a time, and asks them at most every
// queue_place_seconds.
func moveLine(r Reading, s Settings, now time.Time) {
	if _, err := os.Stat(queueFile()); err != nil {
		return // no line: the common case costs one stat
	}
	if !moving.TryLock() {
		return
	}
	defer moving.Unlock()
	if len(s.QueueOrder) > 0 && now.Sub(lastOrder) >= time.Duration(s.QueuePlaceSeconds*float64(time.Second)) {
		lastOrder = now
		orderLine(s)
	}
	var ask placer
	if now.Sub(lastPlace) >= time.Duration(s.QueuePlaceSeconds*float64(time.Second)) {
		ask = askPlace(s)
	}
	// The other machines are asked with the line unlocked, since a launch hook waits on that
	// lock: each pass under the lock uses the answers so far and names the next launch to ask
	// about, which is asked outside it.
	type question struct {
		e      queued
		local  *Verdict
		usable []string
	}
	answers := map[int]string{}
	var notices []notice
	for round := 0; round < 4; round++ {
		var next *question
		var place placer
		if ask != nil {
			place = func(e queued, local *Verdict, usable []string) string {
				if m, ok := answers[e.ID]; ok {
					if slices.Contains(usable, m) {
						return m
					}
					return ""
				}
				if next == nil {
					next = &question{e, local, usable}
				}
				return ""
			}
		}
		err := withLock(func() error {
			q := loadQueue()
			if len(q) == 0 {
				return nil
			}
			// Each waiting launch is sized by what its repo\'s threads are measured using now.
			known := loadLearned()
			for i := range q {
				q[i].learned = known.levels("thread", q[i].Repo, "", nil, now, s)
			}
			after, told := advance(q, r, s, now, pendingNeed(now, settle(s), r.Work), place)
			notices = append(notices, told...)
			return saveQueue(after)
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "degraded: the line was not moved:", err)
			break
		}
		if next == nil {
			break
		}
		lastPlace = now
		answers[next.e.ID] = ask(next.e, next.local, next.usable)
	}
	for _, n := range notices {
		rec := map[string]any{"at": now.UTC(), "event": n.Event, "id": n.Entry.ID, "title": n.Entry.Title, "launcher": n.Entry.Launcher,
			"machine": n.Entry.Machine, "offers": n.Entry.Offers}
		if err := tell(s, n); err != nil {
			rec["degraded"] = err.Error()
			fmt.Fprintln(os.Stderr, "degraded: the launcher was not told:", err)
		}
		appendLine("decisions.jsonl", rec, 8<<20)
	}
}

// join puts a launch at the back of the line, or finds it there, and says where it stands.
func join(q []queued, e queued, body []byte) ([]queued, int, bool) {
	if at, held := position(q, e.Key); held != nil {
		// Sent again while it waits: it keeps its place, and may now run on the machines named
		// (a launch queued for the PC alone before the line covered the hosts gains them).
		if held.State == "waiting" && len(e.Machines) > 0 && !slices.Equal(held.Machines, e.Machines) {
			held.Machines = e.Machines
			return q, at, true
		}
		return q, at, false
	}
	e.ID, e.State = nextID(q), "waiting"
	if len(body) > 0 {
		dir := filepath.Join(homeDir(), "queue")
		if err := os.MkdirAll(dir, 0o755); err == nil {
			e.Request = filepath.Join(dir, fmt.Sprintf("%d.json", e.ID))
			os.WriteFile(e.Request, body, 0o644)
		}
	}
	q = append(q, e)
	return q, len(q), true
}

func cmdQueue(args []string) error {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "drop":
		if len(args) < 2 {
			return fmt.Errorf("queue drop <id>")
		}
		return withLock(func() error {
			q := loadQueue()
			kept := slices.DeleteFunc(slices.Clone(q), func(e queued) bool { return fmt.Sprint(e.ID) == args[1] })
			if len(kept) == len(q) {
				return fmt.Errorf("no launch %s in the line", args[1])
			}
			fmt.Printf("fairshare: launch %s left the line\n", args[1])
			return saveQueue(kept)
		})
	case "move":
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
	case "add":
		// A launch that can run on more than this machine and found room on none of them. The
		// caller asked the machines; this only keeps its place.
		fs := flag.NewFlagSet("queue add", flag.ExitOnError)
		key := fs.String("key", "", "what names this launch")
		launcher := fs.String("launcher", "", "the thread that asks")
		label := fs.String("label", "", "its title")
		threads := fs.Int("threads", 0, "agent threads open on this machine now")
		count := fs.Int("count", 1, "new threads asked for at once")
		checkout := fs.Bool("checkout", false, "the work makes a checkout")
		repo := fs.String("repo", "", "the repo it works in")
		machines := fs.String("machines", "", "the machines it can run on, comma separated")
		request := fs.String("request", "", "the launch exactly as sent (file or -)")
		fs.Parse(args[1:])
		if *key == "" || *launcher == "" {
			return fmt.Errorf("queue add needs --key and --launcher")
		}
		var body []byte
		if *request == "-" {
			body, _ = io.ReadAll(os.Stdin)
		} else if *request != "" {
			body, _ = os.ReadFile(*request)
		}
		s, err := loadSettings()
		if err != nil {
			return err
		}
		e := queued{Key: *key, Launcher: *launcher, Title: *label, Threads: *threads, Count: max(*count, 1), Checkout: *checkout, Repo: *repo, QueuedAt: time.Now()}
		e.learned = loadLearned().levels("thread", *repo, "", nil, e.QueuedAt, s)
		for _, m := range strings.Split(*machines, ",") {
			if m = strings.TrimSpace(m); m != "" {
				e.Machines = append(e.Machines, m)
			}
		}
		if r, err := currentReading(s); err == nil {
			one, _ := estimate(r, s, Ask{Threads: e.Threads, Count: e.Count, Disk: e.Checkout, Learned: e.learned})
			e.Need = one.times(e.Count)
		}
		return withLock(func() error {
			q, at, added := join(loadQueue(), e, body)
			if added {
				if err := saveQueue(q); err != nil {
					return err
				}
			}
			return printJSON(map[string]any{"place": at, "of": len(q), "added": added, "notify": len(s.QueueNotify) > 0})
		})
	case "claim":
		// The launch whose turn came, sent again: it leaves the line, and on this machine it
		// counts as let in. Exit 0 with the entry when room was held for it, 1 while it still
		// waits, 3 when it is not in the line.
		fs := flag.NewFlagSet("queue claim", flag.ExitOnError)
		key := fs.String("key", "", "what names this launch")
		fs.Parse(args[1:])
		code := 3
		err := withLock(func() error {
			q := loadQueue()
			at, e := position(q, *key)
			if e == nil {
				return printJSON(map[string]any{"state": "none"})
			}
			if e.State != "offered" {
				code = 1
				return printJSON(map[string]any{"state": "waiting", "place": at, "of": len(q), "launcher": e.Launcher,
					"minutes": time.Since(e.QueuedAt).Minutes(), "id": e.ID})
			}
			code = 0
			got := *e
			if err := saveQueue(append(q[:at-1], q[at:]...)); err != nil {
				return err
			}
			if got.Machine == "" || got.Machine == here {
				s, _ := loadSettings()
				takeGrant(time.Now(), settle(s), grant{Need: got.Need, Label: got.Title, Repo: got.Repo, Count: got.Count})
			}
			appendLine("decisions.jsonl", map[string]any{"at": time.Now().UTC(), "event": "claimed", "id": got.ID, "title": got.Title,
				"machine": got.Machine, "waited_minutes": time.Since(got.QueuedAt).Minutes()}, 8<<20)
			return printJSON(map[string]any{"state": "offered", "machine": got.Machine, "id": got.ID, "minutes": time.Since(got.QueuedAt).Minutes()})
		})
		if err != nil {
			return err
		}
		os.Exit(code)
	}
	q := loadQueue()
	if sub == "--json" {
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
			state = fmt.Sprintf("room held for it on %s since %s, its launcher was told", e.Machine, e.OfferedAt.Local().Format("15:04"))
		}
		fmt.Printf("  %d. #%d %s (%s; can run on %s; from %s)\n", i+1, e.ID, e.Title, state, strings.Join(e.machines(), ", "), e.Launcher)
	}
	return nil
}
