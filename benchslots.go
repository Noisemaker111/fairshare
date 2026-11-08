package main

// `fairshare bench slots` runs a real heavy command as 1, 2, 3 and 4 copies at once and records
// each copy's wall time and exit code with the whole PC's CPU and free memory during the batch. It
// is the measurement behind the heavy-job slot thresholds (slot.go): a slot past the first two is
// worth granting only while copies neither fail nor slow down more than the queue time they save.
//
// Other chats keep loading this PC while it runs, so it alternates the order of the counts each
// round and measures the PC's own load for a few seconds before every batch.

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

type slotBenchBatch struct {
	At         string    `json:"at"`
	Cmd        string    `json:"cmd"`
	N          int       `json:"n"`
	Round      int       `json:"round"`
	WallS      []float64 `json:"wall_s"`
	Exits      []int     `json:"exits"`
	CPUBefore  float64   `json:"cpu_before_pct"` // whole PC, the seconds before the batch
	CPUDuring  float64   `json:"cpu_during_pct"` // whole PC, mean over the batch
	AvailMinMB uint64    `json:"avail_min_mb"`   // least free physical memory during the batch
	CPUs       int       `json:"cpus"`
}

func benchSlots(args []string) int {
	fs := flag.NewFlagSet("bench slots", flag.ExitOnError)
	at := fs.String("at", "1,2,3,4", "copies at once, one batch each")
	rounds := fs.Int("rounds", 1, "times through the counts, alternating order")
	dir := fs.String("dir", "", "folder each copy runs in; {i} becomes the copy's number (default: here)")
	logs := fs.String("logs", "", "folder for each copy's output (default: a new temp folder)")
	before := fs.Duration("before", 8*time.Second, "PC load measured before each batch")
	out := fs.String("out", "", "append one JSON line per batch to this file")
	fs.Parse(args)
	cmdArgs := fs.Args()
	if len(cmdArgs) == 0 {
		fmt.Fprintln(os.Stderr, "usage: fairshare bench slots [flags] -- command args...")
		return 2
	}
	var counts []int
	for _, s := range strings.Split(*at, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || n < 1 {
			fmt.Fprintln(os.Stderr, "fairshare bench slots: --at needs counts like 1,2,3,4")
			return 2
		}
		counts = append(counts, n)
	}
	if _, err := killOnExitJob(); err != nil {
		fmt.Fprintln(os.Stderr, "fairshare bench slots:", err)
		return 1
	}
	if *logs == "" {
		d, err := os.MkdirTemp("", "fairshare-bench-slots-")
		if err != nil {
			fmt.Fprintln(os.Stderr, "fairshare bench slots:", err)
			return 1
		}
		*logs = d
	}
	os.MkdirAll(*logs, 0o755)
	text := strings.Join(cmdArgs, " ")
	fmt.Printf("%d logical CPUs; %q at %v, %d round(s); output in %s\n", runtime.NumCPU(), text, counts, *rounds, *logs)
	failed := false
	for r := range *rounds {
		order := slices.Clone(counts)
		if r%2 == 1 {
			slices.Reverse(order)
		}
		for _, n := range order {
			var cs cpuSampler
			cs.sample()
			time.Sleep(*before)
			b := slotBenchBatch{At: time.Now().UTC().Format(time.RFC3339), Cmd: text, N: n, Round: r, CPUBefore: round1(cs.sample()), CPUs: runtime.NumCPU()}
			b.WallS, b.Exits = make([]float64, n), make([]int, n)
			stop := make(chan struct{})
			var during []float64
			b.AvailMinMB = ^uint64(0)
			var sampled sync.WaitGroup
			sampled.Add(1)
			go func() {
				defer sampled.Done()
				for {
					select {
					case <-stop:
						return
					case <-time.After(time.Second):
						during = append(during, cs.sample())
						if a, _, _ := MemStatus(); a < b.AvailMinMB {
							b.AvailMinMB = a
						}
					}
				}
			}()
			var wg sync.WaitGroup
			for i := range n {
				wg.Add(1)
				go func() {
					defer wg.Done()
					c := exec.Command(cmdArgs[0], cmdArgs[1:]...)
					if *dir != "" {
						c.Dir = strings.ReplaceAll(*dir, "{i}", strconv.Itoa(i))
					}
					c.Env = append(os.Environ(), heldEnv+"=1") // the bench is the slot holder
					if f, err := os.Create(filepath.Join(*logs, fmt.Sprintf("r%d-n%d-c%d.log", r, n, i))); err == nil {
						c.Stdout, c.Stderr = f, f
						defer f.Close()
					}
					t := time.Now()
					code := 0
					if err := c.Run(); err != nil {
						code = 127
						if ee, ok := err.(*exec.ExitError); ok {
							code = ee.ExitCode()
						}
					}
					b.WallS[i], b.Exits[i] = round1(time.Since(t).Seconds()), code
				}()
			}
			wg.Wait()
			close(stop)
			sampled.Wait()
			if len(during) > 0 {
				var sum float64
				for _, x := range during {
					sum += x
				}
				b.CPUDuring = round1(sum / float64(len(during)))
			}
			for _, c := range b.Exits {
				failed = failed || c != 0
			}
			fmt.Printf("round %d, %d at once: wall %v s, exits %v, PC CPU %.0f%% before, %.0f%% during, least free memory %d MB\n",
				r, n, b.WallS, b.Exits, b.CPUBefore, b.CPUDuring, b.AvailMinMB)
			if *out != "" {
				if f, err := os.OpenFile(*out, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
					json.NewEncoder(f).Encode(b)
					f.Close()
				}
			}
		}
	}
	if failed {
		fmt.Println("some copies failed; their output is in", *logs)
		return 1
	}
	return 0
}
