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

The pool is checked end to end by `python -I scripts/pool-e2e.py fairshare.exe` (or
`fairshare-linux` on Linux): three machines in private homes, each reached through a command put
in front of the command as a real member is, with a real wake command, a real plan-hours command,
real git checkouts, and a stand-in for the `mesh` program that prints what mesh prints. On Linux a
hand-written reading comes from `FAIRSHARE_READING=<file>`, held to the same age limit as the
meter's; with it set no meter is started.

What is learned is checked end to end by `python -I scripts/learn-e2e.py fairshare.exe` (or
`fairshare-linux`): a real meter in a private home, processes that burn real processors, a real
run through the pool and, on Linux, a thread whose job detaches from it. Nothing in it is written
by hand, so it takes about a minute and wants a machine with two processors to spare.
`LEARN_E2E_BREAK=1` keeps the meter from starting, to see the checks fail.

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
  `~/.agents-pc/fairshare`. The meter running there from the old file ends by itself and the next
  question starts the new one; nothing has to restart it.
- Anything that wants a machine without naming one asks the pool: `fairshare pool place` for a
  launch, `fairshare pool run` for a gate or a build. The pool is `pool.json` in the home of the
  machine that places (the PC); each member is reached through its `via` (`boat ssh <id>`,
  `mesh exec <device>`), and the far end of every via is `fairshare pool here` on that machine.
- Owl applies the reserve: the meter runs `owl agents set reserve_processors <n> --for 3m
  --real-home` with `OWL_CALLER=fairshare`.
- Owl holds older sessions back for an urgent launch: `owl agents set yield_before <unix seconds>
  --for 3m --real-home` from `room` when it lets one in and from the meter every minute while the
  hold stands (`urgent.go`), `owl agents unset yield_before` when it ends. The hook passes `--repo`
  (the project's folder name) so a launch counts as what that repo's threads were measured using
  (`learn.go`). A caller that passes `--cores` or `--threads` loses nothing by dropping them.

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
- No size of a piece of work is written down anywhere: not per repo, not per command, not in a
  caller. What work takes comes from `learned.json`, which only the meter writes, from minutes it
  measured (`learn.go`); which process is which work is `work.go`. Both are the same code on
  Windows and Linux and are fed by each meter's look. A caller's `--cores` is a starting guess
  that measured minutes replace, never an override.
- A piece of work is counted three ways in turn and never two at once: let in and not yet seen
  (its grant), open (the meter's `work` in the reading), ended (nothing). A change that makes
  work count must say which of the three it is.
- The pool decides nothing about a machine. Each member answers `fairshare room` for itself and
  the pool compares; a rule about one machine's room goes in `room.go`, never in `pool.go`.
- Everything sent to a member is one word of hex (`pool here <word>`): boat, SSH and mesh each
  split and quote a command differently, and a member is never a different code path per carrier.
- A machine that costs plan-hours is woken only when no awake machine has room, and never when
  its hours cannot be read.
- When it cannot tell, it exits 2 with `degraded: <cause>`. It never answers from a stale reading.
- Windows calls go through `syscall`, never `golang.org/x/sys/windows`, which imports `net`.

## Land

Land through a pull request to `main` on GitHub (`origin`) after the test line passes, then run
`fairshare install` from the merged build and `boat-host.sh up` for each host that is running.
