package main

import (
	"errors"
	"slices"
	"strings"
	"testing"
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
		got, why := mayWake(c.left, c.known, c.keep)
		if got != c.want || !strings.Contains(why, c.wantInWords) {
			t.Errorf("%s: %v %q", c.name, got, why)
		}
	}
	for text, want := range map[string]float64{"41.5\n": 41.5, ` {"left": 12} `: 12, "boat: note\n{\"left\": 0}": 0} {
		if got, ok := readHours([]byte(text)); !ok || got != want {
			t.Errorf("hours %q read as %v %v", text, got, ok)
		}
	}
	for _, text := range []string{"", "plenty", `{"used": 3}`, `{"left": "many"}`} {
		if _, ok := readHours([]byte(text)); ok {
			t.Errorf("hours %q were read as a number", text)
		}
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
	got := strings.Join(w.roomArgs(member{Name: "pc"}, true), " ")
	if got != "room --json --count 2 --checkout --repo appforgutters --urgent --cores 4 --mem-gb 6.5 --threads 7 --label fix uploads --take" {
		t.Errorf("the PC is asked %q", got)
	}
	got = strings.Join(work{}.roomArgs(member{Name: "host"}, false), " ")
	if got != "room --json --count 1" {
		t.Errorf("a bare piece of work asks %q", got)
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
