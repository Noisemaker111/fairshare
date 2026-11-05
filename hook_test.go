package main

import (
	"os/exec"
	"strings"
	"testing"
)

// The hook starts on every agent tool call, so every package init is paid on every call. net's
// init starts Winsock (3 ms) though fairshare opens no socket; golang.org/x/sys/windows pulled it
// in, which is why Windows calls go through internal/windows.
func TestNoNetInBinary(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Skip("go list:", err)
	}
	for _, p := range strings.Fields(string(out)) {
		if p == "net" {
			t.Fatal("fairshare links package net; its init costs every hook call 3 ms. `go list -f '{{.ImportPath}}: {{.Imports}}' -deps .` shows who imports it")
		}
	}
}
