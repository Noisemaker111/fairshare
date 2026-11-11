# fairshare

fairshare says how much room a machine has for AI agent work, from what was measured on it, and
keeps for the person at the PC exactly the processors their own programs are using. It replaces
two fixed numbers: a cap on how many agent threads may be open, and a set count of processors
kept for the user.

A count of threads is wrong in both directions. Seven threads waiting on their models use under
two processors; five threads building Rust want every processor twice over. On 2026-11-10 a cap
of seven let in seven threads that together wanted 36 of the 16 processors they could use, and
on calmer nights the same cap refused an eighth thread on an idle PC.

## What it does

- **Measures.** On Windows a small meter looks at the PC every 5 seconds: how busy each logical
  processor is, how many threads want a processor and have none, free memory, free disk, what
  fills the screen in the foreground and when the keyboard was last used. On Linux the kernel
  already keeps those averages, so there is no meter and `fairshare reading` reads them. Both
  give the same reading in the same units: processors, GB of memory, GB of disk.
- **Answers one question: is there room?** `fairshare room` says yes while, with the new thread
  counted, the processors the work wants stay under the target share of the processors agent
  work may use, an eighth of the memory stays free and 15 GB of disk stays free. What the work
  wants is the processors in use plus the threads waiting for one, and the higher of the last
  minute and the last 10 minutes (5 on Linux), so neither a calm minute in a busy hour nor a calm
  hour before a busy minute lets a thread in. A new thread counts as the machine's own measured
  average per open thread, never under one processor and 2 GB, and threads let in during the last
  5 minutes still count at that estimate, because their load does not show yet.
- **Keeps for the user what the user is using.** The reserve is the most the user's own programs
  used in the last 3 minutes, times 1.5 (times 2 while something is full screen), in whole
  physical cores. It grows on the look that sees them use more or wait for a processor, and
  shrinks only after the whole window has been quieter. It remembers what each full-screen program
  needed the last time, so a game gets its processors on its first look, and what the desk needed
  before the user walked away, which is put back on the first key press. Nobody tells it anything.
  The least is one physical core and the most is half the machine. Owl holds agent work off the
  reserved processors; fairshare tells it the number for three minutes at a time and says it again
  every minute, so if the meter ever stops, Owl puts its own number back.
- **Says why.** A refusal names the numbers: processors in use, threads waiting, the limit and
  what set it, and the busiest programs. Every answer goes to `decisions.jsonl`.

It never runs per command and never touches another process. The first fairshare put a hook in
front of every agent command and every process in a Windows job; that cost 130 to 160 ms a call
and broke commands, and it was removed on 2026-11-08. That code is the tag `v1-final`.

## Install

Windows, Go 1.26.5 or newer:

```
go build -o fairshare.exe . && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o fairshare-linux .
.\fairshare.exe install
```

`install` copies both programs to `%LOCALAPPDATA%\fairshare\bin` and registers the scheduled task
`fairshare-meter`, which starts the meter with no window, within a minute of logon, and again
within a minute if it ever ends. No admin rights. `fairshare install --uninstall` stops the meter
and removes the task. On Linux there is nothing to install: copy `fairshare-linux` anywhere.

## Commands

```
fairshare status                 what the machine has, what work is taking, and whether a thread fits
fairshare room --threads 7 --checkout --take --label "<what>"
                                 the answer a launch asks for: exit 0 room, 1 no room, 2 cannot tell
fairshare room --reading <file>  the same rule on a reading from another machine
fairshare reading                this machine's reading as JSON
fairshare history --hours 24     how full it ran, how long work waited, the reserve unused
fairshare settings               the numbers in use and the file they come from
```

`room` exits 2 and prints `degraded: <cause>` when it cannot tell (the meter is not running, the
settings do not parse). A caller then falls back to whatever it did before and says so.

## Settings

`settings.json` in fairshare's home (`%LOCALAPPDATA%\fairshare`, or `~/.local/state/fairshare`),
read on every call. None of them is a count of threads.

| Key | Default | Meaning |
| --- | --- | --- |
| `cpu_target` | 0.90 | share of the agent processors filled while someone is at the PC |
| `cpu_target_full_screen` | 0.75 | that share while something fills the screen |
| `cpu_target_away` | 1.00 | that share with nobody at the PC, and on a machine with no screen |
| `away_minutes` | 10 | no keyboard or mouse for this long is away |
| `mem_reserve_fraction` | 0.125 | share of memory that always stays free |
| `disk_reserve_gb` | 15 | disk that always stays free where work is checked out |
| `thread_cores`, `thread_mem_gb`, `thread_disk_gb` | 1, 2, 5 | the least a new thread counts as |
| `settle_minutes` | 5 | how long a thread just let in counts at its estimate |
| `reserve_min_processors` | 2 | the least kept for the user |
| `reserve_max_share` | 0.5 | the most, as a share of the machine |
| `reserve_headroom`, `reserve_headroom_full_screen` | 1.5, 2 | room over what the user's programs used |
| `reserve_window_seconds` | 180 | how far back "used" looks |
| `reserved_processors` | unset | set it to pin the reserve and switch the measuring off |
| `sample_seconds` | 5 | how often the meter looks |

## What it measures on Windows, exactly

- Processors in use: each logical processor's idle time against its total, from one
  `NtQuerySystemInformation` call. Processors from the reserve up are the agents'.
- Threads waiting: every thread in the ready, standby or deferred-ready state, from the one call
  that lists every process.
- Agent work: processes at below-normal priority or lower, which is how Owl holds every agent
  process. The user's programs: everything else in the user's own session. A process that starts
  and ends between two looks is in the processor totals but in no program's line.
- Memory: free physical memory, or free commit when that is less; the least seen in the last
  minute. Disk: the home folder's drive.
- Full screen: what Windows reports for notifications, or a borderless window covering its whole
  monitor. A maximized window is not full screen.

## Measured on Jon's PC (2026-11-10, Ryzen 9 3900X, 24 logical processors, 64 GB)

| | the count that applied | what fairshare measured |
|---|---|---|
| 7 PC threads, 5 building Rust | 7 of 7: all let in | work wanted 36.0 of 16 processors: 15.3 in use, 20.7 waiting |
| The same PC 4 minutes later | 7 of 7: an eighth refused | 5.0 of 22 in use, none waiting: room |
| Boat host `t3-host-2`, a landing gate running | 0 of 4 threads: room | work wanted 28.1 of 8 processors |
| Processors kept for Jon, away 45 minutes | 8, fixed | 4, then 2: 22 of 24 for agent work |

The meter and its numbers agree with Windows' own per-processor counters to the percent. One look
costs two system calls; the hook that asks takes 20 ms on the PC and 0.7 s for a boat host.

## Develop

See `AGENTS.md`.

## License

Public domain under [The Unlicense](UNLICENSE): use, change, and share it for any purpose.
