# fairshare

A Go command for Windows and Linux that measures a machine and says whether it has room for more
agent work, and on Windows works out how many processors to keep for the person at the PC. See
`README.md`.

## Test

```
go vet ./... && GOOS=linux go vet ./... && go test ./... && go build -o fairshare.exe . && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o fairshare-linux .
```

Run it as written or with its output sent to a file. `go test ./... | tail` passes on tail's exit
code, so the `&&` after it builds anyway and a failing run looks green (2026-11-07).

`TestALookMeasuresThisPC` looks at the real PC with `FAIRSHARE_RESERVE=measure`, which works the
reserve out and never tells Owl. A test never runs `install`, and never runs a meter without
`FAIRSHARE_HOME` pointed at a temp folder: the meter for a home is one per home, and the real
home's meter is the one every launch asks.

To see it live: `fairshare status`, then the launch hook the way T3 runs it,
`echo '<a t3_thread_launch payload>' | python -I ~/.claude/hooks/thread-cap.py`. To see it fail:
`fairshare install --uninstall`, wait 20 s, and the same hook says `degraded:` and judges by its
old counts; `fairshare install` puts it back.

## Who asks it

- `~/.claude/hooks/thread-cap.py` (Claude Code and Codex, before a T3 thread starts) asks
  `fairshare room --take` for the PC and `boat-host.sh room` for a boat host. Its tests are
  `~/.claude/hooks/test_thread_cap.py`.
- `~/.agents/skills/boat-host/boat-host.sh up` copies `fairshare-linux` to each host as
  `~/.agents-pc/fairshare`.
- Owl applies the reserve: the meter runs `owl agents set reserve_processors <n> --for 3m
  --real-home` with `OWL_CALLER=fairshare`.

## Rules

- Nothing runs per agent command. The first version's per-command hook cost 130 to 160 ms a call
  and broke commands; a launch is the only moment fairshare is asked.
- fairshare reads and decides. It never sets a priority, puts a process in a job, suspends or
  ends anything. Owl's keeper does the holding; fairshare only tells it the reserve.
- No count of threads in the rule or the settings. A limit is a share of what a machine has, so
  one settings file is right on every machine.
- The room rule (`room.go`) and the reserve rule (`reserve.go`) are pure functions of a reading
  and the settings, and every number in them is tested there. Windows calls live only in
  `meter_windows.go`; Linux file reading only in `reading_linux.go`, with the parsing in
  `linuxproc.go` so it is tested on Windows too.
- When it cannot tell, it exits 2 with `degraded: <cause>`. It never answers from a stale reading.
- Windows calls go through `syscall`, never `golang.org/x/sys/windows`, which imports `net`.

## Land

Land through a pull request to `main` on GitHub (`origin`) after the test line passes, then run
`fairshare install` from the merged build and `boat-host.sh up` for each host that is running.
