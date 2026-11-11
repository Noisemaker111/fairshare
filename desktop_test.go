package main

import (
	"strings"
	"testing"
	"time"
)

// 2026-11-11, 13:30: Jon at the PC, 8 processors kept for him and half of them idle, memory and
// disk free, and the desktop stalling for up to two seconds at a time because three gates were
// driving windows and capturing the screen on it.
func TestWorkThatDrivesTheDesktopStartsOnlyWhileNobodyIsAtIt(t *testing.T) {
	s := defaultSettings()
	gate := LiveWork{Kind: "run", Repo: "owl-go", What: "cargo test port_parity", Desktop: true}
	build := LiveWork{Kind: "run", Repo: "owl-go", What: "cargo build"}
	at := func(state string, work ...LiveWork) Reading {
		r := pc(4, 4, 36, 60, state)
		r.Work = work
		return r
	}
	full := at("full-screen")
	full.Presence.App = "7daystodie.exe"
	cases := []struct {
		name  string
		r     Reading
		taken bool
		ok    bool
		says  string
	}{
		{"someone is at the PC", at("active"), false, false, "someone is at the PC"},
		{"a game fills the screen", full, false, false, "7daystodie.exe is full screen"},
		{"nobody has touched the PC for ten minutes", at("away"), false, true, ""},
		{"another piece of work is driving it", at("away", build, gate), false, false, "run driving the desktop in owl-go: cargo test port_parity has it"},
		{"work that does not drive it is no obstacle", at("away", build), false, true, ""},
		{"one was let in a moment ago and has not started", at("away"), true, false, "let in a moment ago"},
		{"a machine with no screen has no desktop", at("none"), false, false, "no desktop"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := decide(c.r, s, Ask{Kind: "run", Desktop: true, DesktopTaken: c.taken}, Need{})
			if v.Desk == nil || v.Desk.OK != c.ok || v.OK != c.ok {
				t.Fatalf("ok = %v (desktop %+v), want %v: %s", v.OK, v.Desk, c.ok, v.Reason)
			}
			if !c.ok && (!strings.Contains(v.Reason, c.says) || !strings.Contains(v.Reason, "It drives the desktop")) {
				t.Fatalf("reason lacks %q: %s", c.says, v.Reason)
			}
			// The processors had room all along: that is why the rule is its own.
			if !v.CPU.OK || !v.Mem.OK {
				t.Fatalf("processors or memory refused it: %s", v.Reason)
			}
		})
	}
	// Work that does not drive the desktop is not asked about it.
	if v := decide(at("active", gate), s, Ask{Kind: "run"}, Need{}); !v.OK || v.Desk != nil {
		t.Fatalf("a build was refused for the desktop: %s", v.Reason)
	}
}

func TestThePoolAsksTheMachineThatHasTheDesktop(t *testing.T) {
	pc := member{Name: "pc", OS: "windows"}
	host := member{Name: "t3-host-2", OS: "linux", Via: []string{"boat", "ssh", "bx_1"}}
	w := work{Kind: "run", What: "cargo test port_parity", Repo: "owl-go", Needs: []string{"desktop"}}
	if ok, why := open(host, w); ok || why != "has no desktop" {
		t.Errorf("a Linux host was open to work that drives a desktop: %v %q", ok, why)
	}
	if ok, why := open(pc, w); !ok {
		t.Errorf("the PC was closed to it before it was asked: %q", why)
	}
	if got := strings.Join(w.roomArgs(pc, true, nil), " "); got != "room --json --count 1 --kind run --what cargo test port_parity --repo owl-go --desktop --take" {
		t.Errorf("the PC is asked %q", got)
	}
	if got := strings.Join(work{Kind: "run", What: "cargo build"}.roomArgs(pc, false, nil), " "); strings.Contains(got, "--desktop") {
		t.Errorf("a build asks for the desktop: %q", got)
	}
}

func TestTheDesktopIsTakenFromTheMomentWorkIsLetIn(t *testing.T) {
	t.Setenv("FAIRSHARE_HOME", t.TempDir())
	now := time.Now()
	settle := 5 * time.Minute
	if desktopPending(now, settle, nil) {
		t.Fatal("the desktop is taken with nothing let in")
	}
	takeGrant(now.Add(-20*time.Second), settle, grant{Need: Need{Cores: 2}, Kind: "run", Repo: "owl-go", Desktop: true})
	takeGrant(now.Add(-10*time.Second), settle, grant{Need: Need{Cores: 2}, Kind: "run", Repo: "appforgutters"})
	if !desktopPending(now, settle, nil) {
		t.Fatal("a desktop run let in 20 s ago and not yet seen does not hold the desktop")
	}
	// Once the meter sees it running it is open work, and counts there.
	seen := []LiveWork{{Kind: "run", Repo: "owl-go", Since: now.Add(-15 * time.Second), Desktop: true}}
	if desktopPending(now, settle, seen) {
		t.Fatal("a desktop run that is running is still counted as about to start")
	}
	if d := desktopFree(func() Reading { r := pc(4, 4, 36, 60, "away"); r.Work = seen; return r }(), false); d.OK {
		t.Fatal("and the desktop is free while it runs")
	}
}
