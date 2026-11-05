# fairshare

A Go command for Windows that runs AI agents' heavy work so it yields to the foreground, freezes
instead of exhausting memory, and is computed once per code state. See `README.md`.

## Test

```
go vet ./... && go test ./... && go build -o fairshare.exe .
```

The bench is the real check of the foreground claim. It puts load on the whole PC, so keep runs
under 10 minutes and never run it while Jon is gaming:

```
fairshare bench --jobs 50 --mode plain --units 1200 --threads 4 --mem 40 --spawn-every 20 --files 20 --pairs 3 --deadline 4m --out results/bench.jsonl
fairshare bench --jobs 50 --mode fair  --units 1200 --threads 4 --mem 40 --spawn-every 20 --files 20 --pairs 3 --deadline 4m --out results/bench.jsonl
```

Read "jobs on" against "jobs off" in the same run, never against another run: other sessions'
load on this PC swings from 20% to 100% within minutes. The bench's on/off freezing and `run`'s
memory freezing both suspend processes, so use `--pairs 0` when testing memory freezing.

The thread shell is live: `fairshare install` puts `%LOCALAPPDATA%\fairshare\bin\fairshare.exe
hook` in every Claude Code, Codex and OpenCode chat, and keeps that copy's `adopt` running for the
whole PC (scheduled task `fairshare`). Rebuilding `fairshare.exe` here doesn't change either; run
`fairshare install` again to ship a change, then `fairshare throttle` to see adopt's chats.

Tests run while that adopt governs this PC, including the test processes themselves; they must
pass that way. A test's own `adopt` always takes `--pid`, and a test never runs `install` without
`--hooks-only` (that would replace the PC's adopt).

## Rules

- Everything a command starts must end up in a job Windows kills when its owner exits
  (`killOnExitJob`, `Governor.NewUnit(true)`). `adopt` is the one exception: work it adopts must
  survive it stopping.
- Put processes in jobs through a `jobSet` (jobset.go), never `AssignProcessToJobObject` directly.
  A process already in a job can only join an empty job, and that nests the new job inside the old
  one. The Store PowerShell is always in its package's job.
- List processes with `snapshot()` / `scan()` (one NtQuerySystemInformation call), not Toolhelp,
  which makes the same call and copies it: 5.4 ms against 2.5 ms per look.
- Measure process CPU with `GetProcessTimes` (see `pidsCPU`), and time sub-millisecond work with
  `nowMs` (the performance counter). Go's own clock on Windows moves in ~0.5 ms steps.
- Windows calls go through `internal/windows`, never `golang.org/x/sys/windows`: that imports
  `net`, whose init starts Winsock, 3 ms of every hook call (`TestNoNetInBinary`).
- The hook runs on every agent tool call, so it never lists the whole PC (`snapshot`, 5 ms): it walks
  its own ancestors with `liveProc`. Claude Code starts it in exec form (`command` + `args`); a
  plain command line would go through Git Bash first, about 90 ms.

## Land

Land through a pull request to `main` on GitHub (`origin`) after the test line passes, then run
`fairshare install` from the merged build.
