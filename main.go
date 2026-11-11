// fairshare says how much room a machine has for agent work, from what was measured on it.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

const usage = `fairshare: how much room this machine has for agent work, measured

  fairshare status [--json]        what the machine has, what work is taking, and whether a thread fits
  fairshare room [flags]           the answer a launch asks for: exit 0 room, 1 no room, 2 cannot tell
      --threads N                  agent threads open on this machine now (sizes the estimate)
      --count N                    new threads asked for at once (default 1)
      --checkout                   the work makes a checkout, so the disk is checked too
      --repo NAME                  the repo the work is in: it counts as what that repo's work was measured using
      --kind thread|run --what "<command>"
                                   what the work is: a thread (the default), or a run of that command
      --urgent                     judge it against the work that would not step aside, and hold the rest back
                                   (a title holding one of urgent_words is urgent too)
      --desktop                    the work drives the desktop (opens windows on it, moves the pointer, captures
                                   the screen): room only while nobody is at the PC, one piece at a time
      --cores X --mem-gb X         the caller's guess: where the estimate starts until work like it is measured
      --disk-gb X                  count its checkout as this
      --seen '<json>'              what another machine of the pool measured for this work (the pool passes it)
      --take --label "<what>"      when there is room, count this work until its load shows
      --queue --key K --launcher T --request <file|->
                                   when there is none, keep the launch in the line and tell T when its turn comes
      --reading <file|->           decide on a reading from another machine instead of this one's
      --json
  fairshare queue [--json]         the launches waiting for room, in order; "queue drop <id>" takes one out
  fairshare queue add|claim ...    for a launch that can run on several machines: keep its place, take its turn
  fairshare reading                this machine's reading as JSON
  fairshare history [--hours N]    what the meter saw: how full, how long work waited, the reserve unused
  fairshare learned [--json]       what each kind of work was measured using: threads by repo, runs by command
  fairshare pool                   every machine that joined, as one place: what is free, and where a thread fits
  fairshare pool place|run ...     put a piece of work where there is room, without naming a machine ("pool help")
  fairshare settings               the numbers in use and the file they come from
  fairshare meter                  the meter itself (on Windows install runs it; on Linux the first question starts it)
  fairshare install [--uninstall]  make the meter part of this PC (Windows)

Nothing here runs per command or touches another process. It replaced a fixed thread cap.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "status":
		err = cmdStatus(args)
	case "room":
		os.Exit(cmdRoom(args))
	case "reading":
		err = cmdReading()
	case "queue":
		err = cmdQueue(args)
	case "history":
		err = cmdHistory(args)
	case "settings":
		err = cmdSettings()
	case "learned", "repos":
		err = cmdLearned(args)
	case "pool":
		os.Exit(cmdPool(args))
	case "meter":
		err = runMeter()
	case "install":
		fs := flag.NewFlagSet("install", flag.ExitOnError)
		uninstall := fs.Bool("uninstall", false, "stop the meter and remove its task")
		fs.Parse(args)
		if err = install(*uninstall); err == nil {
			if *uninstall {
				fmt.Println("fairshare: meter stopped and its task removed; settings and history kept in", homeDir())
			} else {
				fmt.Println("fairshare: meter running; `fairshare status` shows what it sees")
			}
		}
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "fairshare: no command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "degraded:", err)
		os.Exit(2)
	}
}

func printJSON(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

func cmdReading() error {
	s, serr := loadSettings()
	r, err := currentReading(s)
	if err != nil {
		return err
	}
	if serr != nil {
		r.Notes = append(r.Notes, serr.Error())
	}
	return printJSON(r)
}

func cmdSettings() error {
	s, err := loadSettings()
	fmt.Println(settingsFile())
	if perr := printJSON(s); perr != nil {
		return perr
	}
	return err
}

// readingFrom reads a reading another machine printed, from a file or stdin.
func readingFrom(path string) (Reading, error) {
	var r Reading
	var b []byte
	var err error
	if path == "-" {
		b, err = io.ReadAll(os.Stdin)
	} else {
		b, err = os.ReadFile(path)
	}
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return r, fmt.Errorf("reading does not parse: %w", err)
	}
	if r.Capacity <= 0 || r.MemTotalGB <= 0 {
		return r, fmt.Errorf("reading names no processors or memory")
	}
	return r, nil
}

type decision struct {
	At      time.Time `json:"at"`
	OK      bool      `json:"ok"`
	Took    bool      `json:"took"`
	Label   string    `json:"label,omitempty"`
	Repo    string    `json:"repo,omitempty"`
	Kind    string    `json:"kind,omitempty"`
	What    string    `json:"what,omitempty"`
	Urgent  bool      `json:"urgent,omitempty"`
	Machine string    `json:"machine"`
	Threads int       `json:"threads"`
	Count   int       `json:"count"`
	CPU     Limit     `json:"cpu"`
	Mem     Limit     `json:"mem"`
	Disk    Limit     `json:"disk"`
	// Sized is what the work was counted as and from what, and Open the open work as counted.
	Sized  Sizing   `json:"sized"`
	Open   OpenWork `json:"open"`
	State  string   `json:"presence"`
	Reason string   `json:"reason"`
}

func settle(s Settings) time.Duration { return time.Duration(s.SettleMinutes * float64(time.Minute)) }

func cmdRoom(args []string) int {
	fs := flag.NewFlagSet("room", flag.ExitOnError)
	threads := fs.Int("threads", 0, "agent threads open on this machine now")
	count := fs.Int("count", 1, "new threads asked for at once")
	checkout := fs.Bool("checkout", false, "the work makes a checkout")
	cores := fs.Float64("cores", 0, "count one new thread as this many processors")
	memGB := fs.Float64("mem-gb", 0, "count one new thread as this much memory")
	diskGB := fs.Float64("disk-gb", 0, "count one new thread as this much disk")
	take := fs.Bool("take", false, "when there is room, count this work until its load shows")
	label := fs.String("label", "", "what the work is, for the record")
	from := fs.String("reading", "", "a reading from another machine (file or -)")
	wait := fs.Bool("queue", false, "when there is no room, keep the launch in the line (needs --key and --launcher)")
	key := fs.String("key", "", "what names this launch; the same launch sent again carries the same key")
	launcher := fs.String("launcher", "", "the thread that asks, told when there is room")
	request := fs.String("request", "", "the launch exactly as sent (file or -), kept with it in the line")
	asJSON := fs.Bool("json", false, "print the whole answer as JSON")
	repo := fs.String("repo", "", "the repo the work is in")
	kind := fs.String("kind", "thread", "what the work is: thread or run")
	what := fs.String("what", "", "a run's command")
	seenFlag := fs.String("seen", "", "what another machine of the pool measured for this work, as JSON")
	urgentFlag := fs.Bool("urgent", false, "judge it against the work that would not step aside")
	desktop := fs.Bool("desktop", false, "the work drives the desktop: room only while nobody is at the PC, one piece at a time")
	fs.Parse(args)

	fail := func(err error) int {
		if *asJSON {
			printJSON(map[string]any{"ok": nil, "degraded": err.Error()})
		}
		fmt.Fprintln(os.Stderr, "degraded:", err)
		return 2
	}
	s, err := loadSettings()
	if err != nil {
		return fail(err)
	}
	now := time.Now()
	if *kind != "run" {
		*kind, *what = "thread", ""
	}
	var seen *Sizing
	if *seenFlag != "" {
		var got Sizing
		if json.Unmarshal([]byte(*seenFlag), &got) != nil {
			return fail(fmt.Errorf("--seen is not a sizing: %s", *seenFlag))
		}
		seen = &got
	}
	ask := Ask{Threads: *threads, Count: *count, Explicit: Need{*cores, *memGB, *diskGB}, Disk: *checkout, Kind: *kind, Desktop: *desktop}
	let := grant{Label: *label, Kind: *kind, Repo: *repo, Count: max(*count, 1), Desktop: *desktop}
	letIn := func(need Need) bool {
		let.Need = need
		return takeGrant(now, settle(s), let) == nil
	}
	urgent, urgentWhy := isUrgent(*urgentFlag, *label, *repo, s)
	if *from != "" { // another machine's reading: the rule only, no record and no line
		r, err := readingFrom(*from)
		if err != nil {
			return fail(err)
		}
		ask.Urgent = urgent
		return answer(decide(r, s, ask, Need{}), *asJSON)
	}
	r, err := currentReading(s)
	if err != nil {
		return fail(err)
	}
	ask.Learned = loadLearned().levels(*kind, *repo, *what, seen, now, s)
	ask.DesktopTaken = *desktop && desktopPending(now, settle(s), r.Work)
	// Only a machine where Owl holds agent work can put other work behind an urgent launch.
	urgent = urgent && r.OS == "windows"
	var body []byte
	if *request == "-" {
		body, _ = io.ReadAll(os.Stdin)
	} else if *request != "" {
		body, _ = os.ReadFile(*request)
	}
	var v Verdict
	took := false
	err = withLock(func() error {
		q := loadQueue()
		at, held := position(q, *key)
		if *key != "" && held != nil && held.State == "offered" && (held.Machine == "" || held.Machine == here) {
			// Its turn: the room was held for it, so it goes in on what it was offered at.
			v = decide(r, s, ask, Need{})
			v.OK, v.CPU.OK, v.Mem.OK, v.Disk.OK = true, true, true, true
			v.Reason = fmt.Sprintf("%s: this launch waited in the line %.0f minutes and room was held for it; it goes in now.", r.Machine, now.Sub(held.QueuedAt).Minutes())
			need := held.Need
			q = append(q[:at-1], q[at:]...)
			if err := saveQueue(q); err != nil {
				return err
			}
			took = letIn(need)
			return nil
		}
		if urgent && (*key == "" || held == nil) {
			// Urgent: it passes the line, and is judged against the work that would not step
			// aside for it: the user's, the sessions let in since the hold began, and room held
			// for launches told their turn.
			hold, standing := loadUrgent()
			standing = standing && now.Before(hold.Last.Add(minutes(s.UrgentMostMinutes)))
			var since Need
			if standing {
				since = pendingSinceNeed(now, settle(s), hold.Since)
			}
			ua := ask
			ua.Urgent = true
			v = decideBehind(r, s, ua, since.plus(lineNeed(q, true)), Need{})
			v.Reason += " Urgent because " + urgentWhy + "."
			if !v.OK || !*take {
				return nil
			}
			took = letIn(Need{v.CPU.Need, v.Mem.Need, v.Disk.Need})
			// A hold is only what Owl took: when it cannot be told, nothing steps aside, and
			// nothing is recorded as if it did.
			hold = hold.join(now, *label, *repo, standing)
			if err := tellYield(s, hold.Since); err != nil {
				// The answer said the other work is held; it is not.
				if i, j := strings.Index(v.Reason, " The other agent work"), strings.Index(v.Reason, "stay yours."); i >= 0 && j > i {
					v.Reason = v.Reason[:i] + v.Reason[j+len("stay yours."):]
				}
				v.Reason += " degraded: Owl was not told to hold the other work back, so it runs beside it: " + err.Error()
				return nil
			}
			if err := saveUrgent(hold); err != nil {
				v.Reason += " degraded: the hold was not recorded: " + err.Error()
			}
			return nil
		}
		// A new arrival passes the line only when there is room for everyone in it too.
		v = decideBehind(r, s, ask, pendingNeed(now, settle(s), r.Work), lineNeed(q, false))
		if *key != "" && held != nil {
			v.OK = false
			v.Reason = fmt.Sprintf("%s: this launch is already in the line, place %d of %d, waiting %.0f minutes. It is not lost: fairshare tells %s the moment there is room, and it goes in then. Do not send it again before that.",
				r.Machine, at, len(q), now.Sub(held.QueuedAt).Minutes(), held.Launcher)
			return nil
		}
		if v.OK {
			if *take {
				took = letIn(Need{v.CPU.Need, v.Mem.Need, v.Disk.Need})
			}
			return nil
		}
		// Work that drives the desktop is not kept in the line: the line is offered room by
		// processors, and this waits for the person to leave (`pool run --wait`).
		if !*wait || *key == "" || *launcher == "" || *desktop {
			return nil
		}
		q, _, _ = join(q, queued{Key: *key, Launcher: *launcher, Title: *label, Threads: *threads, Count: max(*count, 1), Checkout: *checkout,
			Repo: *repo, Need: Need{v.CPU.Need, v.Mem.Need, v.Disk.Need}, QueuedAt: now}, body)
		if err := saveQueue(q); err != nil {
			return err
		}
		v.Reason += fmt.Sprintf(" It is now in the line, place %d: fairshare watches for room and tells %s the moment there is; send the same launch again then and it goes straight in. Do not send it again before that.", len(q), *launcher)
		if len(s.QueueNotify) == 0 {
			v.Reason += " degraded: queue_notify is not set, so nobody will be told; `fairshare queue` shows the line."
		}
		return nil
	})
	if err != nil {
		return fail(err)
	}
	v.Measured = measured(ask.Learned, r.Machine)
	appendLine("decisions.jsonl", decision{At: now.UTC(), OK: v.OK, Took: took, Label: *label, Repo: *repo, Kind: *kind, What: *what, Urgent: v.Urgent, Machine: v.Machine, Threads: v.Threads,
		Count: v.Count, CPU: v.CPU, Mem: v.Mem, Disk: v.Disk, Sized: v.Sized, Open: v.Open, State: r.Presence.State, Reason: v.Reason}, 8<<20)
	return answer(v, *asJSON)
}

func answer(v Verdict, asJSON bool) int {
	if asJSON {
		printJSON(v)
	} else {
		fmt.Println(v.Reason)
	}
	if v.OK {
		return 0
	}
	return 1
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print as JSON")
	threads := fs.Int("threads", 0, "agent threads open on this machine now")
	fs.Parse(args)
	s, err := loadSettings()
	if err != nil {
		return err
	}
	r, err := currentReading(s)
	if err != nil {
		return err
	}
	now := time.Now()
	v := decide(r, s, Ask{Threads: *threads, Count: 1, Disk: true, Learned: loadLearned().levels("thread", "", "", nil, now, s)}, pendingNeed(now, settle(s), r.Work))
	if *asJSON {
		return printJSON(v)
	}
	fmt.Print(statusText(v, s))
	return nil
}

func statusText(v Verdict, s Settings) string {
	r := v.Reading
	var b strings.Builder
	who := map[string]string{"full-screen": "full screen: " + r.Presence.App, "active": "someone is at the PC",
		"away": fmt.Sprintf("nobody at the PC for %.0f minutes", r.Presence.IdleMinutes), "none": "no screen"}[r.Presence.State]
	fmt.Fprintf(&b, "%s (%s), %s\n\n", r.Machine, r.OS, who)
	row := func(label, text string) { fmt.Fprintf(&b, "  %-16s %s\n", label, text) }
	if r.Presence.State == "none" {
		row("processors", fmt.Sprintf("%.0f, all for agent work, filled to %.0f%% (%.1f)", r.Capacity, v.Target*100, v.CPU.Limit))
	} else {
		row("processors", fmt.Sprintf("%d, %d kept for you, %.0f for agent work, filled to %.0f%% (%.1f)", r.Processors, r.Reserved, r.Capacity, v.Target*100, v.CPU.Limit))
	}
	if r.ReserveWhy != "" {
		row("kept because", r.ReserveWhy)
	}
	use := func(busy, waiting float64) string {
		if waiting >= 1 {
			return fmt.Sprintf("%.1f in use, and %.0f more threads waited while they were full", busy, waiting)
		}
		return fmt.Sprintf("%.1f in use", busy)
	}
	row("last "+span(r.ShortSeconds), use(r.BusyShort, r.WaitingShort))
	row("last "+span(r.LongSeconds), use(r.BusyLong, r.WaitingLong))
	if r.Reserved > 0 {
		row("your side", fmt.Sprintf("%.1f of %d in use, %.1f of it your own programs", r.UserBusy, r.Reserved, r.UserCores))
	}
	row("memory", fmt.Sprintf("%.1f of %.0f GB free, %.1f kept free", r.MemAvailGB, r.MemTotalGB, r.MemTotalGB*s.MemReserveFraction))
	row("disk", fmt.Sprintf("%.1f GB free on %s, %.0f kept free", r.DiskFreeGB, r.DiskPath, s.DiskReserveGB))
	if v.Open.Pieces > 0 {
		text := fmt.Sprintf("%d pieces, using %.1f processors this minute, expected to use %.1f", v.Open.Pieces, v.Open.Now, v.Open.Expect)
		if v.Open.Pieces == 1 {
			text = fmt.Sprintf("1 piece, using %.1f processors this minute, expected to use %.1f", v.Open.Now, v.Open.Expect)
		}
		if v.Open.Swing >= 0.1 {
			text += fmt.Sprintf(" give or take %.1f", v.Open.Swing)
		}
		row("open work", fmt.Sprintf("%s; counted as %.1f in use", text, v.CPU.Used))
	}
	if v.CPU.Pending > 0 {
		row("just let in", fmt.Sprintf("counted as %.1f processors until it is seen running", v.CPU.Pending))
	}
	if len(r.Others) > 0 {
		text := programWords(r.Others, 5)
		if r.OtherCores > 0 {
			text = fmt.Sprintf("%.1f of the processors in use above: %s", r.OtherCores, text)
		}
		row("everything else", text)
	}
	if r.FrameMs > 0 {
		text := fmt.Sprintf("a frame takes %.0f ms, the slowest in the last %s %.0f ms", r.FrameMs, span(r.ShortSeconds), r.FrameWorstMs)
		if r.FrameWorstMs >= 10*r.FrameMs {
			text += ": it stalled, which is what work drawing on or capturing the screen does"
		}
		row("screen", text)
	}
	if r.Urgent != nil {
		row("urgent first", fmt.Sprintf("since %s (%s): agent sessions older than that are held at idle priority, %.1f processors of it now; work that does not step aside uses %.1f",
			r.Urgent.Since.Local().Format("15:04"), strings.Join(r.Urgent.Labels, "; "), r.IdleCores, max(r.FirmShort, r.FirmLong)))
	}
	if q := loadQueue(); len(q) > 0 {
		row("in the line", fmt.Sprintf("%d launches wait for room, the first for %.0f minutes (`fairshare queue`)", len(q), time.Since(q[0].QueuedAt).Minutes()))
	}
	if len(r.Work) > 0 {
		fmt.Fprintf(&b, "\n  open work (processors now, expected, give or take; GB now, expected; its own minutes):\n")
		for i, w := range r.Work {
			if i == 12 {
				fmt.Fprintf(&b, "    and %d more\n", len(r.Work)-i)
				break
			}
			fmt.Fprintf(&b, "    %-52.52s %5.1f %5.1f %5.1f  %5.1f %5.1f  %5.0f\n", workWords(w), w.Now, w.Expect, w.Swing, w.MemGB, w.ExpectMemGB, w.Minutes)
		}
	}
	if len(r.Programs) > 0 {
		fmt.Fprintf(&b, "\n  agent work by program, last %s (processors, GB):\n", span(r.ShortSeconds))
		for _, p := range r.Programs {
			fmt.Fprintf(&b, "    %-28.28s %5.1f %6.1f   %d running\n", p.Name, p.Cores, p.MemGB, p.Processes)
		}
	}
	for _, n := range r.Notes {
		fmt.Fprintf(&b, "\n  note: %s\n", n)
	}
	fmt.Fprintf(&b, "\n%s\n", v.Reason)
	return b.String()
}

func cmdLearned(args []string) error {
	fs := flag.NewFlagSet("learned", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print as JSON")
	fs.Parse(args)
	s, err := loadSettings()
	if err != nil {
		return err
	}
	t := loadLearned().table(time.Now(), s)
	if *asJSON {
		return printJSON(t)
	}
	if len(t) == 0 {
		fmt.Println("Nothing has been measured yet: the meter records what each thread, run and job uses once a minute.")
		return nil
	}
	fmt.Printf("What work was measured using here. Minutes fade by half every %.0f days; %.0f minutes weigh as much as everything wider.\n", s.LearnHalfLifeDays, s.LearnTrustMinutes)
	fmt.Printf("\n  %-58s %6s %6s %6s %8s %6s %8s\n", "WORK", "PROC", "+/-", "GB", "MINUTES", "ENDED", "LASTS")
	for _, n := range t {
		name := map[string]string{"thread": "every thread", "run": "every run", "job": "every job outside a thread"}[n.Kind]
		switch {
		case n.What != "":
			name = fmt.Sprintf("%s in %s: %s", strings.TrimSuffix(kindWords(n.Kind), "s"), n.Repo, n.What)
		case n.Repo != "":
			name = fmt.Sprintf("%s in %s", kindWords(n.Kind), n.Repo)
		}
		took := "-"
		if n.Seconds >= 1 && n.Kind == "run" {
			took = lasts(n.Seconds)
		}
		fmt.Printf("  %-58.58s %6.1f %6.1f %6.1f %8.0f %6.0f %8s\n", name, n.Cores, n.Swing, n.MemGB, n.Minutes, n.Works, took)
	}
	return nil
}
