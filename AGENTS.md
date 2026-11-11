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

The line is checked end to end by `python -I scripts/queue-e2e.py fairshare.exe`: the built
program in a private home, with real files, the real lock and a real notify command, on readings
written by hand so "full" and "room" are certain whatever the PC is doing.

To see it live: `fairshare status`, then the launch hook the way T3 runs it,
`echo '<a t3_thread_launch payload>' | python -I ~/.claude/hooks/thread-cap.py`. To see it fail:
`fairshare install --uninstall`, wait 20 s, and the same hook says `degraded:` and judges by its
old counts; `fairshare install` puts it back.

## Who asks it

- `~/.claude/hooks/thread-cap.py` (Claude Code and Codex, before a T3 thread starts) decides
  where a launch runs through `~/.claude/hooks/placement.py`: the PC (`fairshare room`) or any
  boat host that has the repo (`boat-host.sh --conf <dir> room`), whichever has room, and the
  line (`fairshare queue add --machines ...`) when none has. `place-on-host.py` moves the launch
  to the host that was picked. Tests: `test_thread_cap.py`, `test_placement.py`,
  `test_place_on_host.py` in the same folder.
- `~/.claude/hooks/fairshare-place.py` is this PC's `queue_place` (the same choice for a launch
  that waited) and `fairshare-order.py` its `queue_order`: the launches the manager lists in
  `~/.config/t3-manager/goals.json` "pending" go first, in that list's order.
- `~/.claude/hooks/fairshare-offer.py` is this PC's `queue_notify`: it puts the notice in the
  manager's ping outbox (`notify-manager.py`), so a busy thread gets it when its turn ends.
- `~/.agents/skills/boat-host/boat-host.sh up` copies `fairshare-linux` to each host as
  `~/.agents-pc/fairshare`.
- Owl applies the reserve: the meter runs `owl agents set reserve_processors <n> --for 3m
  --real-home` with `OWL_CALLER=fairshare`.
- Owl holds older sessions back for an urgent launch: `owl agents set yield_before <unix seconds>
  --for 3m --real-home` from `room` when it lets one in and from the meter every minute while the
  hold stands (`urgent.go`), `owl agents unset yield_before` when it ends. The hook passes `--repo`
  (the project's folder name) so a launch counts as its repo's measured sessions (`repos.go`).

## Rules

- Nothing runs per agent command. The first version's per-command hook cost 130 to 160 ms a call
  and broke commands; a launch is the only moment fairshare is asked.
- fairshare reads and decides. It never sets a priority (an urgent hold is Owl's keeper's, told), puts a process in a job, suspends or
  ends anything. Owl's keeper does the holding; fairshare only tells it the reserve.
- fairshare never starts a thread. The line holds a launch's place and its room; the thread that
  asked sends the identical launch again, so every launch still passes every launch hook.
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
