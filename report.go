package main

// `fairshare report [--hours 24]` reads the event log: what every chat's calls became, what the
// long runs cost, what was stopped or sent back, and the pieces of agent work that ran longest.
// `fairshare decide` runs the rules alone over NDJSON calls, for checks against recorded traffic.

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

type event map[string]any

func (e event) str(k string) string  { s, _ := e[k].(string); return s }
func (e event) num(k string) float64 { return num(e[k]) }

func readEvents(since time.Time) []event {
	// the log and the files it rotated into (<name>-<time>.jsonl), oldest first
	base := strings.TrimSuffix(eventsFile(), ".jsonl")
	files, _ := filepath.Glob(base + "-*.jsonl")
	sort.Strings(files)
	files = append(files, base+".jsonl")
	var out []event
	cutoff := since.UTC().Format("2006-01-02T15:04:05.000Z")
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 1<<20), 1<<24)
		for sc.Scan() {
			var e event
			if json.Unmarshal(sc.Bytes(), &e) == nil && e.str("ts") >= cutoff {
				out = append(out, e)
			}
		}
		fh.Close()
	}
	return out
}

func pct(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	return s[min(len(s)-1, int(float64(len(s))*p))]
}

type counter map[string]int

func (c counter) top(n int) []string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return c[keys[i]] > c[keys[j]] || c[keys[i]] == c[keys[j]] && keys[i] < keys[j] })
	return keys[:min(n, len(keys))]
}

func cmdReport(args []string) int {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	hours := fs.Float64("hours", 24, "how far back to read")
	fs.Parse(args)
	evs := readEvents(time.Now().Add(-time.Duration(*hours * float64(time.Hour))))
	fmt.Printf("fairshare report: last %.0f h, %d events in %s\n", *hours, len(evs), filepath.Dir(eventsFile()))

	kinds, rules, agents, sessions := counter{}, counter{}, counter{}, map[string]bool{}
	var hookMs []float64
	var sent []event
	var runs, lanes, adopts []event
	errs := 0
	for _, e := range evs {
		switch e.str("ev") {
		case "call":
			kinds[e.str("kind")]++
			agents[e.str("agent")]++
			sessions[e.str("session")] = true
			hookMs = append(hookMs, e.num("hook_ms"))
			if rs, ok := e["rules"].([]any); ok {
				for _, r := range rs {
					rules[fmt.Sprint(r)]++
				}
			} else if r := e.str("rules"); r != "" {
				rules[r]++
			}
			if e.str("kind") == "deny" {
				sent = append(sent, e)
			}
		case "run":
			runs = append(runs, e)
		case "lane":
			lanes = append(lanes, e)
		case "adopt":
			adopts = append(adopts, e)
		case "error":
			errs++
		}
	}
	calls := len(hookMs)
	if calls > 0 {
		fmt.Printf("\ntool calls: %d from %d chats (%s) · hook time p50 %.0f ms, p99 %.0f ms\n", calls, len(sessions), joinCounts(agents), pct(hookMs, .5), pct(hookMs, .99))
		fmt.Printf("  passed %d · rewritten %d · sent back %d · waited %d · inputs changed %d · hook errors %d\n", kinds["pass"], kinds["rewrite"], kinds["deny"], kinds["wait"], kinds["input"], errs)
		for _, r := range rules.top(20) {
			fmt.Printf("  %6d  %s\n", rules[r], r)
		}
	}
	if len(sent) > 0 {
		fmt.Printf("\nsent back (%d):\n", len(sent))
		for _, e := range sent[max(0, len(sent)-10):] {
			fmt.Printf("  %s %s %s: %s\n", e.str("ts")[:16], e.str("agent"), e.str("rules"), clip(oneLine(e.str("cmd")), 120))
		}
	}
	if len(runs) > 0 {
		var wall, cpu, outB, sumB float64
		stops := counter{}
		for _, e := range runs {
			wall += e.num("wall_ms")
			cpu += e.num("cpu_ms")
			outB += e.num("out_bytes")
			sumB += e.num("summary_bytes")
			if s := e.str("stopped"); s != "" {
				stops[s]++
			}
		}
		fmt.Printf("\nlong runs: %d · %.1f min wall · %.1f CPU-min · output %.1f MB shown as %.2f MB (about %.0fk tokens kept out of context)\n",
			len(runs), wall/60000, cpu/60000, outB/(1<<20), sumB/(1<<20), (outB-sumB)/4/1000)
		if len(stops) > 0 {
			fmt.Printf("  stopped early: %s\n", joinCounts(stops))
		}
		slices.SortFunc(runs, func(a, b event) int { return int(b.num("wall_ms") - a.num("wall_ms")) })
		fmt.Println("  slowest:")
		for _, e := range runs[:min(8, len(runs))] {
			fmt.Printf("  %6.1fs cpu %6.1fs peak %5.0f MB exit %3.0f %-8s %s\n", e.num("wall_ms")/1000, e.num("cpu_ms")/1000, e.num("peak_mb"), e.num("exit"), e.str("stopped"), clip(oneLine(e.str("cmd")), 90))
		}
	}
	if len(lanes) > 0 {
		var wall, cpu, held, wait, outside float64
		for _, e := range lanes {
			wall += e.num("wall_ms")
			cpu += e.num("cpu_ms")
			held += e.num("held_ms")
			wait += e.num("cpu_wait_ms")
			outside += e.num("outside")
		}
		fmt.Printf("\nchats under adopt: %d · %.1f CPU-min · held back %.1f min · threads waited %.1f min for a CPU · %.0f processes refused a lane\n",
			len(lanes), cpu/60000, held/60000, wait/60000, outside)
		slices.SortFunc(lanes, func(a, b event) int { return int(b.num("cpu_ms") - a.num("cpu_ms")) })
		fmt.Println("  heaviest:")
		for _, e := range lanes[:min(8, len(lanes))] {
			fmt.Printf("  %-12s cpu %7.1fs over %6.1f min (peak %4.1f cores) peak %5.0f MB procs %4.0f held %5.1fs waited %6.1fs: %s\n", e.str("harness"), e.num("cpu_ms")/1000, e.num("wall_ms")/60000,
				e.num("peak_cores"), e.num("peak_mb"), e.num("procs"), e.num("held_ms")/1000, e.num("cpu_wait_ms")/1000, clip(e.str("top"), 90))
		}
	}
	reportSlots(evs)
	reportHeavy(evs)
	for _, e := range adopts {
		fmt.Printf("\nadopt ran %.1f min and used %.1f s CPU itself (%.2f%% of one core) over %.0f looks\n", e.num("wall_ms")/60000, e.num("self_cpu_ms")/1000, 100*e.num("self_cpu_ms")/max(1, e.num("wall_ms")), e.num("ticks"))
	}
	return 0
}

// reportSlots shows what heavy-job slots cost: minutes waited per heavy run, against those runs'
// own wall time and failures, and how many ran beside two or more others (the room rule, room.go).
// Compare windows before and after a slot change with --hours.
func reportSlots(evs []event) {
	var runs, refused, failed int
	var waited, wall float64
	beside := counter{}
	for _, e := range evs {
		if ev := e.str("ev"); ev != "slot" && ev != "run" || e["waited_ms"] == nil {
			continue
		}
		waited += e.num("waited_ms")
		if e.str("stopped") == "no-slot" {
			refused++
			continue
		}
		runs++
		wall += e.num("wall_ms")
		if e.num("exit") != 0 {
			failed++
		}
		beside[fmt.Sprint(e.num("slots_beside"))]++
	}
	if runs+refused == 0 {
		return
	}
	fmt.Printf("\nheavy runs: %d · %d refused for no slot · slot wait %.1f min (%.2f min per run) · wall %.2f min per run · failed %d (%.0f%%)\n",
		runs, refused, waited/60000, waited/60000/float64(max(1, runs)), wall/60000/float64(max(1, runs)), failed, 100*float64(failed)/float64(max(1, runs)))
	fmt.Printf("  granted beside n others: %s\n", joinCounts(beside))
}

// reportHeavy shows the heavy list (heavy.go) and what joined it in the window.
func reportHeavy(evs []event) {
	list, err := readHeavy()
	if err != nil {
		fmt.Printf("\nheavy list: %v\n", err)
		return
	}
	var added []event
	for _, e := range evs {
		if e.str("ev") == "heavy" {
			added = append(added, e)
		}
	}
	fmt.Printf("\nheavy list: %d commands wait for a slot (%s); %d joined in this window\n", len(list), heavyFile(), len(added))
	for _, e := range added {
		fmt.Printf("  joined %s  cpu %6.0fs  %s: %s\n", e.str("ts")[:16], e.num("cpu_ms")/1000, e.str("repo"), clip(e.str("cmd"), 90))
	}
	slices.SortFunc(list, func(a, b heavyEntry) int { return int(b.CPUSeconds - a.CPUSeconds) })
	for _, e := range list {
		fmt.Printf("  cpu %6.0fs wall %5.0fs peak %5.0f MB runs %3d last %s  %s: %s\n", e.CPUSeconds, e.WallSeconds, e.PeakMB, e.Runs, clip(e.LastSeen, 10), e.Repo, clip(e.Cmd, 90))
	}
}

func joinCounts(c counter) string {
	var parts []string
	for _, k := range c.top(len(c)) {
		parts = append(parts, fmt.Sprintf("%s %d", or(k, "?"), c[k]))
	}
	return strings.Join(parts, ", ")
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// cmdDecide reads {agent, tool, shell, cwd, command, input} lines and prints one decision per line.
func cmdDecide() int {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()
	for sc.Scan() {
		var in struct {
			Agent, Tool, Shell, Cwd string
			Command                 *string
			Input                   map[string]any
		}
		if err := json.Unmarshal(sc.Bytes(), &in); err != nil {
			w.WriteString("{\"kind\":\"bad-input\"}\n")
			continue
		}
		d := decide(Call{Agent: in.Agent, Tool: in.Tool, Shell: in.Shell, Cwd: in.Cwd, Command: in.Command, Input: in.Input})
		b, _ := json.Marshal(map[string]any{"kind": d.Kind, "rules": d.Rules, "command": d.Command, "patch": d.Patch, "input": d.Input, "reason": d.Reason})
		w.Write(b)
		w.WriteByte('\n')
	}
	return 0
}
