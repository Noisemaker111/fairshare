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
	if err := stopMeter(); err != nil {
		return err
	}
	exe := installedExe()
	if !strings.EqualFold(filepath.Clean(self), filepath.Clean(exe)) {
		if err := copyFile(self, exe); err != nil {
			return fmt.Errorf("copy to %s: %w", exe, err)
		}
	}
	// The Linux build, when it was built beside this one: boat-host.sh copies it to each Linux
	// host from here, so every machine answers with the same program.
	if linux := filepath.Join(filepath.Dir(self), "fairshare-linux"); !strings.EqualFold(filepath.Dir(self), filepath.Dir(exe)) {
		if _, err := os.Stat(linux); err == nil {
			if err := copyFile(linux, filepath.Join(filepath.Dir(exe), "fairshare-linux")); err != nil {
				return fmt.Errorf("copy the Linux build: %w", err)
			}
		}
	}
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
