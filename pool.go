package main

// The pool. Every machine that joined is one place to run work: a piece of work (a T3 thread, a
// gate, a build) says what it is, never which machine, and the pool puts it where there is room.
//
// Each machine answers for itself. The pool asks every member the question a launch already asks
// one machine, `fairshare room`, with that machine's own measurements, settings, line and reserve,
// and only compares the answers. So the processors kept for the person at the PC are out of the
// pool before the pool sees them, and nothing here knows how a machine decides.
//
// A member is reached through its `via`: the words that run a command on that machine, put in
// front of the command. `boat ssh <id>`, `ssh <host>` and `mesh exec <device>` are all a via, so
// what carries the command (boat, plain SSH, Hona/mesh over Tailscale) is one field of a member
// and the pool is the same over each. Everything sent is one word of hex, because every one of
// those carriers splits or quotes a command differently.
//
// A machine that costs plan-hours to keep awake (a boat host) is woken only when no machine that
// is already awake has room, and never once its plan-hours are down to what the member keeps.
//
// Why: Jon, 2026-11-11: "it goes away from this server, that server; it's essentially one place".
// Until then a launch was placed by a hook that knew each host by its folder, a gate went to the
// first host or stayed where it started, and a build ran wherever its thread happened to be.

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// cannotRun is the exit of `pool run` and `pool here` when the pool, not the command, failed.
const cannotRun = 69

// member is one machine in the pool.
type member struct {
	Name string `json:"name"`
	// Via runs a command on the machine when put in front of it; none means this machine.
	Via []string `json:"via,omitempty"`
	// Fairshare is the fairshare program there; unset, `fairshare` on its PATH.
	Fairshare string `json:"fairshare,omitempty"`
	// Wake is run here when the machine does not answer; without it a silent machine is skipped.
	Wake []string `json:"wake,omitempty"`
	// Awake is run here before anything is sent: the machine is up when it exits 0 and, with
	// AwakeMatch, prints something that pattern finds. For a carrier that would wake a sleeping
	// machine just by being used, which would spend plan-hours on every question.
	Awake      []string `json:"awake,omitempty"`
	AwakeMatch string   `json:"awake_match,omitempty"`
	// Hours is run here before a wake and prints the plan-hours left (a number, or {"left": n}).
	// The machine is woken only while more than KeepHours are left.
	Hours     []string `json:"hours,omitempty"`
	KeepHours float64  `json:"keep_hours,omitempty"`
	// Can is what the machine can do beyond its OS ("gpu", "deploy"); work that needs one of them
	// goes only to a machine that can.
	Can []string `json:"can,omitempty"`
	// Work is the folder that holds its repos, one folder each by name; Repos names a repo's
	// folder where it is somewhere else.
	Work  string            `json:"work,omitempty"`
	Repos map[string]string `json:"repos,omitempty"`
	// BusyDir is a folder the machine's own idle watch reads: a run leaves its process id there
	// while it runs, so the machine is not put to sleep under it.
	BusyDir string `json:"busy_dir,omitempty"`
	// Extra is handed back with a placement, for the caller's own use (a T3 environment id).
	Extra map[string]string `json:"extra,omitempty"`
	// OS and Processors are what it measured when it joined, shown while it is asleep.
	OS         string `json:"os,omitempty"`
	Processors int    `json:"processors,omitempty"`
	// From is "mesh" for a member that is a device of the mesh; those follow the mesh.
	From    string `json:"from,omitempty"`
	offline bool
}

// poolFile is pool.json in fairshare's home, on the machine that places work.
type poolFile struct {
	Members []member `json:"members"`
	// AskSeconds is how long a machine has to answer; WakeSeconds how long a wake may take.
	AskSeconds  float64 `json:"ask_seconds,omitempty"`
	WakeSeconds float64 `json:"wake_seconds,omitempty"`
	// Mesh is the mesh program; set, every device of the mesh is a member.
	Mesh []string `json:"mesh,omitempty"`
}

func poolPath() string { return filepath.Join(homeDir(), "pool.json") }

func (p poolFile) askTime() time.Duration {
	if p.AskSeconds > 0 {
		return time.Duration(p.AskSeconds * float64(time.Second))
	}
	return 20 * time.Second
}

func (p poolFile) wakeTime() time.Duration {
	if p.WakeSeconds > 0 {
		return time.Duration(p.WakeSeconds * float64(time.Second))
	}
	return 280 * time.Second
}

func readPool() (poolFile, error) {
	var p poolFile
	b, err := os.ReadFile(poolPath())
	if os.IsNotExist(err) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return p, fmt.Errorf("%s does not parse: %w", poolPath(), err)
	}
	return p, nil
}

func savePool(p poolFile) error {
	if err := os.MkdirAll(homeDir(), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	tmp := poolPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, poolPath())
}

// loadPool is the pool as it stands now: the file, brought up to the mesh when it follows one.
func loadPool() (poolFile, error) {
	p, err := readPool()
	if err != nil {
		return p, err
	}
	if len(p.Mesh) > 0 {
		devices, err := meshDevices(p.Mesh, p.askTime())
		if err != nil {
			fmt.Fprintln(os.Stderr, "degraded: the mesh did not say which devices it has, so the pool is the machines it had:", err)
		} else if after, changed := followMesh(p, devices); changed {
			p = after
			if err := savePool(stripOffline(p)); err != nil {
				fmt.Fprintln(os.Stderr, "degraded: the pool was not saved:", err)
			}
		} else {
			p = after
		}
	}
	if len(p.Members) == 0 {
		return p, fmt.Errorf("no machine has joined the pool (%s); run: fairshare pool join", poolPath())
	}
	return p, nil
}

func stripOffline(p poolFile) poolFile {
	p.Members = slices.Clone(p.Members)
	for i := range p.Members {
		p.Members[i].offline = false
	}
	return p
}

// meshDevice is one device as `mesh status --json --fast` names it (Hona/mesh SPEC 8.1).
type meshDevice struct {
	Name   string `json:"name"`
	Self   bool   `json:"self"`
	OS     string `json:"os"`
	Online bool   `json:"online"`
}

func meshDevices(prog []string, limit time.Duration) ([]meshDevice, error) {
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	cmd := exec.CommandContext(ctx, prog[0], append(slices.Clone(prog[1:]), "status", "--json", "--fast")...)
	hideWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s status: %v", prog[0], err)
	}
	return parseMesh(out)
}

func parseMesh(out []byte) ([]meshDevice, error) {
	var got struct {
		Self    string       `json:"self"`
		Devices []meshDevice `json:"devices"`
		Error   string       `json:"error"`
	}
	if err := json.Unmarshal(jsonPart(out), &got); err != nil {
		return nil, fmt.Errorf("mesh status does not parse: %w", err)
	}
	// A device outside a mesh gets {"error": "..."} (seen with mesh 0.1.0-dev.30): no devices is
	// never read as "every device left".
	if got.Error != "" || len(got.Devices) == 0 {
		return nil, fmt.Errorf("mesh named no devices: %s", cmpOr(got.Error, "an empty list"))
	}
	for i := range got.Devices {
		if got.Devices[i].Name == got.Self {
			got.Devices[i].Self = true
		}
	}
	return got.Devices, nil
}

// followMesh makes the pool's mesh members the mesh's devices: a new device joins, a device that
// left the mesh leaves, and what was set on a member by hand (its fairshare, its folders, what it
// can do) stays. A machine that joined by itself keeps its entry; this machine is not added twice.
func followMesh(p poolFile, devices []meshDevice) (poolFile, bool) {
	changed := false
	inMesh := map[string]meshDevice{}
	for _, d := range devices {
		inMesh[d.Name] = d
	}
	hasLocal := slices.ContainsFunc(p.Members, func(m member) bool { return len(m.Via) == 0 && m.From != "mesh" })
	var kept []member
	for _, m := range p.Members {
		d, still := inMesh[m.Name]
		if m.From == "mesh" && (!still || (d.Self && hasLocal)) {
			changed = true
			continue
		}
		if m.From == "mesh" {
			via := meshVia(p.Mesh, d)
			if !slices.Equal(m.Via, via) || (d.OS != "" && m.OS != d.OS) {
				m.Via, changed = via, true
				if d.OS != "" {
					m.OS = d.OS
				}
			}
			m.offline = !d.Online && !d.Self
		}
		kept = append(kept, m)
		delete(inMesh, m.Name)
	}
	for _, d := range devices { // the mesh's order
		if _, isNew := inMesh[d.Name]; !isNew || (d.Self && hasLocal) {
			continue
		}
		kept = append(kept, member{Name: d.Name, Via: meshVia(p.Mesh, d), OS: d.OS, From: "mesh", offline: !d.Online && !d.Self})
		changed = true
	}
	p.Members = kept
	return p, changed
}

// meshVia is how a mesh device is reached. --no-stall: mesh ends a command that prints nothing
// and sits idle for 20 s, and a gate waiting on a lock does exactly that.
func meshVia(prog []string, d meshDevice) []string {
	if d.Self {
		return nil
	}
	return append(slices.Clone(prog), "exec", "--no-stall", d.Name)
}

// errand is what one machine is asked to do, sent as one word.
type errand struct {
	// Args is a fairshare command to run there, Argv a program. Dir is the folder the work needs:
	// a machine without it says so instead of answering. Commit runs Argv in a checkout of exactly
	// that commit. Login runs Argv in the user's login shell, for a machine reached from outside,
	// where the carrier's shell has none of the user's PATH.
	Args    []string `json:"args,omitempty"`
	Argv    []string `json:"argv,omitempty"`
	Dir     string   `json:"dir,omitempty"`
	Commit  string   `json:"commit,omitempty"`
	BusyDir string   `json:"busy_dir,omitempty"`
	Login   bool     `json:"login,omitempty"`
}

func (e errand) word() string {
	b, _ := json.Marshal(e)
	return hex.EncodeToString(b)
}

func readErrand(word string) (errand, error) {
	var e errand
	b, err := hex.DecodeString(word)
	if err != nil {
		return e, fmt.Errorf("the errand is not hex: %w", err)
	}
	if err := json.Unmarshal(b, &e); err != nil {
		return e, fmt.Errorf("the errand does not parse: %w", err)
	}
	return e, nil
}

// self is this program, for a member that is this machine.
func self() string {
	if exe, err := os.Executable(); err == nil {
		return exe
	}
	return os.Args[0]
}

// command is the command that carries an errand to the member.
func (m member) command(ctx context.Context, e errand) *exec.Cmd {
	argv := []string{self()}
	if len(m.Via) > 0 {
		e.Login = e.Login || len(e.Argv) > 0
		prog := m.Fairshare
		if prog == "" {
			prog = "fairshare"
		}
		argv = append(slices.Clone(m.Via), prog)
	}
	argv = append(argv, "pool", "here", e.word())
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	hideWindow(cmd)
	return cmd
}

// folder is where the member keeps a repo, or "" when nothing says.
func (m member) folder(repo string) string {
	if repo == "" {
		return ""
	}
	if dir := m.Repos[repo]; dir != "" {
		return dir
	}
	if m.Work == "" {
		return ""
	}
	sep := "/"
	if m.OS == "windows" {
		sep = `\`
	}
	return strings.TrimRight(m.Work, `/\`) + sep + repo
}

// work is a piece of work looking for a machine.
type work struct {
	Label    string
	Repo     string
	Count    int
	Checkout bool
	Urgent   bool
	Explicit Need
	// Threads is the agent threads open on each machine now, by member, for the callers that know.
	Threads map[string]int
	// Needs is what the machine must be or can do: an OS, or one of a member's Can.
	Needs []string
	// Among, when set, is the only machines it may go to.
	Among []string
	// Folder says the work runs in the repo's folder, so the machine must have it.
	Folder bool
}

// roomArgs is the question for one member: `fairshare room`, as a launch asks it.
func (w work) roomArgs(m member, take bool) []string {
	args := []string{"room", "--json", "--count", strconv.Itoa(max(w.Count, 1))}
	if w.Checkout {
		args = append(args, "--checkout")
	}
	if w.Repo != "" {
		args = append(args, "--repo", w.Repo)
	}
	if w.Urgent {
		args = append(args, "--urgent")
	}
	for i, v := range []float64{w.Explicit.Cores, w.Explicit.MemGB, w.Explicit.DiskGB} {
		if v > 0 {
			args = append(args, []string{"--cores", "--mem-gb", "--disk-gb"}[i], strconv.FormatFloat(v, 'f', -1, 64))
		}
	}
	if n := w.Threads[m.Name]; n > 0 {
		args = append(args, "--threads", strconv.Itoa(n))
	}
	if w.Label != "" {
		args = append(args, "--label", w.Label)
	}
	if take {
		args = append(args, "--take")
	}
	return args
}

// open says whether the work may go to the member at all, before anything is measured.
func open(m member, w work) (bool, string) {
	if len(w.Among) > 0 && !slices.Contains(w.Among, m.Name) {
		return false, "not one of the machines this work may use"
	}
	for _, need := range w.Needs {
		if need != m.OS && !slices.Contains(m.Can, need) {
			return false, "cannot do " + need
		}
	}
	if w.Folder && w.Repo != "" && m.folder(w.Repo) == "" {
		return false, "no folder on record for " + w.Repo
	}
	return true, ""
}

// reply is what one member said about a piece of work.
type reply struct {
	Name string `json:"name"`
	// State is "room", "full", "asleep" (silent, and it can be woken), "unreachable" (silent, and
	// it cannot), "closed" (this work may not go there) or "degraded" (it could not tell).
	State string `json:"state"`
	Why   string `json:"why,omitempty"`
	// Free is the processors left under its limit once the work is counted; Used and Limit are
	// the two sides of that.
	Free    float64 `json:"free"`
	Used    float64 `json:"used"`
	Limit   float64 `json:"limit"`
	Seconds float64 `json:"seconds"`
	verdict *Verdict
}

// jsonPart is the first JSON object in the output: a carrier may print lines of its own before
// the answer and after it.
func jsonPart(out []byte) []byte {
	i := strings.IndexByte(string(out), '{')
	if i < 0 {
		return out
	}
	var first json.RawMessage
	if json.NewDecoder(strings.NewReader(string(out[i:]))).Decode(&first) != nil {
		return out[i:]
	}
	return first
}

func tail(text string, n int) string {
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > n {
		return "..." + text[len(text)-n:]
	}
	return text
}

// up says whether the member's own check calls it awake; a member with no check is asked directly.
func up(p poolFile, m member) (bool, string) {
	if len(m.Awake) == 0 {
		return true, ""
	}
	out, err := command(m.Awake, nil, p.askTime())
	if err != nil {
		return false, "its awake check says no: " + tail(err.Error(), 160)
	}
	if m.AwakeMatch != "" {
		found, err := regexp.Match(m.AwakeMatch, out)
		if err != nil {
			return false, "awake_match is not a pattern: " + err.Error()
		}
		if !found {
			return false, "its awake check printed: " + tail(string(out), 160)
		}
	}
	return true, ""
}

// send carries an errand to a member and returns what it printed. A member its own check calls
// asleep is sent nothing.
func send(p poolFile, m member, e errand) ([]byte, []byte, error) {
	if ok, why := up(p, m); !ok {
		return nil, []byte(why), errors.New(why)
	}
	ctx, cancel := context.WithTimeout(context.Background(), p.askTime())
	defer cancel()
	cmd := m.command(ctx, e)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if ctx.Err() != nil {
		err = fmt.Errorf("no answer in %.0f s", p.askTime().Seconds())
	}
	return out, []byte(stderr.String()), err
}

// read turns what a member printed for `room` or `status` into its answer.
func read(m member, out, stderr []byte, err error) reply {
	a := reply{Name: m.Name}
	var got struct {
		Verdict
		OK       *bool  `json:"ok"`
		Degraded string `json:"degraded"`
		Closed   string `json:"closed"`
	}
	if json.Unmarshal(jsonPart(out), &got) != nil || (got.OK == nil && got.Degraded == "" && got.Closed == "") {
		why := tail(string(stderr), 200)
		if why == "" && err != nil {
			why = err.Error()
		}
		if why == "" {
			why = "it printed no answer"
		}
		a.State, a.Why = "unreachable", "did not answer: "+why
		if len(m.Wake) > 0 {
			a.State = "asleep"
		}
		return a
	}
	switch {
	case got.Closed != "":
		a.State, a.Why = "closed", got.Closed
	case got.OK == nil:
		a.State, a.Why = "degraded", got.Degraded
	default:
		v := got.Verdict
		v.OK = *got.OK
		a.verdict = &v
		a.Used, a.Limit = round2(v.CPU.Used+v.CPU.Pending), round2(v.CPU.Limit)
		a.Free = round2(v.CPU.Limit - v.CPU.Used - v.CPU.Pending - v.CPU.Need)
		a.State, a.Why = "full", v.Reason
		if v.OK {
			a.State = "room"
		}
	}
	return a
}

// askRoom asks one member whether the work fits, and with take counts it there when it does.
func askRoom(p poolFile, m member, w work, take bool) reply {
	if ok, why := open(m, w); !ok {
		return reply{Name: m.Name, State: "closed", Why: why}
	}
	if m.offline {
		return reply{Name: m.Name, State: "unreachable", Why: "offline in the mesh"}
	}
	e := errand{Args: w.roomArgs(m, take)}
	if w.Folder {
		e.Dir = m.folder(w.Repo)
	}
	start := time.Now()
	out, stderr, err := send(p, m, e)
	a := read(m, out, stderr, err)
	a.Seconds = round2(time.Since(start).Seconds())
	return a
}

// askAll asks every member at once.
func askAll(p poolFile, ask func(member) reply) []reply {
	answers := make([]reply, len(p.Members))
	var wg sync.WaitGroup
	for i, m := range p.Members {
		wg.Add(1)
		go func() {
			defer wg.Done()
			answers[i] = ask(m)
		}()
	}
	wg.Wait()
	return answers
}

// best is the machine with room that has the most processors left once the work is counted, the
// earlier member on a tie; -1 when none has room.
func best(answers []reply) int {
	at := -1
	for i, a := range answers {
		if a.State == "room" && (at < 0 || a.Free > answers[at].Free+1e-9) {
			at = i
		}
	}
	return at
}

// readHours reads what a member's hours command printed: a number, or {"left": n}.
func readHours(out []byte) (float64, bool) {
	text := strings.TrimSpace(string(out))
	if v, err := strconv.ParseFloat(text, 64); err == nil {
		return v, true
	}
	var got struct {
		Left *float64 `json:"left"`
	}
	if json.Unmarshal(jsonPart(out), &got) == nil && got.Left != nil {
		return *got.Left, true
	}
	return 0, false
}

// mayWake says whether a sleeping machine with plan-hours may be woken: only while more are left
// than it keeps, and never when they could not be read.
func mayWake(left float64, known bool, keep float64) (bool, string) {
	if !known {
		return false, "its plan-hours could not be read, so it stays asleep"
	}
	if left <= keep+1e-9 {
		return false, fmt.Sprintf("%.1f plan-hours are left and %.1f are kept, so it stays asleep", left, keep)
	}
	return true, fmt.Sprintf("%.1f plan-hours left", left)
}

// hoursLeft runs the member's hours command.
func hoursLeft(p poolFile, m member) (float64, bool) {
	out, err := command(m.Hours, nil, p.askTime())
	if err != nil {
		return 0, false
	}
	return readHours(out)
}

// placement is the pool's answer for a piece of work.
type placement struct {
	OK      bool   `json:"ok"`
	Machine string `json:"machine,omitempty"`
	Local   bool   `json:"local,omitempty"`
	// Asleep says the machine was picked asleep and the caller wakes it (--no-wake); Woke that the
	// pool woke it; Took that the work is counted there until its load shows.
	Asleep   bool              `json:"asleep,omitempty"`
	Woke     bool              `json:"woke,omitempty"`
	Took     bool              `json:"took,omitempty"`
	Reason   string            `json:"reason"`
	Extra    map[string]string `json:"extra,omitempty"`
	Machines []reply           `json:"machines"`
	Seconds  float64           `json:"seconds"`
	member   member
}

// place puts a piece of work on a machine: the awake machine with the most room, and only when
// none has any, a sleeping one that may be woken. wake is how a machine is woken (nil: the caller
// wakes it).
func place(p poolFile, w work, take bool, wake func(member) error) placement {
	start := time.Now()
	answers := askAll(p, func(m member) reply { return askRoom(p, m, w, false) })
	done := func(i int, pl placement) placement {
		pl.Machines, pl.Seconds = answers, round2(time.Since(start).Seconds())
		if i >= 0 {
			m := p.Members[i]
			pl.OK, pl.Machine, pl.Local, pl.Extra, pl.member = true, m.Name, len(m.Via) == 0, m.Extra, m
		}
		pl.Reason = placeWords(pl, answers, i)
		return pl
	}
	for {
		i := best(answers)
		if i < 0 {
			break
		}
		if !take {
			return done(i, placement{})
		}
		// The answer that counts is the one that takes it: another launch may have gone in since.
		if a := askRoom(p, p.Members[i], w, true); a.State == "room" {
			answers[i] = a
			return done(i, placement{Took: true})
		} else {
			answers[i] = a
		}
	}
	for i, m := range p.Members {
		if answers[i].State != "asleep" {
			continue
		}
		if len(m.Hours) > 0 {
			left, known := hoursLeft(p, m)
			ok, why := mayWake(left, known, m.KeepHours)
			if !ok {
				answers[i].Why = why
				continue
			}
			answers[i].Why = why
		}
		if wake == nil {
			return done(i, placement{Asleep: true})
		}
		if err := wake(m); err != nil {
			answers[i].State, answers[i].Why = "unreachable", "did not wake: "+err.Error()
			continue
		}
		a := askRoom(p, m, w, take)
		for try := 0; try < 3 && (a.State == "asleep" || a.State == "unreachable"); try++ {
			time.Sleep(5 * time.Second)
			a = askRoom(p, m, w, take)
		}
		if a.State == "asleep" {
			a.State, a.Why = "unreachable", "woken, and still "+a.Why
		}
		answers[i] = a
		if a.State == "room" {
			return done(i, placement{Woke: true, Took: take})
		}
	}
	return done(-1, placement{})
}

// wakeMember runs a member's wake command.
func wakeMember(p poolFile) func(member) error {
	return func(m member) error {
		_, err := command(m.Wake, nil, p.wakeTime())
		return err
	}
}

// placeWords says where the work went and why, or why nowhere, with each machine's number.
func placeWords(pl placement, answers []reply, picked int) string {
	var others []string
	for i, a := range answers {
		if i == picked {
			continue
		}
		switch a.State {
		case "room":
			others = append(others, fmt.Sprintf("%s would have %.1f", a.Name, a.Free))
		case "full":
			others = append(others, fmt.Sprintf("%s is full (%.1f of %.1f in use)", a.Name, a.Used, a.Limit))
		case "asleep":
			if strings.Contains(a.Why, "plan-hours") {
				others = append(others, fmt.Sprintf("%s is asleep (%s)", a.Name, a.Why))
			} else {
				others = append(others, a.Name+" is asleep and stays asleep")
			}
		case "closed":
			others = append(others, fmt.Sprintf("%s is not open to it (%s)", a.Name, a.Why))
		default:
			others = append(others, fmt.Sprintf("%s: %s: %s", a.Name, a.State, tail(a.Why, 160)))
		}
	}
	rest := ""
	if len(others) > 0 {
		rest = " The others: " + strings.Join(others, "; ") + "."
	}
	if picked < 0 {
		return "No machine in the pool has room for it." + rest
	}
	a := answers[picked]
	switch {
	case pl.Asleep:
		return fmt.Sprintf("%s takes it once it is awake: no machine that is awake has room.%s", a.Name, rest)
	case pl.Woke:
		return fmt.Sprintf("%s was woken and takes it (%.1f processors free there after it): no machine that was awake had room.%s", a.Name, a.Free, rest)
	}
	return fmt.Sprintf("%s takes it: %.1f processors are free there after it, the most in the pool.%s", a.Name, a.Free, rest)
}

// exitOf is the exit for a placement: 0 placed, 1 no machine takes it, 2 cannot tell (a machine
// that might have had room could not say, or nobody answered at all).
func exitOf(pl placement) int {
	if pl.OK {
		return 0
	}
	answered := false
	for _, a := range pl.Machines {
		if a.State == "degraded" {
			return 2
		}
		answered = answered || a.State != "unreachable"
	}
	if !answered {
		return 2
	}
	return 1
}

// list reads a comma separated flag.
func list(text string) []string {
	var out []string
	for _, part := range strings.Split(text, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// pairs reads "a=1,b=2".
func pairs(text string) map[string]string {
	out := map[string]string{}
	for _, part := range list(text) {
		if k, v, ok := strings.Cut(part, "="); ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// argvOf reads a command given as one flag: a JSON list, or words with spaces between them.
func argvOf(text string) ([]string, error) {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "[") {
		var argv []string
		if err := json.Unmarshal([]byte(text), &argv); err != nil {
			return nil, fmt.Errorf("%q is not a JSON list of words: %w", text, err)
		}
		return argv, nil
	}
	return strings.Fields(text), nil
}

// workFlags are the flags that say what the work is, shared by `pool place` and `pool run`.
func workFlags(fs *flag.FlagSet) func() work {
	label := fs.String("label", "", "what the work is, for the record")
	repo := fs.String("repo", "", "the repo it works in")
	count := fs.Int("count", 1, "new threads asked for at once")
	checkout := fs.Bool("checkout", false, "the work makes a checkout")
	urgent := fs.Bool("urgent", false, "judge it against the work that would not step aside")
	cores := fs.Float64("cores", 0, "count it as this many processors")
	memGB := fs.Float64("mem-gb", 0, "count it as this much memory")
	diskGB := fs.Float64("disk-gb", 0, "count it as this much disk")
	threads := fs.String("threads", "", "agent threads open on each machine now: pc=7,host=2")
	needs := fs.String("needs", "", "what the machine must be or can do: windows,gpu,deploy")
	among := fs.String("among", "", "the only machines it may go to")
	return func() work {
		w := work{Label: *label, Repo: *repo, Count: *count, Checkout: *checkout, Urgent: *urgent,
			Explicit: Need{*cores, *memGB, *diskGB}, Needs: list(*needs), Among: list(*among)}
		for name, n := range pairs(*threads) {
			if v, err := strconv.Atoi(n); err == nil {
				if w.Threads == nil {
					w.Threads = map[string]int{}
				}
				w.Threads[name] = v
			}
		}
		return w
	}
}

const poolUsage = `fairshare pool: every machine that joined, as one place to run work

  fairshare pool                     the machines, what each has free, and where a new thread fits
  fairshare pool join                this machine joins (--name, --can gpu,deploy, --work <folder of repos>)
  fairshare pool join --name N --via "<words that run a command on N>"
                                     another machine joins: --fairshare <program there> --wake "<cmd>"
                                     --awake "<cmd that says it is up>" --awake-match <pattern>
                                     --hours "<cmd printing plan-hours left>" --keep-hours N
                                     --work <folder> --repos name=folder,... --busy-dir <folder> --extra k=v,...
  fairshare pool join --mesh         every device of the mesh (Hona/mesh) joins, and the pool follows the mesh
  fairshare pool leave <name>        a machine leaves
  fairshare pool place [flags]       where a piece of work goes: exit 0 and the machine, 1 the pool is full, 2 cannot tell
      --repo R --count N --checkout --urgent --cores X --mem-gb X --disk-gb X --label "<what>"
      --needs windows,deploy         only a machine that is or can do each of these
      --among a,b                    only these machines
      --take                         count the work on the machine picked until its load shows
      --no-wake                      name a sleeping machine instead of waking it
      --json
  fairshare pool run [flags] -- <command>
                                     run a command where there is room: in --repo's folder, on --commit when given;
                                     --wait N keeps asking for N minutes. The command's exit, or 69 when the pool failed.
`

func cmdPool(args []string) int {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	fail := func(err error) int {
		fmt.Fprintln(os.Stderr, "degraded:", err)
		return 2
	}
	switch sub {
	case "", "status", "--json":
		p, err := loadPool()
		if err != nil {
			return fail(err)
		}
		return poolStatus(p, slices.Contains(args, "--json"))
	case "join":
		if err := poolJoin(args[1:]); err != nil {
			return fail(err)
		}
		return 0
	case "leave":
		if len(args) < 2 {
			return fail(errors.New("pool leave <name>"))
		}
		p, err := readPool()
		if err != nil {
			return fail(err)
		}
		kept := slices.DeleteFunc(slices.Clone(p.Members), func(m member) bool { return m.Name == args[1] })
		if len(kept) == len(p.Members) {
			return fail(fmt.Errorf("no machine %s in the pool", args[1]))
		}
		p.Members = kept
		if err := savePool(p); err != nil {
			return fail(err)
		}
		fmt.Printf("fairshare: %s left the pool; %d machines remain\n", args[1], len(kept))
		return 0
	case "place":
		fs := flag.NewFlagSet("pool place", flag.ExitOnError)
		get := workFlags(fs)
		take := fs.Bool("take", false, "count the work on the machine picked")
		noWake := fs.Bool("no-wake", false, "name a sleeping machine instead of waking it")
		asJSON := fs.Bool("json", false, "print the whole answer as JSON")
		fs.Parse(args[1:])
		p, err := loadPool()
		if err != nil {
			if *asJSON {
				printJSON(map[string]any{"ok": nil, "degraded": err.Error()})
			}
			return fail(err)
		}
		wake := wakeMember(p)
		if *noWake {
			wake = nil
		}
		pl := place(p, get(), *take, wake)
		record(pl, get())
		if *asJSON {
			printJSON(pl)
		} else {
			fmt.Println(pl.Reason)
		}
		return exitOf(pl)
	case "run":
		return poolRun(args[1:])
	case "here":
		if len(args) < 2 {
			return fail(errors.New("pool here <errand>"))
		}
		return poolHere(args[1])
	case "help", "-h", "--help":
		fmt.Print(poolUsage)
		return 0
	}
	fmt.Fprintf(os.Stderr, "fairshare: no pool command %q\n\n%s", sub, poolUsage)
	return 2
}

// record writes a placement to decisions.jsonl, where every other answer already goes.
func record(pl placement, w work) {
	appendLine("decisions.jsonl", map[string]any{"at": time.Now().UTC(), "event": "pool place", "label": w.Label, "repo": w.Repo,
		"ok": pl.OK, "machine": pl.Machine, "woke": pl.Woke, "took": pl.Took, "seconds": pl.Seconds, "reason": pl.Reason, "machines": pl.Machines}, 8<<20)
}

func poolJoin(args []string) error {
	fs := flag.NewFlagSet("pool join", flag.ExitOnError)
	name := fs.String("name", "", "the machine's name in the pool")
	via := fs.String("via", "", "the words that run a command on it; none: this machine")
	prog := fs.String("fairshare", "", "the fairshare program there")
	wake := fs.String("wake", "", "the command that wakes it")
	awake := fs.String("awake", "", "the command that says whether it is up, run before anything is sent to it")
	awakeMatch := fs.String("awake-match", "", "a pattern that command's output must hold")
	hours := fs.String("hours", "", "the command that prints its plan-hours left")
	keep := fs.Float64("keep-hours", 0, "plan-hours that are never spent on a wake")
	can := fs.String("can", "", "what it can do beyond its OS: gpu,deploy")
	workDir := fs.String("work", "", "the folder that holds its repos")
	repos := fs.String("repos", "", "repos kept somewhere else: name=folder,...")
	busy := fs.String("busy-dir", "", "a folder its idle watch reads for running work")
	extra := fs.String("extra", "", "handed back with a placement: k=v,...")
	mesh := fs.Bool("mesh", false, "every device of the mesh joins, and the pool follows the mesh")
	meshProg := fs.String("mesh-program", "mesh", "the mesh program")
	fs.Parse(args)

	p, err := readPool()
	if err != nil {
		return err
	}
	if *mesh {
		p.Mesh, err = argvOf(*meshProg)
		if err != nil || len(p.Mesh) == 0 {
			return fmt.Errorf("--mesh-program names no program")
		}
		devices, err := meshDevices(p.Mesh, p.askTime())
		if err != nil {
			return fmt.Errorf("the mesh did not answer, so nothing joined: %w", err)
		}
		p, _ = followMesh(p, devices)
		if err := savePool(stripOffline(p)); err != nil {
			return err
		}
		fmt.Printf("fairshare: the pool follows the mesh: %d devices, %d machines in the pool. A device that runs `mesh up` is in the pool from then on.\n", len(devices), len(p.Members))
		for _, m := range p.Members {
			if m.From == "mesh" && len(m.Via) > 0 {
				if _, _, err := reading(p, m); err != nil && !m.offline {
					fmt.Printf("  degraded: %s does not answer as a pool member yet: %v\n", m.Name, err)
				}
			}
		}
		return nil
	}
	m := member{Name: *name, Fairshare: *prog, AwakeMatch: *awakeMatch, KeepHours: *keep, Can: list(*can), Work: *workDir, Repos: pairs(*repos), BusyDir: *busy, Extra: pairs(*extra)}
	for _, c := range []struct {
		text string
		into *[]string
	}{{*via, &m.Via}, {*wake, &m.Wake}, {*awake, &m.Awake}, {*hours, &m.Hours}} {
		if *c.into, err = argvOf(c.text); err != nil {
			return err
		}
	}
	if len(m.Via) > 0 && m.Name == "" {
		return errors.New("a machine reached through --via needs a --name")
	}
	r, why, err := reading(p, m)
	if err != nil {
		return fmt.Errorf("%s did not answer, so it did not join: %w%s", cmpOr(m.Name, "this machine"), err, why)
	}
	m.OS, m.Processors = r.OS, r.Processors
	if m.Name == "" {
		m.Name, _, _ = strings.Cut(strings.ToLower(r.Machine), ".")
	}
	if at := slices.IndexFunc(p.Members, func(o member) bool { return o.Name == m.Name }); at >= 0 {
		p.Members[at] = m
	} else {
		p.Members = append(p.Members, m)
	}
	if err := savePool(stripOffline(p)); err != nil {
		return err
	}
	how := "this machine"
	if len(m.Via) > 0 {
		how = "reached through `" + strings.Join(m.Via, " ") + "`"
	}
	var names []string
	for _, o := range p.Members {
		names = append(names, o.Name)
	}
	fmt.Printf("fairshare: %s joined the pool: %s, %d processors, %s. The pool has %d: %s.\n", m.Name, m.OS, m.Processors, how, len(names), strings.Join(names, ", "))
	return nil
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// reading asks a member for its reading, which is also how a machine proves it is in reach.
func reading(p poolFile, m member) (Reading, string, error) {
	var r Reading
	out, stderr, err := send(p, m, errand{Args: []string{"reading"}})
	if json.Unmarshal(jsonPart(out), &r) != nil || r.Processors == 0 {
		if err == nil {
			err = errors.New("it printed no reading")
		}
		why := ""
		if text := tail(string(stderr)+" "+string(out), 300); text != "" {
			why = " (" + text + ")"
		}
		if len(m.Via) > 0 {
			why += fmt.Sprintf(". It needs this build of fairshare at `%s`, and to be awake", cmpOr(m.Fairshare, "fairshare"))
		}
		return r, why, err
	}
	return r, "", nil
}

// poolStatus prints the pool as one place: what it has, what is in use, and where a thread fits.
func poolStatus(p poolFile, asJSON bool) int {
	type row struct {
		reply
		OS         string   `json:"os"`
		Processors int      `json:"processors"`
		Reserved   int      `json:"reserved"`
		Capacity   float64  `json:"capacity"`
		Presence   string   `json:"presence,omitempty"`
		MemFreeGB  float64  `json:"mem_free_gb"`
		HoursLeft  *float64 `json:"hours_left,omitempty"`
		Via        string   `json:"via,omitempty"`
	}
	rows := make([]row, len(p.Members))
	var wg sync.WaitGroup
	for i, m := range p.Members {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := row{OS: m.OS, Processors: m.Processors, Via: strings.Join(m.Via, " ")}
			if m.offline {
				r.reply = reply{Name: m.Name, State: "unreachable", Why: "offline in the mesh"}
			} else {
				start := time.Now()
				out, stderr, err := send(p, m, errand{Args: []string{"status", "--json"}})
				r.reply = read(m, out, stderr, err)
				r.Seconds = round2(time.Since(start).Seconds())
			}
			if v := r.verdict; v != nil {
				r.OS, r.Processors, r.Reserved, r.Capacity = v.Reading.OS, v.Reading.Processors, v.Reading.Reserved, v.Reading.Capacity
				r.Presence, r.MemFreeGB = v.Reading.Presence.State, v.Reading.MemAvailGB
			}
			if len(m.Hours) > 0 {
				if left, known := hoursLeft(p, m); known {
					r.HoursLeft = &left
				}
			}
			rows[i] = r
		}()
	}
	wg.Wait()
	if asJSON {
		printJSON(map[string]any{"machines": rows})
		return 0
	}
	var processors, awake int
	var capacity, used, limit float64
	var fits, asleep []string
	for _, r := range rows {
		if r.verdict == nil {
			if r.State == "asleep" {
				asleep = append(asleep, r.Name)
			}
			continue
		}
		awake++
		processors += r.Processors
		capacity += r.Capacity
		used += r.Used
		limit += r.Limit
		if r.State == "room" {
			fits = append(fits, r.Name)
		}
	}
	fmt.Printf("The pool: %d machines, %d awake. Those have %d processors, %.0f of them for agent work; %.1f are in use and %.1f are free under each machine's limit.\n",
		len(rows), awake, processors, capacity, used, max(0, limit-used))
	switch {
	case len(fits) > 0:
		fmt.Printf("A new thread fits on %s.", strings.Join(fits, ", "))
	case len(asleep) > 0:
		fmt.Printf("A new thread fits on no machine that is awake.")
	default:
		fmt.Printf("A new thread fits nowhere.")
	}
	if len(asleep) > 0 {
		fmt.Printf(" Asleep, woken only when the others are full: %s.", strings.Join(asleep, ", "))
	}
	fmt.Print("\n\n")
	fmt.Printf("  %-26s %-12s %-8s %5s %5s %6s %7s %6s %6s %7s  %s\n", "MACHINE", "STATE", "OS", "PROC", "KEPT", "WORK", "IN USE", "LIMIT", "FREE", "MEM GB", "NOTE")
	for _, r := range rows {
		note := ""
		switch {
		case r.Presence == "full-screen":
			note = "something is full screen there"
		case r.Presence == "active":
			note = "someone is at it"
		case r.Presence == "away":
			note = "nobody at it"
		case r.verdict == nil:
			note = tail(r.Why, 90)
		}
		if r.HoursLeft != nil {
			note = strings.TrimSpace(fmt.Sprintf("%.1f plan-hours left. %s", *r.HoursLeft, note))
		}
		if r.verdict == nil {
			fmt.Printf("  %-26.26s %-12s %-8s %5d %5s %6s %7s %6s %6s %7s  %s\n", r.Name, r.State, r.OS, r.Processors, "-", "-", "-", "-", "-", "-", note)
			continue
		}
		fmt.Printf("  %-26.26s %-12s %-8s %5d %5d %6.0f %7.1f %6.1f %6.1f %7.1f  %s\n", r.Name, r.State, r.OS, r.Processors, r.Reserved, r.Capacity,
			r.Used, r.Limit, max(0, r.Limit-r.Used), r.MemFreeGB, note)
	}
	return 0
}

// poolRun runs a command where the pool has room for it.
func poolRun(args []string) int {
	fs := flag.NewFlagSet("pool run", flag.ExitOnError)
	get := workFlags(fs)
	commit := fs.String("commit", "", "run it in a checkout of exactly this commit of --repo")
	wait := fs.Float64("wait", 0, "minutes to keep asking while the pool is full")
	fs.Parse(args)
	argv := fs.Args()
	stop := func(format string, a ...any) int {
		fmt.Fprintf(os.Stderr, "degraded: "+format+"\n", a...)
		return cannotRun
	}
	if len(argv) == 0 {
		return stop("pool run needs a command after --")
	}
	w := get()
	if w.Label == "" {
		w.Label = tail(strings.Join(argv, " "), 80)
	}
	w.Folder = w.Repo != ""
	if *commit != "" && w.Repo == "" {
		return stop("--commit needs --repo")
	}
	p, err := loadPool()
	if err != nil {
		return stop("%v", err)
	}
	until := time.Now().Add(time.Duration(*wait * float64(time.Minute)))
	var pl placement
	for {
		pl = place(p, w, true, wakeMember(p))
		record(pl, w)
		if pl.OK || !time.Now().Before(until) {
			break
		}
		fmt.Fprintf(os.Stderr, "fairshare: %s Asking again in 20 s.\n", pl.Reason)
		time.Sleep(20 * time.Second)
	}
	if !pl.OK {
		return stop("%s", pl.Reason)
	}
	m := pl.member
	fmt.Fprintf(os.Stderr, "fairshare: %s (placed in %.1f s)\n", pl.Reason, pl.Seconds)
	cmd := m.command(context.Background(), errand{Argv: argv, Dir: m.folder(w.Repo), Commit: *commit, BusyDir: m.BusyDir})
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	start := time.Now()
	code := exitCode(cmd.Run())
	seconds := round2(time.Since(start).Seconds())
	fmt.Fprintf(os.Stderr, "fairshare: %s ran it in %.1f s, exit %d\n", m.Name, seconds, code)
	appendLine("decisions.jsonl", map[string]any{"at": time.Now().UTC(), "event": "pool run", "label": w.Label, "repo": w.Repo, "commit": *commit,
		"machine": m.Name, "woke": pl.Woke, "placed_seconds": pl.Seconds, "seconds": seconds, "exit": code}, 8<<20)
	return code
}

// exitCode is a finished command's exit, or cannotRun when it never ran.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() >= 0 {
		return exit.ExitCode()
	}
	fmt.Fprintln(os.Stderr, "degraded:", err)
	return cannotRun
}

// homePath turns a folder that starts with ~ into this user's.
func homePath(dir string) string {
	if rest, ok := strings.CutPrefix(dir, "~"); ok && (rest == "" || rest[0] == '/' || rest[0] == '\\') {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	return dir
}

// poolHere does an errand on this machine: the far end of every member's via.
func poolHere(word string) int {
	e, err := readErrand(word)
	if err != nil {
		fmt.Fprintln(os.Stderr, "degraded:", err)
		return cannotRun
	}
	dir := homePath(e.Dir)
	host, _ := os.Hostname()
	if dir != "" {
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			if len(e.Args) > 0 {
				printJSON(map[string]any{"ok": nil, "closed": fmt.Sprintf("no folder %s on %s", dir, host)})
				return 3
			}
			fmt.Fprintf(os.Stderr, "degraded: no folder %s on %s\n", dir, host)
			return cannotRun
		}
	}
	if len(e.Args) > 0 {
		cmd := exec.Command(self(), e.Args...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		return exitCode(cmd.Run())
	}
	if len(e.Argv) == 0 {
		fmt.Fprintln(os.Stderr, "degraded: the errand names nothing to run")
		return cannotRun
	}
	if e.Commit != "" {
		tree, err := checkout(dir, e.Commit)
		if err != nil {
			fmt.Fprintf(os.Stderr, "degraded: %s could not check out %s in %s: %v\n", host, e.Commit, dir, err)
			return cannotRun
		}
		defer git(dir, "worktree", "remove", "--force", tree)
		dir = tree
	}
	if busy := homePath(e.BusyDir); busy != "" {
		if os.MkdirAll(busy, 0o755) == nil {
			mark := filepath.Join(busy, fmt.Sprintf("pool-%d", os.Getpid()))
			if os.WriteFile(mark, []byte(strconv.Itoa(os.Getpid())), 0o644) == nil {
				defer os.Remove(mark)
			}
		}
	}
	cmd := exec.Command(e.Argv[0], e.Argv[1:]...)
	if e.Login && runtime.GOOS != "windows" {
		cmd = exec.Command(loginShell(), append([]string{"-lc", `exec "$@"`, "fairshare"}, e.Argv...)...)
	}
	cmd.Dir = dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return exitCode(cmd.Run())
}

// loginShell is the user's shell when it reads `exec "$@"` the way sh does, else sh.
func loginShell() string {
	shell := os.Getenv("SHELL")
	switch filepath.Base(shell) {
	case "bash", "zsh", "sh", "dash", "ksh":
		return shell
	}
	return "/bin/sh"
}

func git(dir string, args ...string) error {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	hideWindow(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %v: %s", args[0], err, tail(string(out), 300))
	}
	return nil
}

// checkout makes a folder holding exactly that commit of the repo in dir, fetching it when the
// repo does not have it yet, and returns the folder. The repo's own checkout is not touched.
func checkout(dir, commit string) (string, error) {
	if git(dir, "cat-file", "-e", commit+"^{commit}") != nil {
		if err := git(dir, "fetch", "--quiet", "origin", commit); err != nil {
			return "", err
		}
	}
	runs := filepath.Join(homeDir(), "runs")
	if err := os.MkdirAll(runs, 0o755); err != nil {
		return "", err
	}
	tree := filepath.Join(runs, fmt.Sprintf("%s-%.9s-%d", filepath.Base(dir), commit, os.Getpid()))
	if err := git(dir, "worktree", "add", "--detach", tree, commit); err != nil {
		return "", err
	}
	return tree, nil
}
