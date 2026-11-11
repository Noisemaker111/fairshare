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
  processor is, how many threads waited while they were all busy, free memory, free disk, what
  fills the screen in the foreground and when the keyboard was last used. On Linux there is no
  meter: `fairshare reading` reads the kernel's processor-time counters against the marks its
  earlier runs left (and for one second on a first run), and the kernel's memory numbers. Both
  give the same reading in the same units: processors, GB of memory, GB of disk.
- **Answers one question: is there room?** `fairshare room` says yes while, with the new thread
  counted, the processors in use stay under the target share of the processors agent work may
  use, an eighth of the memory stays free and 15 GB of disk stays free. Processors in use is the
  higher of the last minute and the last 10 minutes, so neither a calm minute in a busy hour nor
  a calm hour before a busy minute lets a thread in. Threads that are ready to run are not added
  to it: work that is short of processors keeps them all busy, and a ready thread beside an idle
  processor is not short of one. (Until 2026-11-11 they were added, and launches were refused
  for half an hour at 13 of 22 processors in use, because bursts of compiler jobs averaged in as
  38 more wanted. On Linux the load average was used, which also counts tasks waiting on a disk.)
  How many waited while the processors were full is still shown, to say how far over a full
  machine is. A new thread counts as the machine's own measured
  average per open thread, never under one processor and 2 GB, and threads let in during the last
  5 minutes still count at that estimate, because their load does not show yet.
- **Puts urgent work first.** A launch with `--urgent`, or whose title holds one of
  `urgent_words` ("production", "hotfix", "outage", ...), is judged against the work that would not
  step aside for it: the user's own and Windows' on the agent processors, and agent sessions started
  since an urgent hold began. When it is let in, Owl's keeper holds every agent session older than it
  at idle priority (its `yield_before`, told for three minutes and again every minute), so Windows
  runs the urgent session first and the older work on what it leaves. Nothing is paused or ended.
  The hold stands for 20 minutes, then while a session started since is running, and never past
  two hours. The processors kept for the user stay theirs: urgent work is held off them like all
  agent work. Why: on 2026-11-11 a production fix for AppForGutters was refused room because two Owl
  Rust ports' gates used 17 of the 18 agent processors, and had to go to a host that cannot deploy.
- **Counts a thread as its repo's own.** The meter adds up, each minute, what every agent
  session's process tree used, under the repo its runner works in (the runner's current folder:
  `.t3\worktrees\<repo>\...`). A launch with `--repo` counts as that repo's average over the last
  7 days once it has 30 session-minutes, instead of the machine's average per open thread: an Owl
  thread compiling Rust measured 4.5 processors, an AppForGutters thread about 1. `fairshare repos`
  lists them.
- **Keeps processors for the user only while a game runs.** Jon's rule (2026-11-11): two
  processors kept (`reserve_min_processors`) so the desktop never fails, unless a game runs. A game
  is a program full screen (not a browser or player named in `not_games`), a program named in
  `games`, or one seen full screen before. While one runs, the reserve is the most the game's own
  processes used in the last 3 minutes, times 2, in whole physical cores, plus a core while its
  threads wait for a processor; it remembers what each game needed last time, so it gets its
  processors on its first look. Windows' own background work (dwm, Defender, the search indexer)
  is not the user and sizes nothing: on 2026-11-11 counting it kept 6 with nobody at the PC. The
  most is half the machine. Owl holds agent work off the reserved processors; fairshare tells it
  the number for three minutes at a time and again every minute, so if the meter ever stops, Owl
  puts its own number back.
- **Keeps the line.** A launch that finds no room is not lost. With `--queue` it waits in
  fairshare's line with its whole request, in the order it came. After every look the meter checks
  the front of the line; when the machine has room for it, that room is held for 10 minutes and the
  thread that asked is told, and the same launch sent again goes straight in. The line is strict:
  the first launch that does not fit stops it, and a new arrival passes it only when there is room
  for everyone waiting too. An offer nobody takes goes to the back, and leaves after six. fairshare
  does not start the thread itself: a launch passes through hooks that place it and pick its model,
  and the command line that starts threads cannot say everything a launch can (an existing
  worktree, a model's effort, who launched it), so the thread that asked sends the identical
  request.
- **Says why.** A refusal names the numbers: processors in use, threads waiting, the limit and
  what set it, and the busiest programs. Every answer goes to `decisions.jsonl`.

It never runs per command and never touches another process. Which processors agent work runs
on is Owl's alone to set: its keeper holds every agent process on exactly the processors from
the reserve up, wider as well as narrower (owl-go #1138, 2026-11-11). Before that it only
narrowed, so once the reserve followed the user, agent sessions alive while a game ran stayed
on 12 of 24 processors; for a few hours the meter widened them itself, and that code is gone.
If agent work is again short of processors beside idle ones, look there first. The first fairshare put a hook in
front of every agent command and every process in a Windows job; that cost 130 to 160 ms a call
and broke commands, and it was removed on 2026-11-08. That code is the tag `v1-final`.

## Install

Windows, Go 1.26.5 or newer:

```
go build -o fairshare.exe . && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o fairshare-linux .
.\fairshare.exe install
```

`install` copies both programs to `%LOCALAPPDATA%\fairshare\bin`, puts a `fairshare` starter in
`~/bin` and `~/.local/bin` when they exist (so it is a command in Git Bash, cmd and PowerShell),
and registers the scheduled task
`fairshare-meter`, which starts the meter with no window, within a minute of logon, and again
within a minute if it ever ends. No admin rights. `fairshare install --uninstall` stops the meter
and removes the task. On Linux there is nothing to install: copy `fairshare-linux` anywhere.

## Commands

```
fairshare status                 what the machine has, what work is taking, and whether a thread fits
fairshare room --threads 7 --checkout --take --label "<what>" [--repo <name>] [--urgent]
                                 the answer a launch asks for: exit 0 room, 1 no room, 2 cannot tell
fairshare room ... --queue --key <k> --launcher <thread> --request <file|->
                                 no room: wait in the line; the launcher is told when its turn comes
fairshare queue                  the line, in order; `queue drop <id>` takes a launch out
fairshare room --reading <file>  the same rule on a reading from another machine
fairshare reading                this machine's reading as JSON
fairshare history --hours 24     how full it ran, how long work waited, the reserve unused
fairshare repos                  what a session of each repo was measured using
fairshare pool                   every machine that joined, as one place (see The pool)
fairshare settings               the numbers in use and the file they come from
```

`room` exits 2 and prints `degraded: <cause>` when it cannot tell (the meter is not running, the
settings do not parse). A caller then falls back to whatever it did before and says so.

## The pool

Every machine that joined is one place to run work. A piece of work says what it is (a repo, what
it needs), never which machine, and the pool puts it where there is room.

```
fairshare pool                   the machines as one: what they have, what is in use, where a thread fits
fairshare pool join              this machine joins
fairshare pool join --name t3-host-2 --via "boat ssh <id>" --wake "boat resume <id>" --work /home/user/work
                                 another machine joins: --via is the words that run a command on it
fairshare pool join --mesh       every device of a Hona/mesh mesh joins, and the pool follows the mesh
fairshare pool place --repo owl-go --take --json
                                 where a thread goes: exit 0 and the machine, 1 no machine takes it, 2 cannot tell
fairshare pool run --repo appforgutters --commit <sha> -- bash -c "bun install && bun run gate"
                                 a gate or a build, run where there is room, on exactly that commit
```

- **Each machine answers for itself.** The pool asks every member `fairshare room`, the question a
  launch already asks one machine, and compares the answers. The processors kept for the person at
  the PC are out of the pool before the pool sees them, and a machine's own settings, line and
  measured repos decide its answer.
- **The most room wins.** Of the machines that have room, the work goes to the one with the most
  processors left once it is counted. `--take` counts it there until its load shows, and the
  answer that takes it is the one that decides, so two launches at once cannot both be told yes
  on room for one.
- **Plan-hours.** A machine with a `wake` command that does not answer is asleep. It is woken
  only when no machine that is awake has room, and when it has an `hours` command, only while
  that prints more plan-hours left than the member's `keep_hours`. Hours that cannot be read are
  never spent.
- **What work needs.** `--needs windows,deploy` keeps work on machines that are that OS or were
  joined with `--can deploy`; `--among a,b` on the machines named. A run in a repo goes only to a
  machine that has the repo's folder, checked on the machine at that moment.
- **A run.** `pool run` runs the command in the repo's folder on the machine picked, through that
  machine's own fairshare, and its exit is the command's (69 when the pool could not run it).
  With `--commit` it runs in a fresh checkout of exactly that commit, which is removed after; the
  folder's own checkout is not touched. On a member with a `busy_dir` it leaves its process id
  there while it runs, so the machine's idle watch does not put it to sleep under the run.

The pool is `pool.json` in the home of the machine that places work. A member:

| Field | Meaning |
| --- | --- |
| `name` | what the pool calls it |
| `via` | the words that run a command on it, put in front of the command; none for this machine |
| `fairshare` | the fairshare program there; unset, `fairshare` on its PATH |
| `wake` | run on this machine when it does not answer; without it a silent machine is skipped |
| `awake`, `awake_match` | run on this machine before anything is sent: up when it exits 0 and prints what the pattern finds. For a carrier that wakes a sleeping machine just by being used |
| `hours`, `keep_hours` | prints its plan-hours left (a number or `{"left": n}`); it is woken only above `keep_hours` |
| `can` | what it can do beyond its OS, for `--needs` |
| `work`, `repos` | the folder that holds its repos, and folders for repos kept somewhere else |
| `busy_dir` | a folder its idle watch reads for running work |
| `extra` | handed back with a placement, for the caller (a T3 environment id) |

`ask_seconds` (20) is how long a machine has to answer and `wake_seconds` (280) how long a wake
may take.

### Hona/mesh

[mesh](https://github.com/Hona/mesh) runs a command on a named device of yours in about 35 ms,
over SSH on Tailscale, with the devices listed in a private GitHub repo. That makes it a `via`:
`mesh exec <device>`. It is not a pool by itself. It measures nothing, keeps no line, and every
command names its device; the choosing is fairshare's. `fairshare pool join --mesh` makes every
device of the mesh a member and looks again on every pool command, so a machine that runs
`mesh up` is in the pool from then on with nothing else to do, and one that leaves the mesh
leaves the pool.

What it needs and cannot do (read from its SPEC and tried with 0.1.0-dev.30 on 2026-11-11):

- Every device needs Tailscale, an SSH server, `gh` and opencode. `mesh up` stops at
  `tailscale not found` without the first. It sets up opencode (its MCP entry, its `AGENTS.md`),
  not T3, Claude Code or Codex.
- It cannot wake a machine: a device that is offline is skipped. A boat host that sleeps still
  needs its `wake` command, run from a machine signed in to boat.
- It has no license, so none of its code is in fairshare; fairshare runs the `mesh` program.
  It is a development build with no tests by its own rule.
- `mesh exec` ends a command that prints nothing and sits idle for 20 s, so the pool always
  passes `--no-stall`.

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
| `reserve_headroom_full_screen` | 2 | room over what a running game's own processes used (`reserve_headroom` is no longer used) |
| `games`, `not_games` | 7daystodie.exe; browsers, players, T3, Owl | programs that are games while they run, and programs that are never games even full screen |
| `reserve_window_seconds` | 180 | how far back "used" looks |
| `reserved_processors` | unset | set it to pin the reserve and switch the measuring off |
| `sample_seconds` | 5 | how often the meter looks |
| `queue_notify` | unset | the command that tells a thread its launch has room (one JSON notice on stdin) |
| `queue_place` | unset | the command that picks a machine for a waiting launch that can run on more than this one (`{"entry", "local", "usable"}` on stdin, `{"machine": "<name>"}` on stdout) |
| `queue_order` | unset | the command that says which waiting launches go first (the line on stdin, a JSON list of ids, front first, on stdout) |
| `queue_place_seconds` | 30 | how often, at most, those two are asked |
| `queue_hold_minutes` | 10 | how long room is held for a launch that was told |
| `queue_offers`, `queue_max_hours` | 6, 24 | untaken offers, and hours in the line, before a launch leaves it |
| `urgent_words` | urgent, production, prod fix, hotfix, outage | whole words in a title or repo that make a launch urgent |
| `urgent_least_minutes`, `urgent_most_minutes` | 20, 120 | how long an urgent hold stands at least, and at most after the latest urgent launch |
| `runners` | claude.exe, codex.exe, opencode.exe | the programs that are one agent session each (Windows) |
| `repo_days`, `repo_min_minutes` | 7, 30 | the days a repo's estimate averages, and the session-minutes it needs first |

## What it measures on Windows, exactly

- Processors in use: each logical processor's idle time against its total, from one
  `NtQuerySystemInformation` call. Processors from the reserve up are the agents'.
- Threads waiting: the threads in the ready, standby or deferred-ready state at a look, from the
  one call that lists every process, counted only when 90% or more of the processors they may
  use were busy over that look. Shown, and kept in the history; not part of the room decision.
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
