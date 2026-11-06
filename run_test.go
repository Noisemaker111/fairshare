package main

import (
	"os/exec"
	"strings"
	"testing"
)

// A measurement that needs normal priority (strobe refuses frame and input-latency metrics at idle
// priority) is run with `run --normal` from inside a chat's lane. The outer `run` here is that lane:
// an idle-priority job its children cannot raise themselves out of. `--normal` once only made a job
// nested in it, so the command still ran at idle priority and said nothing.
func TestRunNormalLeavesTheIdleJobAroundIt(t *testing.T) {
	bin := fairshareBin(t)
	out, err := exec.Command(bin, "run", "--", bin, "run", "--normal", "--", bin, "inspect").CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	first, _, _ := strings.Cut(string(out), "\n")
	if !strings.Contains(first, "priority=normal io=2 mem=5") {
		t.Fatalf("run --normal inside an idle job gave %q, want normal CPU, disk and memory priority\n%s", first, out)
	}
	// and without --normal the same nesting keeps agent work low
	out, err = exec.Command(bin, "run", "--", bin, "run", "--", bin, "inspect").CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if first, _, _ := strings.Cut(string(out), "\n"); !strings.Contains(first, "priority=idle") {
		t.Fatalf("plain run gave %q, want idle", first)
	}
}

// Strobe's browser windows, started through `run --normal` from an agent's shell, once opened
// jgengine games on Jon's screen and over his game. Started from the screen's desktop, the command
// must begin on a private one; started from any other (an Owl drive desktop), it stays there.
func TestRunNormalStartsOffScreen(t *testing.T) {
	bin := fairshareBin(t)
	desktop := func(args ...string) string {
		out, err := exec.Command(bin, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		_, d, _ := strings.Cut(string(out), " desktop=")
		d, _, _ = strings.Cut(d, "\n")
		return strings.TrimSpace(d)
	}
	onScreen := func(d string) bool { return d == "" || strings.HasSuffix(strings.ToLower(d), `\default`) }
	here := desktop("inspect")
	got := desktop("run", "--normal", "--", bin, "inspect")
	if onScreen(here) {
		if onScreen(got) || !strings.Contains(got, `\fairshare-`) {
			t.Fatalf("run --normal from the screen's desktop (%q) started its command on %q, want a private fairshare desktop", here, got)
		}
	} else if got != here {
		t.Fatalf("run --normal from %q started its command on %q, want the same desktop", here, got)
	}
	if plain := desktop("run", "--", bin, "inspect"); plain != here {
		t.Fatalf("plain run moved its command from %q to %q", here, plain)
	}
}
