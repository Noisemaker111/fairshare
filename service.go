package main

// fairshare as part of the PC rather than of one harness: `fairshare install` registers a scheduled
// task that runs the installed copy's `adopt` for every chat on the PC, with no window. The task
// fires every minute and does nothing while adopt runs, so adopt starts within a minute of logon
// and comes back within a minute if it ever ends. No admin rights needed.

import (
	"embed"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

const adoptTask = "fairshare"

//go:embed opencode-plugin/index.mjs opencode-plugin/package.json
var openCodePlugin embed.FS

func adoptTaskXML(exe string) []byte {
	who := ""
	if u, err := user.Current(); err == nil {
		who = "<UserId>" + xmlEscape(u.Username) + "</UserId>"
	}
	conhost := filepath.Join(os.Getenv("SystemRoot"), "System32", "conhost.exe")
	x := `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo><Description>fairshare adopt: every agent chat's work in its own low-priority lane, shared fairly and logged (Projects\fairshare)</Description></RegistrationInfo>
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
  <Actions Context="Author"><Exec><Command>` + xmlEscape(conhost) + `</Command><Arguments>--headless "` + xmlEscape(exe) + `" adopt</Arguments></Exec></Actions>
</Task>
`
	u := utf16.Encode([]rune(x))
	b := []byte{0xff, 0xfe}
	for _, c := range u {
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

// adoptRunning reports whether a machine-wide adopt holds its mutex.
func adoptRunning() bool {
	h, err := windows.OpenMutex(windows.SYNCHRONIZE, false, windows.StringToUTF16Ptr(adoptMutex))
	if err != nil {
		return false
	}
	windows.CloseHandle(h)
	return true
}

// stopAdopt asks the running adopt to finish (it closes its lanes and logs them) and waits for it.
func stopAdopt() error {
	ev, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, windows.StringToUTF16Ptr(adoptStopEvent))
	if err != nil {
		return nil // none running
	}
	windows.SetEvent(ev)
	windows.CloseHandle(ev)
	for t0 := time.Now(); time.Since(t0) < 15*time.Second; time.Sleep(100 * time.Millisecond) {
		if !adoptRunning() {
			return nil
		}
	}
	return fmt.Errorf("the running adopt did not stop within 15 s")
}

// installService (re)registers the task and restarts adopt from the installed copy.
func installService(exe string, uninstall bool) error {
	if err := stopAdopt(); err != nil {
		return err
	}
	if uninstall {
		if err := schtasks("/Query", "/TN", adoptTask); err != nil {
			return nil
		}
		return schtasks("/Delete", "/TN", adoptTask, "/F")
	}
	f, err := os.CreateTemp("", "fairshare-task-*.xml")
	if err != nil {
		return err
	}
	f.Write(adoptTaskXML(exe))
	f.Close()
	defer os.Remove(f.Name())
	if err := schtasks("/Create", "/TN", adoptTask, "/XML", f.Name(), "/F"); err != nil {
		return err
	}
	if err := schtasks("/Run", "/TN", adoptTask); err != nil {
		return err
	}
	for t0 := time.Now(); time.Since(t0) < 10*time.Second; time.Sleep(100 * time.Millisecond) {
		if adoptRunning() {
			return nil
		}
	}
	return fmt.Errorf("the task ran but adopt is not running after 10 s")
}

// installOpenCodePlugin puts the plugin where OpenCode loads every folder from.
func installOpenCodePlugin(uninstall bool) (string, error) {
	dir := filepath.Join(homeDir(), ".config", "opencode", "plugins", "fairshare")
	if uninstall {
		return dir, os.RemoveAll(dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return dir, err
	}
	for _, name := range []string{"index.mjs", "package.json"} {
		b, _ := openCodePlugin.ReadFile("opencode-plugin/" + name)
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			return dir, err
		}
	}
	return dir, nil
}
