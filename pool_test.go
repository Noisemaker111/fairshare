package main

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestTheMachineWithTheMostRoomTakesIt(t *testing.T) {
	cases := []struct {
		name    string
		answers []reply
		want    int
	}{
		{"the most processors left, not the first", []reply{{Name: "pc", State: "room", Free: 2.1}, {Name: "host", State: "room", Free: 6.5}}, 1},
		{"an idle PC beats an idle host: 18.8 left against 7", []reply{{Name: "pc", State: "room", Free: 18.8}, {Name: "host", State: "room", Free: 7}}, 0},
		{"a tie goes to the earlier member", []reply{{Name: "a", State: "room", Free: 4}, {Name: "b", State: "room", Free: 4}}, 0},
		{"a full machine is never picked, whatever it has left", []reply{{Name: "pc", State: "full", Free: 9}, {Name: "host", State: "room", Free: 0.2}}, 1},
		{"a sleeping machine is not room", []reply{{Name: "pc", State: "full"}, {Name: "host", State: "asleep", Free: 8}}, -1},
		{"nor one that is closed, silent or cannot tell", []reply{{State: "closed", Free: 8}, {State: "unreachable", Free: 8}, {State: "degraded", Free: 8}}, -1},
		{"nobody", nil, -1},
	}
	for _, c := range cases {
		if got := best(c.answers); got != c.want {
			t.Errorf("%s: picked %d, want %d", c.name, got, c.want)
		}
	}
}

func TestAMachineIsOpenOnlyToWorkItCanDo(t *testing.T) {
	pc := member{Name: "pc", OS: "windows", Can: []string{"gpu", "deploy"}, Repos: map[string]string{"owl-go": `C:\Users\Jk101\owl-go`}}
	host := member{Name: "t3-host-2", OS: "linux", Via: []string{"boat", "ssh", "bx_1"}, Work: "/home/user/work"}
	bare := member{Name: "vps", OS: "linux", Via: []string{"ssh", "vps"}}
	cases := []struct {
		name string
		m    member
		w    work
		want bool
	}{
		{"work that needs nothing goes anywhere", host, work{}, true},
		{"Windows work stays off a Linux host", host, work{Needs: []string{"windows"}}, false},
		{"and goes to the PC", pc, work{Needs: []string{"windows"}}, true},
		{"a deploy needs a machine that can deploy", host, work{Needs: []string{"linux", "deploy"}}, false},
		{"every need has to be met", pc, work{Needs: []string{"windows", "gpu", "deploy"}}, true},
		{"a machine not among those named is closed", pc, work{Among: []string{"t3-host-2"}}, false},
		{"one that is named is open", host, work{Among: []string{"pc", "t3-host-2"}}, true},
		{"a run in a repo needs the repo's folder on record", bare, work{Repo: "owl-go", Folder: true}, false},
		{"a thread in a repo does not: its launch makes its own checkout", bare, work{Repo: "owl-go"}, true},
		{"the folder may be named for that repo alone", pc, work{Repo: "owl-go", Folder: true}, true},
		{"or come from the folder of repos", host, work{Repo: "owl-go", Folder: true}, true},
		{"the PC has no folder for a repo it was not told about", pc, work{Repo: "appforgutters", Folder: true}, false},
	}
	for _, c := range cases {
		if got, why := open(c.m, c.w); got != c.want || (!got && why == "") {
			t.Errorf("%s: open=%v (%q), want %v", c.name, got, why, c.want)
		}
	}
	if got := host.folder("owl-go"); got != "/home/user/work/owl-go" {
		t.Errorf("a Linux host's folder: %q", got)
	}
	if got := (member{OS: "windows", Work: `D:\work\`}).folder("owl-go"); got != `D:\work\owl-go` {
		t.Errorf("a Windows machine's folder: %q", got)
	}
}

func TestPlanHoursDecideAWake(t *testing.T) {
	cases := []struct {
		name        string
		left        float64
		known       bool
		keep        float64
		want        bool
		wantInWords string
	}{
		{"hours left over what is kept: woken", 41.5, true, 10, true, "41.5 plan-hours left"},
		{"exactly what is kept: not woken", 10, true, 10, false, "10.0 are kept"},
		{"under it: not woken", 3.2, true, 10, false, "3.2 plan-hours are left"},
		{"none kept and some left: woken", 0.5, true, 0, true, "0.5"},
		{"none left: not woken", 0, true, 0, false, "stays asleep"},
		{"hours that could not be read never spend any", 0, false, 0, false, "could not be read"},
		{"even when the number would have been fine", 99, false, 10, false, "could not be read"},
	}
	for _, c := range cases {
		got, why := mayWake(planHours{Left: c.left, Keep: c.keep, Known: c.known})
		if got != c.want || !strings.Contains(why, c.wantInWords) {
			t.Errorf("%s: %v %q", c.name, got, why)
		}
	}
	for text, want := range map[string]float64{"41.5\n": 41.5, ` {"left": 12} `: 12, "boat: note\n{\"left\": 0}": 0} {
		if got := readHours([]byte(text)); !got.Known || got.Left != want || got.Keep != 0 {
			t.Errorf("hours %q read as %+v", text, got)
		}
	}
	for _, text := range []string{"", "plenty", `{"used": 3}`, `{"left": "many"}`} {
		if got := readHours([]byte(text)); got.Known {
			t.Errorf("hours %q were read as a number", text)
		}
	}
	// How many are kept can be the command's to say: a plan that has to last to its reset keeps
	// more on a day that used much, and says why. 310 left is plenty, and still no wake.
	h := readHours([]byte(`{"left": 309.9, "keep": 309.9, "why": "83 used in the last 24 hours, and 12 a day lasts to the reset"}`))
	if ok, why := mayWake(h); ok || !strings.Contains(why, "309.9 plan-hours are left and 309.9 are kept (83 used in the last 24 hours, and 12 a day lasts to the reset), so it stays asleep") {
		t.Errorf("a plan over its daily rate: %v %q", ok, why)
	}
	if ok, _ := mayWake(readHours([]byte(`{"left": 309.9, "keep": 2, "why": "5 used in the last 24 hours"}`))); !ok {
		t.Errorf("a plan under its daily rate was not woken")
	}
}

// "That machine" is said by what its extra holds, never by its name; and a machine launches
// already wait in the line for is shut to the launch that arrives now.
func TestWorkAsksForAMachineByWhatItHoldsNotItsName(t *testing.T) {
	pcEnv, hostEnv := "3be418ad", "a1488a4f"
	pc := member{Name: "jons-pc", OS: "windows", Extra: map[string]string{"env": pcEnv}}
	host := member{Name: "t3-host", OS: "linux", Via: []string{"boat"}, Extra: map[string]string{"env": hostEnv, "project.appforgutters": "mcp:049a"}}
	bare := member{Name: "blade", OS: "linux", Via: []string{"mesh", "exec", "blade"}}
	cases := []struct {
		name string
		m    member
		w    work
		want bool
	}{
		{"the machine whose extra holds it", host, work{Where: map[string]string{"env": hostEnv}}, true},
		{"another machine", pc, work{Where: map[string]string{"env": hostEnv}}, false},
		{"a machine that holds nothing", bare, work{Where: map[string]string{"env": hostEnv}}, false},
		{"work that asks for none goes anywhere", bare, work{}, true},
		{"this machine is `pc` in the line whatever it is called in the pool", pc, work{Shut: []string{"pc"}}, false},
		{"and is not shut by its pool name", pc, work{Shut: []string{"jons-pc"}}, true},
		{"a host is its own name in the line", host, work{Shut: []string{"t3-host"}}, false},
	}
	for _, c := range cases {
		if got, why := open(c.m, c.w); got != c.want {
			t.Errorf("%s: %v (%s)", c.name, got, why)
		}
	}
	p := poolFile{Members: []member{pc, host, bare}}
	if m, ok := p.byLine("pc"); !ok || m.Name != "jons-pc" {
		t.Errorf("the line's pc is %+v", m)
	}
	if m, ok := p.byLine("t3-host"); !ok || m.Name != "t3-host" {
		t.Errorf("the line's t3-host is %+v", m)
	}
	if _, ok := p.byLine("gone"); ok {
		t.Errorf("a machine that left the pool was found")
	}
}

// A launch that found no room waits for the machines open to it: not one that said itself it
// cannot run it, not one that never answers, and still one it could not be asked about because
// launches already wait for it.
func TestALaunchWaitsForTheMachinesOpenToIt(t *testing.T) {
	p := poolFile{Members: []member{
		{Name: "jons-pc", OS: "windows"},
		{Name: "full", OS: "linux", Via: []string{"x"}},
		{Name: "no-folder", OS: "linux", Via: []string{"x"}},
		{Name: "asleep", OS: "linux", Via: []string{"x"}, Wake: []string{"wake"}},
		{Name: "silent", OS: "linux", Via: []string{"x"}},
		{Name: "waited-for", OS: "linux", Via: []string{"x"}},
	}}
	answers := []reply{{State: "full"}, {State: "full"}, {State: "closed"}, {State: "asleep"}, {State: "unreachable"}, {State: "closed"}}
	w := work{Shut: []string{"waited-for"}}
	if got := lineMachines(p, w, answers); !slices.Equal(got, []string{"pc", "full", "asleep", "waited-for"}) {
		t.Errorf("it waits for %v", got)
	}
	// Work that needs Windows waits for the PC alone.
	w.Needs = []string{"windows"}
	if got := lineMachines(p, w, answers); !slices.Equal(got, []string{"pc"}) {
		t.Errorf("Windows work waits for %v", got)
	}
	// And the machines launches still wait for are the ones not yet offered.
	q := []queued{{Machines: []string{"pc", "full"}}, {State: "offered", Machine: "asleep", Machines: []string{"asleep"}}, {}}
	if got := waitedFor(q); !slices.Equal(got, []string{"pc", "full"}) {
		t.Errorf("launches wait for %v", got)
	}
}

// What `mesh status --json` prints, from Hona/mesh SPEC 8.1.
const meshStatus = `{ "self": "adais",
  "devices": [
    { "name": "adais", "self": true, "os": "windows", "arch": "x64", "shell": "pwsh", "user": "lukem", "online": true },
    { "name": "blade", "self": false, "os": "windows", "arch": "x64", "shell": "powershell", "user": "lukem", "online": true, "reachable": true, "latency_ms": 38,
      "opencode": { "version": "2.0.32", "busy": 1, "service": "ok" }, "daemon": "up", "mesh": "0.1.0" },
    { "name": "air", "self": false, "os": "macos", "online": false } ] }`

func TestThePoolFollowsTheMesh(t *testing.T) {
	devices, err := parseMesh([]byte(meshStatus))
	if err != nil || len(devices) != 3 || !devices[0].Self || devices[1].Name != "blade" || devices[2].Online {
		t.Fatalf("mesh status read as %+v, %v", devices, err)
	}
	p := poolFile{Mesh: []string{"mesh"}}
	p, changed := followMesh(p, devices)
	if !changed || len(p.Members) != 3 {
		t.Fatalf("three devices made %+v", p.Members)
	}
	me, blade, air := p.Members[0], p.Members[1], p.Members[2]
	if len(me.Via) != 0 || me.From != "mesh" {
		t.Errorf("this device should be this machine: %+v", me)
	}
	if !slices.Equal(blade.Via, []string{"mesh", "exec", "--no-stall", "blade"}) || blade.OS != "windows" || blade.offline {
		t.Errorf("blade is reached through mesh exec: %+v", blade)
	}
	if !air.offline {
		t.Errorf("a device the mesh calls offline is not asked: %+v", air)
	}
	// What was set by hand on a mesh member stays, and a second look changes nothing.
	p.Members[1].Fairshare, p.Members[1].Can = `C:\fairshare\fairshare.exe`, []string{"gpu"}
	again, changed := followMesh(p, devices)
	if changed || again.Members[1].Fairshare == "" || !slices.Equal(again.Members[1].Can, []string{"gpu"}) {
		t.Errorf("a second look changed the pool: %v %+v", changed, again.Members[1])
	}
	// A device that left the mesh leaves the pool; one that joined it joins.
	gone, changed := followMesh(p, append(devices[:2:2], meshDevice{Name: "vps", OS: "linux", Online: true}))
	names := func(p poolFile) (out []string) {
		for _, m := range p.Members {
			out = append(out, m.Name)
		}
		return
	}
	if !changed || !slices.Equal(names(gone), []string{"adais", "blade", "vps"}) {
		t.Errorf("after air left and vps joined: %v", names(gone))
	}
	// A machine that joined by itself is not replaced, and this machine is not counted twice.
	own := poolFile{Mesh: []string{"mesh"}, Members: []member{{Name: "pc", Can: []string{"gpu"}}, {Name: "t3-host-2", Via: []string{"boat", "ssh", "bx_1"}}}}
	own, _ = followMesh(own, devices)
	if !slices.Equal(names(own), []string{"pc", "t3-host-2", "blade", "air"}) || own.Members[0].From != "" {
		t.Errorf("a pool with its own machines, following the mesh: %v", names(own))
	}
	// What mesh 0.1.0-dev.30 really prints on a machine that is in no mesh, and an empty mesh:
	// neither is "every device left".
	for _, text := range []string{"mesh: not joined", `{"self": "adais", "devices": []}`,
		`{ "error": "this device isn't in a mesh (/home/user/.local/share/opencode-mesh/state.json missing): run ` + "`mesh up`" + `" }`} {
		if _, err := parseMesh([]byte(text)); err == nil {
			t.Errorf("%q was read as a mesh's devices", text)
		}
	}
}

func TestAnErrandIsOneWordEveryCarrierPasses(t *testing.T) {
	e := errand{Argv: []string{"bash", "-c", `bun install && echo "it's $HOME" > 'out file'`}, Dir: `C:\Users\Jk101\owl go`, Commit: "abc123", Login: true}
	word := e.word()
	if strings.Trim(word, "0123456789abcdef") != "" {
		t.Fatalf("the word holds more than hex: %q", word)
	}
	back, err := readErrand(word)
	if err != nil || !slices.Equal(back.Argv, e.Argv) || back.Dir != e.Dir || back.Commit != e.Commit || !back.Login {
		t.Fatalf("the errand came back as %+v, %v", back, err)
	}
	if _, err := readErrand("not hex"); err == nil {
		t.Errorf("a word that is not hex was read")
	}
	if _, err := readErrand("6e6f"); err == nil {
		t.Errorf("hex that is not an errand was read")
	}
}

func TestWhatAMachinePrintedIsRead(t *testing.T) {
	host := member{Name: "t3-host-2", Via: []string{"boat", "ssh", "bx_1"}, Wake: []string{"boat", "resume", "bx_1"}}
	vps := member{Name: "vps", Via: []string{"ssh", "vps"}}
	room := `{"ok": true, "reason": "has room", "machine": "box", "cpu": {"ok": true, "used": 1.4, "pending": 1, "need": 1, "limit": 8}}`
	full := `{"ok": false, "reason": "box has no room", "cpu": {"ok": false, "used": 7.5, "pending": 0, "need": 1, "limit": 8}}`
	cases := []struct {
		name   string
		m      member
		out    string
		stderr string
		err    error
		state  string
		free   float64
	}{
		{"room, with what is left after the work", host, room, "", nil, "room", 4.6},
		{"a line the carrier printed first does not hide the answer", host, "Warning: Permanently added 'bx_1' to known hosts.\n" + room, "", nil, "room", 4.6},
		{"nor does a line it printed after", host, room + "\nConnection to bx_1 closed.\n", "", nil, "room", 4.6},
		{"full, and still how far over", host, full, "", errors.New("exit status 1"), "full", -0.5},
		{"a machine that could not tell", host, `{"ok": null, "degraded": "the meter has written no reading"}`, "", errors.New("exit status 2"), "degraded", 0},
		{"a machine without the work's folder", host, `{"ok": null, "closed": "no folder /home/user/work/owl-go on box"}`, "", errors.New("exit status 3"), "closed", 0},
		{"silence from a machine that can be woken is sleep", host, "", "ssh: connect to host: no route", errors.New("exit status 255"), "asleep", 0},
		{"silence from one that cannot is out of reach", vps, "", "", errors.New("exit status 255"), "unreachable", 0},
		{"an old fairshare that knows no pool is not an answer", vps, "", `fairshare: no command "pool"`, errors.New("exit status 2"), "unreachable", 0},
		{"JSON that is no answer is not one either", vps, `{"hello": 1}`, "", nil, "unreachable", 0},
	}
	for _, c := range cases {
		a := read(c.m, []byte(c.out), []byte(c.stderr), c.err)
		if a.State != c.state || a.Free != c.free || a.Name != c.m.Name {
			t.Errorf("%s: %+v", c.name, a)
		}
		if a.State != "room" && a.Why == "" {
			t.Errorf("%s: no reason given", c.name)
		}
	}
}

func TestThePoolsExitSaysFullOrCannotTell(t *testing.T) {
	cases := []struct {
		name string
		pl   placement
		want int
	}{
		{"placed", placement{OK: true}, 0},
		{"every machine full", placement{Machines: []reply{{State: "full"}, {State: "full"}}}, 1},
		{"full, and a host whose plan-hours are spent", placement{Machines: []reply{{State: "full"}, {State: "asleep"}}}, 1},
		{"full, and one out of reach", placement{Machines: []reply{{State: "full"}, {State: "unreachable"}}}, 1},
		{"no machine can do the work", placement{Machines: []reply{{State: "closed"}, {State: "closed"}}}, 1},
		{"a machine that could not tell might have had room", placement{Machines: []reply{{State: "full"}, {State: "degraded"}}}, 2},
		{"nobody answered", placement{Machines: []reply{{State: "unreachable"}, {State: "unreachable"}}}, 2},
	}
	for _, c := range cases {
		if got := exitOf(c.pl); got != c.want {
			t.Errorf("%s: exit %d, want %d", c.name, got, c.want)
		}
	}
}

func TestThePlacementSaysEachMachinesNumber(t *testing.T) {
	answers := []reply{
		{Name: "pc", State: "full", Used: 8.9, Limit: 9},
		{Name: "t3-host-2", State: "room", Free: 5.5},
		{Name: "t3-host-1", State: "asleep", Why: "did not answer: no route"},
		{Name: "vps", State: "closed", Why: "cannot do deploy"},
	}
	got := placeWords(placement{}, answers, 1)
	for _, want := range []string{"t3-host-2 takes it: 5.5 processors are free there after it", "pc is full (8.9 of 9.0 in use)", "t3-host-1 is asleep and stays asleep", "vps is not open to it (cannot do deploy)"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q does not say %q", got, want)
		}
	}
	answers[1].State, answers[1].Used, answers[1].Limit = "full", 7.9, 8
	answers[2].Why = "2.0 plan-hours are left and 10.0 are kept, so it stays asleep"
	got = placeWords(placement{}, answers, -1)
	for _, want := range []string{"No machine in the pool has room", "t3-host-2 is full (7.9 of 8.0 in use)", "2.0 plan-hours are left and 10.0 are kept"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q does not say %q", got, want)
		}
	}
	if got := placeWords(placement{Woke: true}, answers, 2); !strings.Contains(got, "t3-host-1 was woken and takes it") {
		t.Errorf("a wake reads %q", got)
	}
}

func TestTheQuestionIsTheOneALaunchAsks(t *testing.T) {
	w := work{Label: "fix uploads", Repo: "appforgutters", Count: 2, Checkout: true, Urgent: true, Explicit: Need{Cores: 4, MemGB: 6.5}, Threads: map[string]int{"pc": 7}}
	got := strings.Join(w.roomArgs(member{Name: "pc"}, true, nil), " ")
	if got != "room --json --count 2 --checkout --repo appforgutters --urgent --cores 4 --mem-gb 6.5 --threads 7 --label fix uploads --take" {
		t.Errorf("the PC is asked %q", got)
	}
	got = strings.Join(work{}.roomArgs(member{Name: "host"}, false, nil), " ")
	if got != "room --json --count 1" {
		t.Errorf("a bare piece of work asks %q", got)
	}
	// A run says what it is, and carries what another machine measured for it.
	gate := work{Repo: "appforgutters", Kind: "run", What: "bun run gate"}
	got = strings.Join(gate.roomArgs(member{Name: "host"}, false, &Sizing{Cores: 5.2, Swing: 0.8, MemGB: 6, Minutes: 90, From: "`bun run gate` in appforgutters on pc"}), " ")
	if got != "room --json --count 1 --kind run --what bun run gate --seen "+`{"cores":5.2,"swing":0.8,"mem_gb":6,"minutes":90,"from":"`+"`bun run gate` in appforgutters on pc"+`"}`+" --repo appforgutters" {
		t.Errorf("a run asks %q", got)
	}
	for text, want := range map[string][]string{
		"boat ssh bx_1": {"boat", "ssh", "bx_1"},
		`["C:\\Program Files\\Git\\bin\\bash.exe", "boat-host.sh", "up"]`: {`C:\Program Files\Git\bin\bash.exe`, "boat-host.sh", "up"},
		"": nil,
	} {
		if got, err := argvOf(text); err != nil || !slices.Equal(got, want) {
			t.Errorf("the command %q read as %q, %v", text, got, err)
		}
	}
	if _, err := argvOf(`["unclosed`); err == nil {
		t.Errorf("a broken list was read as a command")
	}
}

func TestABareBashOnWindowsIsGitBash(t *testing.T) {
	git := `C:\Program Files\Git\bin\bash.exe`
	has := func(path string) bool { return path == git }
	none := func(string) bool { return false }
	cases := []struct {
		name, program, files string
		exists               func(string) bool
		want                 string
	}{
		{"bash is Git Bash when Git is installed", "bash", `C:\Program Files`, has, git},
		{"so is bash.exe, in any case", "Bash.EXE", `C:\Program Files`, has, git},
		{"with no ProgramFiles in the environment the usual folder is tried", "bash", "", has, git},
		{"without Git it stays what was asked for", "bash", `C:\Program Files`, none, "bash"},
		{"a full path is left alone", `D:\msys\bash.exe`, `C:\Program Files`, has, `D:\msys\bash.exe`},
		{"any other program is left alone", "bun", `C:\Program Files`, has, "bun"},
	}
	for _, c := range cases {
		if got := gitBash(c.program, c.files, c.exists); got != c.want {
			t.Errorf("%s: %q", c.name, got)
		}
	}
}

func TestThePoolPassesOnWhatTheMachineThatMeasuredMostSays(t *testing.T) {
	now := time.Date(2026, 11, 11, 18, 0, 0, 0, time.UTC)
	gate := work{Repo: "AppForGutters", Kind: "run", What: "bun run gate"}
	says := func(name string, minutes, cores float64) reply {
		a := reply{Name: name, State: "room", verdict: &Verdict{}}
		if minutes > 0 {
			a.verdict.Measured = &Sizing{Cores: cores, Minutes: minutes, From: "`bun run gate` in appforgutters on " + name}
		}
		return a
	}
	n := news{}
	if n.tell(gate, "t3-host-2") != nil {
		t.Fatal("news before any machine measured it")
	}
	changed := n.hear(gate, []reply{says("pc", 90, 12), says("t3-host-1", 20, 6.5), says("t3-host-2", 0, 0), {Name: "asleep", State: "asleep"}}, now)
	if !changed || n[gate.newsKey()].Machine != "pc" {
		t.Fatalf("the machine with the most minutes is the one to pass on: %+v", n)
	}
	// A machine is told what another measured, never its own.
	if got := n.tell(gate, "t3-host-2"); got == nil || got.Cores != 12 || got.From != "`bun run gate` in appforgutters on pc" {
		t.Fatalf("the new host is told %+v", got)
	}
	if n.tell(gate, "pc") != nil {
		t.Fatal("the PC was told its own measurement")
	}
	// The command is the work: another command, another repo, and a thread of this repo are other news.
	for _, other := range []work{{Repo: "appforgutters", Kind: "run", What: "bun run lint"}, {Repo: "owl-go", Kind: "run", What: "bun run gate"}, {Repo: "appforgutters"}} {
		if n.tell(other, "t3-host-2") != nil {
			t.Errorf("%+v was told the gate's measurement", other)
		}
	}
	// The PC's own newer word replaces its older; a machine with fewer minutes does not replace it
	// until the PC's word is a day old.
	n.hear(gate, []reply{says("pc", 95, 11), says("t3-host-1", 30, 6.5)}, now.Add(time.Hour))
	if got := n.tell(gate, "t3-host-2"); got.Cores != 11 {
		t.Fatalf("after the PC measured again: %+v", got)
	}
	n.hear(gate, []reply{says("t3-host-1", 30, 6.5)}, now.Add(30*time.Hour))
	if got := n.tell(gate, "t3-host-2"); got.Cores != 6.5 {
		t.Fatalf("with the PC silent for a day the host that still measures it is heard: %+v", got)
	}
	// Work in no repo is nobody's news.
	if n.hear(work{Kind: "run", What: "ls"}, []reply{says("pc", 5, 1)}, now) || (work{}).newsKey() != "" {
		t.Fatal("work without a repo was kept as news")
	}
}
