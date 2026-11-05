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
