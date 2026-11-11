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
      --cores X --mem-gb X --disk-gb X   count one new thread as this instead of the estimate
      --take --label "<what>"      when there is room, count this work until its load shows
      --reading <file|->           decide on a reading from another machine instead of this one's
      --json
  fairshare reading                this machine's reading as JSON
  fairshare history [--hours N]    what the meter saw: how full, how long work waited, the reserve unused
  fairshare settings               the numbers in use and the file they come from
  fairshare meter                  the meter itself (Windows; install runs it)
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
	case "history":
		err = cmdHistory(args)
	case "settings":
		err = cmdSettings()
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
	Machine string    `json:"machine"`
	Threads int       `json:"threads"`
	Count   int       `json:"count"`
	CPU     Limit     `json:"cpu"`
	Mem     Limit     `json:"mem"`
	Disk    Limit     `json:"disk"`
	State   string    `json:"presence"`
	Reason  string    `json:"reason"`
}

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
	asJSON := fs.Bool("json", false, "print the whole answer as JSON")
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
	var r Reading
	if *from != "" {
		r, err = readingFrom(*from)
	} else {
		r, err = currentReading(s)
	}
	if err != nil {
		return fail(err)
	}
	now := time.Now()
	settle := time.Duration(s.SettleMinutes * float64(time.Minute))
	var pending Need
	if *from == "" { // the record of threads just let in is this machine's own
		pending = pendingNeed(now, settle)
	}
	ask := Ask{Threads: *threads, Count: *count, Explicit: Need{*cores, *memGB, *diskGB}, Disk: *checkout}
	v := decide(r, s, ask, pending)
	took := false
	if v.OK && *take && *from == "" {
		if err := takeGrant(now, settle, Need{v.CPU.Need, v.Mem.Need, v.Disk.Need}, *label); err != nil {
			fmt.Fprintln(os.Stderr, "degraded: the launch was not recorded:", err)
		} else {
			took = true
		}
	}
	if *from == "" {
		appendLine("decisions.jsonl", decision{At: now.UTC(), OK: v.OK, Took: took, Label: *label, Machine: v.Machine, Threads: v.Threads,
			Count: v.Count, CPU: v.CPU, Mem: v.Mem, Disk: v.Disk, State: r.Presence.State, Reason: v.Reason}, 8<<20)
	}
	if *asJSON {
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
	settle := time.Duration(s.SettleMinutes * float64(time.Minute))
	v := decide(r, s, Ask{Threads: *threads, Count: 1, Disk: true}, pendingNeed(time.Now(), settle))
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
	row("last "+span(r.ShortSeconds), fmt.Sprintf("%.1f in use, %.1f waiting for one", r.BusyShort, r.WaitingShort))
	row("last "+span(r.LongSeconds), fmt.Sprintf("%.1f in use, %.1f waiting for one", r.BusyLong, r.WaitingLong))
	if r.Reserved > 0 {
		row("your side", fmt.Sprintf("%.1f of %d in use, %.1f of it your own programs", r.UserBusy, r.Reserved, r.UserCores))
	}
	row("memory", fmt.Sprintf("%.1f of %.0f GB free, %.1f kept free", r.MemAvailGB, r.MemTotalGB, r.MemTotalGB*s.MemReserveFraction))
	row("disk", fmt.Sprintf("%.1f GB free on %s, %.0f kept free", r.DiskFreeGB, r.DiskPath, s.DiskReserveGB))
	if v.CPU.Pending > 0 {
		row("just let in", fmt.Sprintf("counted as %.1f processors and %.1f GB until their load shows", v.CPU.Pending, v.Mem.Pending))
	}
	if len(r.Others) > 0 {
		row("everything else", programWords(r.Others, 5))
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
