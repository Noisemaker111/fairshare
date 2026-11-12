//go:build windows

package main

// `fairshare install` makes the meter part of the PC: it copies this program to fairshare's home
// and registers a scheduled task that starts the meter with no window. The task fires every minute
// and does nothing while the meter runs, so the meter starts within a minute of logon and comes
// back within a minute if it ever ends. It runs in the user's own session (it has to: what is in
// the foreground and when the keyboard was last used are per session) and needs no admin rights.

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf16"
)

const meterTask = "fairshare-meter"

func installedExe() string { return filepath.Join(homeDir(), "bin", "fairshare.exe") }

func meterTaskXML(exe string) []byte {
	who := ""
	if u, err := user.Current(); err == nil {
		who = "<UserId>" + xmlEscape(u.Username) + "</UserId>"
	}
	conhost := filepath.Join(os.Getenv("SystemRoot"), "System32", "conhost.exe")
	x := `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo><Description>fairshare meter: measures what this PC's processors, memory and disk are taking, so new agent work starts only where there is room (Projects\fairshare)</Description></RegistrationInfo>
  <Triggers>
    <TimeTrigger><StartBoundary>2026-01-01T00:00:00</StartBoundary><Enabled>true</Enabled><Repetition><Interval>PT1M</Interval><StopAtDurationEnd>false</StopAtDurationEnd></Repetition></TimeTrigger>
  </Triggers>
  <Principals><Principal id="Author">` + who + `<LogonType>InteractiveToken</LogonType><RunLevel>LeastPrivilege</RunLevel></Principal></Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>true</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <IdleSettings><StopOnIdleEnd>false</StopOnIdleEnd><RestartOnIdle>false</RestartOnIdle></IdleSettings>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>false</Hidden>
    <RunOnlyIfIdle>false</RunOnlyIfIdle>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Priority>4</Priority>
  </Settings>
  <Actions Context="Author"><Exec><Command>` + xmlEscape(conhost) + `</Command><Arguments>--headless "` + xmlEscape(exe) + `" meter</Arguments></Exec></Actions>
</Task>
`
	b := []byte{0xff, 0xfe}
	for _, c := range utf16.Encode([]rune(x)) {
		b = append(b, byte(c), byte(c>>8))
	}
	return b
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}

func schtasks(args ...string) error {
	out, err := exec.Command("schtasks.exe", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("schtasks %s: %v: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return nil
}

// stopMeter asks the running meter to end and waits for it.
func stopMeter() error {
	if !meterRunning() {
		return nil
	}
	if err := os.WriteFile(stopFile(), nil, 0o644); err != nil {
		return err
	}
	for t0 := time.Now(); time.Since(t0) < 10*time.Second; time.Sleep(100 * time.Millisecond) {
		if !meterRunning() {
			return nil
		}
	}
	os.Remove(stopFile())
	return fmt.Errorf("the running meter did not stop within 10 s")
}

// swapIn puts exe.new in exe's place. Windows will not write over a program that is running,
// and fairshare runs all the time: a pool run that carries a gate holds the installed program
// for as long as the gate lasts. It does let a running program be renamed, so the old one is
// moved aside and the new one takes its name; old ones nothing runs any more are removed.
//
// Why: on 2026-11-11 an install stopped the meter, could not write over the program a gate was
// running, and left the PC with no meter until it was started by hand.
func swapIn(exe string) error {
	dir, name := filepath.Dir(exe), filepath.Base(exe)
	if olds, err := filepath.Glob(filepath.Join(dir, name+".*.old")); err == nil {
		for _, old := range olds {
			os.Remove(old) // fails while something still runs it, and is tried again next time
		}
	}
	aside := fmt.Sprintf("%s.%d.old", exe, time.Now().UnixNano())
	if err := os.Rename(exe, aside); err != nil && !os.IsNotExist(err) {
		os.Remove(exe + ".new")
		return fmt.Errorf("move the running %s aside: %w", name, err)
	}
	if err := os.Rename(exe+".new", exe); err != nil {
		os.Rename(aside, exe)
		return fmt.Errorf("put the new %s in place: %w", name, err)
	}
	return nil
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	out, err := os.Create(to)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// shims makes `fairshare` a command in every shell: a two-line starter in each of the user's own
// bin folders that already exists (they are on PATH in Git Bash, cmd and PowerShell), pointing at
// the installed program, so there is still one copy to update.
func shims(exe string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	for _, dir := range []string{filepath.Join(home, "bin"), filepath.Join(home, ".local", "bin")} {
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			continue
		}
		sh := "#!/bin/sh\nexec \"" + filepath.ToSlash(exe) + "\" \"$@\"\n"
		cmd := "@\"" + exe + "\" %*\r\n"
		if os.WriteFile(filepath.Join(dir, "fairshare"), []byte(sh), 0o755) != nil || os.WriteFile(filepath.Join(dir, "fairshare.cmd"), []byte(cmd), 0o644) != nil {
			fmt.Fprintln(os.Stderr, "degraded: could not write the fairshare starter in", dir)
		}
	}
}

// install puts this program in fairshare's home, registers the task and starts the meter; with
// uninstall it stops the meter and removes the task, leaving the settings and history.
func install(uninstall bool) error {
	if uninstall {
		// The task first, or it would start the meter again within the minute.
		if schtasks("/Query", "/TN", meterTask) == nil {
			if err := schtasks("/Delete", "/TN", meterTask, "/F"); err != nil {
				return err
			}
		}
		return stopMeter()
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	exe := installedExe()
	fresh := !strings.EqualFold(filepath.Clean(self), filepath.Clean(exe))
	// The new program is put beside the old one before anything is stopped, so an install that
	// cannot go ahead leaves the meter running as it was.
	if fresh {
		if err := copyFile(self, exe+".new"); err != nil {
			return fmt.Errorf("copy to %s: %w", exe+".new", err)
		}
	}
	if err := stopMeter(); err != nil {
		os.Remove(exe + ".new")
		return err
	}
	if fresh {
		if err := swapIn(exe); err != nil {
			// The old program is still in place: start the meter again on it.
			schtasks("/Run", "/TN", meterTask)
			return err
		}
	}
	// The Linux build, when it was built beside this one: boat-host.sh copies it to each Linux
	// host from here, so every machine answers with the same program. Nothing runs it here, so
	// it is copied over.
	if linux := filepath.Join(filepath.Dir(self), "fairshare-linux"); !strings.EqualFold(filepath.Dir(self), filepath.Dir(exe)) {
		if _, err := os.Stat(linux); err == nil {
			if err := copyFile(linux, filepath.Join(filepath.Dir(exe), "fairshare-linux")); err != nil {
				return fmt.Errorf("copy the Linux build: %w", err)
			}
		}
	}
	shims(exe)
	f, err := os.CreateTemp("", "fairshare-task-*.xml")
	if err != nil {
		return err
	}
	f.Write(meterTaskXML(exe))
	f.Close()
	defer os.Remove(f.Name())
	if err := schtasks("/Create", "/TN", meterTask, "/XML", f.Name(), "/F"); err != nil {
		return err
	}
	if err := schtasks("/Run", "/TN", meterTask); err != nil {
		return err
	}
	// The meter's first look only sets its counters; the second, sample_seconds later, is the
	// first reading.
	s, _ := loadSettings()
	limit := time.Duration((2*s.SampleSeconds + 15) * float64(time.Second))
	for t0 := time.Now(); time.Since(t0) < limit; time.Sleep(250 * time.Millisecond) {
		if r, err := currentReading(s); err == nil && time.Since(r.At) < time.Duration(2*s.SampleSeconds*float64(time.Second)) && meterRunning() {
			return nil
		}
	}
	return fmt.Errorf("the task ran but the meter wrote no reading within %s", limit)
}
