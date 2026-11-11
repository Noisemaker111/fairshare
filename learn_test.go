package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func near(a, b float64) bool { return math.Abs(a-b) < 0.006 }

func TestWorkCountsAsWhatWorkLikeItWasMeasuredUsing(t *testing.T) {
	s := defaultSettings()
	now := time.Date(2026, 11, 11, 18, 0, 0, 0, time.UTC)
	l := learned{}
	// What the meter recorded on a PC: Owl threads build Rust, AppForGutters threads wait on
	// their model and then run a gate, and the AppForGutters gate is a run of its own.
	for range 120 {
		l.add(now, stretch{Kind: "thread", Repo: "owl-go", Minutes: 1, Cores: 4.5, MemGB: 6}, s)
	}
	for i := range 200 {
		cores := 0.1
		if i%10 == 0 {
			cores = 6
		}
		l.add(now, stretch{Kind: "thread", Repo: "AppForGutters", Minutes: 1, Cores: cores, MemGB: 1.5}, s)
	}
	for range 12 {
		l.add(now, stretch{Kind: "run", Repo: "appforgutters", What: "bun run gate", Minutes: 5, Cores: 5.2, MemGB: 6.5}, s)
		l.add(now, stretch{Kind: "run", Repo: "appforgutters", What: "bun run gate", Ended: true, Seconds: 300}, s)
		l.add(now, stretch{Kind: "run", Repo: "appforgutters", What: "bun run lint", Minutes: 0.5, Cores: 1.1, MemGB: 0.8, Ended: true, Seconds: 30}, s)
	}
	floor := Need{Cores: s.ThreadCores, MemGB: s.ThreadMemGB}
	sized := func(kind, repo, what string, base Need, seen *Sizing) Sizing {
		return size(base, l.levels(kind, repo, what, seen, now, s), s.LearnTrustMinutes)
	}

	owl := sized("thread", "owl-go", "", floor, nil)
	if !near(owl.Cores, 4.5-0.5*10/130*0-(4.5-owl.Cores)) || owl.Cores < 4.2 || owl.Swing > 1.6 || owl.From != "owl-go threads here" || !near(owl.Minutes, 120) {
		t.Errorf("an Owl thread: %+v", owl)
	}
	// The same average can hide very different threads: an AppForGutters thread averages 0.7
	// processors, but in bursts of 6, and its spread says so.
	afg := sized("thread", "appforgutters", "", floor, nil)
	if afg.Cores < 0.6 || afg.Cores > 0.9 || afg.Swing < 1.6 || afg.Swing > 1.9 || afg.From != "appforgutters threads here" {
		t.Errorf("an AppForGutters thread: %+v", afg)
	}
	// A run is its command's own: the gate is not the lint, and neither is "a run in this repo".
	gate := sized("run", "appforgutters", "bun run gate", floor, nil)
	lint := sized("run", "appforgutters", "bun run lint", floor, nil)
	if gate.Cores < 4.9 || gate.Cores > 5.2 || gate.MemGB < 6 || !near(gate.Seconds, 300) || gate.From != "`bun run gate` in appforgutters here" {
		t.Errorf("the gate: %+v", gate)
	}
	if lint.Cores > 2.5 || !near(lint.Seconds, 30) {
		t.Errorf("the lint: %+v", lint)
	}
	// A command never run here counts as this repo's runs, minute for minute: mostly the gate.
	other := sized("run", "appforgutters", "bun run build", floor, nil)
	if other.From != "appforgutters runs here" || other.Cores < 4.5 || other.Cores > 5.2 {
		t.Errorf("an unmeasured command in a measured repo: %+v", other)
	}
	// A repo never seen here counts as every thread here, and with nothing measured at all, as the floor.
	if got := sized("thread", "newrepo", "", floor, nil); got.From != "threads here" || got.Cores < 1.9 || got.Cores > 2.3 {
		t.Errorf("an unmeasured repo: %+v", got)
	}
	if got := size(floor, learned{}.levels("thread", "newrepo", "", nil, now, s), s.LearnTrustMinutes); got.From != "" || got.Cores != 1 || got.MemGB != 2 || got.Swing != 0 {
		t.Errorf("with nothing measured: %+v", got)
	}

	// A caller's own number is where the estimate starts, and no more than that: ten measured
	// minutes already count for half, and the gate's hour for nearly all.
	guess := Need{Cores: 4, MemGB: 4}
	if got := size(guess, nil, s.LearnTrustMinutes); got.Cores != 4 || got.MemGB != 4 {
		t.Errorf("a guess with nothing measured: %+v", got)
	}
	if got := size(guess, []level{{Name: "x", Minutes: 10, Cores: 1, MeanSq: 1, MemGB: 1}}, s.LearnTrustMinutes); !near(got.Cores, 2.5) || !near(got.MemGB, 2.5) {
		t.Errorf("a guess against ten measured minutes: %+v", got)
	}
	if got := sized("run", "appforgutters", "bun run gate", Need{Cores: 1, MemGB: 1}, nil); got.Cores < 4.9 {
		t.Errorf("`--cores 1` against an hour of the gate at 5.2: %+v", got)
	}

	// What this machine measured for exactly this work is what the pool passes on.
	m := measured(l.levels("run", "appforgutters", "bun run gate", nil, now, s), "pc")
	if m == nil || m.Cores != 5.2 || m.MemGB != 6.5 || !near(m.Minutes, 60) || m.From != "`bun run gate` in appforgutters on pc" {
		t.Fatalf("measured here: %+v", m)
	}
	if measured(l.levels("thread", "newrepo", "", nil, now, s), "pc") != nil {
		t.Error("every thread here was passed on as this repo's")
	}
	// A host that never ran the gate takes the PC's word for it; its own minutes then take over.
	host := learned{}
	fresh := size(floor, host.levels("run", "appforgutters", "bun run gate", m, now, s), s.LearnTrustMinutes)
	if fresh.Cores < 4.5 || fresh.From != "`bun run gate` in appforgutters on pc" {
		t.Errorf("a machine told what another measured: %+v", fresh)
	}
	for range 30 {
		host.add(now, stretch{Kind: "run", Repo: "appforgutters", What: "bun run gate", Minutes: 1, Cores: 7.5, MemGB: 5}, s)
	}
	own := size(floor, host.levels("run", "appforgutters", "bun run gate", m, now, s), s.LearnTrustMinutes)
	if own.Cores < 6.8 || own.From != "`bun run gate` in appforgutters here" {
		t.Errorf("its own half hour against another machine's word: %+v", own)
	}

	// Old minutes fade: after two half-lives a quarter is left, and a repo that changed is re-learned.
	later := now.Add(2 * s.halfLife())
	if lv := l.levels("thread", "owl-go", "", nil, later, s); len(lv) != 2 || !near(lv[1].Minutes, 30) || !near(lv[1].Cores, 4.5) {
		t.Errorf("after two half-lives: %+v", lv)
	}
	for range 60 {
		l.add(later, stretch{Kind: "thread", Repo: "owl-go", Minutes: 1, Cores: 1, MemGB: 2}, s)
	}
	if got := size(floor, l.levels("thread", "owl-go", "", nil, later, s), s.LearnTrustMinutes); got.Cores > 2.4 {
		t.Errorf("an hour at 1 after the old 4.5 faded: %+v", got)
	}
	l.forget(later.Add(40*s.halfLife()), s)
	if len(l) != 0 {
		t.Errorf("what faded to nothing is still kept: %d", len(l))
	}
}

func TestARunIsNamedByItsCommand(t *testing.T) {
	for want, argv := range map[string][]string{
		"bun install && bun run gate":   {"bash", "-c", "bun install && bun run gate"},
		"bun run gate":                  {`C:\Program Files\Git\bin\bash.exe`, "-lc", "bun  run   gate"},
		"cargo test --workspace":        {"cargo", "test", "--workspace"},
		"node scripts/land.mjs --pr #":  {"node", "scripts/land.mjs", "--pr", "12345"},
		"git checkout # && go test ./.": {"sh", "-c", "git checkout 147faa3c9d && go test ./."},
		"python3 -c print(1)":           {"python3", "-c", "print(1)"},
		// On Windows a command starts with the whole path of its program, which says nothing.
		`python c:\work\demo\burn.py 2 12`: {`C:\Users\Jk101\AppData\Local\Programs\Python\Python312\python.exe`, `C:\work\demo\burn.py`, "2", "12"},
		"go test ./...":                    {"/usr/local/go/bin/go", "test", "./..."},
	} {
		if got := runName(argv); got != want {
			t.Errorf("%q is named %q, want %q", argv, got, want)
		}
	}
	// Two long commands that differ only near the end are two commands, each named the same way
	// every time, and neither name is longer than a hundred characters.
	long := `C:\Users\Jk101\AppData\Local\Temp\fs-learn-abcdefgh\a-folder-with-a-long-name\and-another-one-inside-it\burn.py`
	short, tall := runName([]string{"python.exe", long, "2", "12"}), runName([]string{"python.exe", long, "2", "3"})
	if short == tall || len(short) > 100 || len(tall) > 100 || short != runName([]string{"python.exe", long, "2", "12"}) || !strings.HasPrefix(short, `python c:\users\jk101\appdata\local\temp\`) {
		t.Errorf("two long commands are named %q and %q", short, tall)
	}
}

func TestWhatTheOldMeterMeasuredIsKept(t *testing.T) {
	t.Setenv("FAIRSHARE_HOME", t.TempDir())
	s := defaultSettings()
	day := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	b, _ := json.Marshal(map[string]any{"owl-go": map[string]any{day: map[string]float64{"minutes": 400, "cores": 1800, "mem_gb": 2400}}})
	os.WriteFile(filepath.Join(homeDir(), "repos.json"), b, 0o644)
	l := loadLearned()
	got := size(Need{Cores: 1, MemGB: 2}, l.levels("thread", "owl-go", "", nil, time.Now(), s), s.LearnTrustMinutes)
	if got.Cores < 4.3 || got.Cores > 4.5 || got.MemGB < 5.8 || got.From != "owl-go threads here" {
		t.Fatalf("the old per-repo average was not carried over: %+v", got)
	}
	// Once something was learned the old file is no longer read.
	if err := saveLearned(learned{}); err != nil {
		t.Fatal(err)
	}
	if l := loadLearned(); len(l) != 0 {
		t.Fatalf("the old file was read again: %v", l)
	}
}
