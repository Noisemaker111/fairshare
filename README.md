# fairshare

Fairshare helps AI agent work yield to your apps on Windows. It manages the CPU priority of
builds, tests and other work, shares CPU time between chats, and can reuse identical checks.

## Install

Requires Windows, Git and Go 1.26.5 or newer. In PowerShell:

```powershell
git clone https://github.com/Noisemaker111/fairshare.git
cd fairshare
go build -o fairshare.exe .
.\fairshare.exe install
```

Installation copies the executable to `%LOCALAPPDATA%\fairshare\bin`, registers hooks for
Claude Code and Codex, installs the OpenCode plugin, and creates a scheduled task to run
`adopt`. It replaces existing agent-guard hooks and backs up files it changes as
`<file>.fairshare.bak`. Run `.\fairshare.exe install --uninstall` to remove Fairshare's
integration. See the commands below for monitoring and running individual jobs.

Fairshare records commands and activity in `%LOCALAPPDATA%\fairshare\events.jsonl` on your PC.
Review that file before sharing it.

## What it does

- **Yields to the foreground.** Every process of a piece of agent work goes into a Windows job at
  idle CPU priority, very low disk priority and very low memory priority. Windows enforces the CPU
  class on the whole process tree, so a child can't raise itself back. Your game and apps get
  priority while agent work uses available capacity.
- **Never leaves anything behind.** `run` puts the tree in a job Windows kills when fairshare
  exits, however it exits.
- **Never suspends anything unless asked to.** A suspended chat looks hung, so by default agent
  work is only lowered in priority. With `--freeze` (`run` or `adopt`), when free RAM or free
  commit falls below 6 GB, the youngest agent work on the PC freezes, one piece at a time; once
  it's back above 7.5 GB, the oldest frozen piece thaws. The oldest piece never freezes, so work
  always drains. Nothing is killed.
- **Runs a check once for every session.** `run --cache` keys on the exact content of the
  working tree (HEAD's tree plus every changed or new file, read without writing to the repo),
  the folder, the command and chosen environment variables. Not the worktree path: 100 worktrees
  on the same code share one result. A run already in progress is waited for and shared; a
  finished success is reused; a failure is only kept with `--cache-failures`.
- **Shares the CPU by chat, not by thread.** `adopt` keeps one lane per chat. With `--balance`,
  while a chat waits for the CPU, any chat taking well over an equal share has its threads
  dropped to the lowest priority, so a chat running 48 busy threads can't starve one running 2.
  It is off by default: on a busy PC it held some chat back most of the time. `fairshare
  throttle` caps one chat outright.
- **Every tool call goes through one thread shell.** The `hook` replaces agent-guard for Claude
  Code, Codex and OpenCode. It passes, rewrites, or sends back each call, and runs long tests and
  builds through `exec`, which keeps their output out of the chat, stops them before the agent's
  time limit, and stops a run that has hung.
- **Runs at most two whole suites at once on the PC.** A whole test suite, build or check (not
  one test file, one Go package or a `-run` filter) waits for one of two heavy-job slots shared by
  every chat and repo (`FAIRSHARE_HEAVY_SLOTS` changes the count). Without them, six chats each
  started a full suite and tests failed on their own time limits instead of on bugs. A slot is a
  lock file Windows frees when its holder exits, however it exits, and a waiting run names who
  holds each slot. `exec` takes one for foreground runs; the hook puts `fairshare slot` in front of
  background runs and runs that write to a file. Work started inside a slot never waits again.
- **Logs every data point.** One JSON line per tool call, long run, chat and `adopt` session goes
  to `%LOCALAPPDATA%\fairshare\events.jsonl`. `fairshare report` reads it back.

## Commands

```
fairshare adopt                          every chat's work becomes fair, no agent changes (install runs it always)
fairshare adopt --pid 1234               only the chat whose own process is 1234 (tests)
fairshare throttle                       the chats adopt holds: cores now, CPU so far, processes, caps
fairshare throttle 1234 10               cap the chat with process 1234 in it at 10% of the PC (off removes it)
fairshare install                        hook in Claude Code, Codex and OpenCode, and adopt always on
fairshare install --uninstall            take all of that back out
fairshare report --hours 24              what every chat ran, waited on, cost and wasted
fairshare run -- bun run test            one command (and all it starts) as fair agent work
fairshare run --cache -- bun run check   ...and once per code state across all sessions
fairshare run --normal -- strobe record …  a measurement at normal priority, outside the chat's lane (strobe
                                         refuses frame and input-latency metrics at idle priority), on a
                                         private desktop: the browser windows it opens are never on screen
                                         and never take a game's focus
fairshare probe --secs 10                how a game-like foreground app is doing right now
fairshare bench --jobs 50 --mode plain   50 heavy synthetic jobs against the probe (or --mode fair)
fairshare bench lanes                    a 48-thread chat against a 2-thread one, under each way of sharing
fairshare ceiling --what idle|spawn      how many idle processes / starts per second the PC takes
fairshare du <folders>                   real disk use, counting hardlinked files once
fairshare inspect                        the priorities and desktop Windows gave this process
fairshare slot bun run check             run it once a heavy-job slot is free (the hook adds this)
```

`fairshare install` makes `adopt` part of the PC rather than of one agent: a scheduled task (no
admin) runs one `adopt` for every chat, with no window, and brings it back within a minute of
logon or of it ending. A second `adopt` sees the first and exits. Installing again stops the
running one cleanly (it logs its chats) and starts the new copy.

A chat is any process T3 Code starts, whatever the provider: T3 starts each session (Claude Code,
Codex, OpenCode, anything else) as its own child, so `adopt` never needs to know a provider's
program name. T3's own git checks count as their own small chats. Outside T3, a chat is the nearest
`claude.exe`, `codex.exe` or `opencode.exe` (an agent started from a terminal); an agent that a T3
chat starts is that chat's work. The rule is in `chat.go`; the hook uses it too, so every call is
logged against its chat.

`adopt` puts each process started under a chat into that chat's lane; whatever it starts
afterwards is in the lane too. The chat's own process stays at normal priority, so the agent's own
loop stays quick. Work it adopted keeps its low priority if `adopt` stops.

A lane is a job named after the chat's harness process. A process that is already inside another
job gets a side job of its own instead, which Windows nests where the process already is:
- the Microsoft Store PowerShell, which every PowerShell command on this PC runs in;
- a test run that made its own job.

Only processes in no job go into the main job, so a test run's job closing can never take the
chat's other work with it. A side job allows breakaway: Claude Code and Codex start every shell in
a job that allows it, and Git Bash asks for it on every program it starts, so a side job that
forbade it would make those starts fail with "Permission denied". Twice a second, from the same process list it uses to find new work,
`adopt` measures each lane, applies throttles and (with `--balance`) holds back a hogging chat. That costs 2.5 ms
per look, four looks a second: about 1% of one core.

The `hook` and `adopt` run from `%LOCALAPPDATA%\fairshare\bin`, so rebuilding a checkout never
touches the copy every chat runs. OpenCode gets `opencode-plugin/` copied to
`~/.config/opencode/plugins/fairshare`, which it loads at start.

## Measured on Jon's PC (2026-10-28)

Ryzen 9 3900X, 64 GB. Other agent sessions were using 60–100% of the CPU throughout, so each
bench compares 5-second slices with its jobs running and frozen, in the order on, off, off, on.
Raw results are in `results/bench.jsonl`.

### After the bench fix (2026-10-29)

The first bench froze one of every three "running" slices by mistake (off, off suspended twice,
on resumed once), so the 2026-10-28 tables further down understate the harm in both modes. With
the fix, 50 always-busy jobs (`--kind math`, 4 threads each):

| | late frames, frozen → running | worst frame | input delay p99, frozen → running |
|---|---|---|---|
| plain (today) | 0.2% → 75.0% | 13953 ms | 1.2 → 220 ms |
| `run` | 1.0% → 6.4% | 111 ms | 3.8 → 6.6 ms |
| plain under `adopt` (default) | 0.7% → 4.8% | 80 ms | 1.7 → 5.9 ms |

### Calm PC, same conditions (2026-10-29 01:33–01:44)

Other sessions used 16–47% of the CPU. Same 50 always-busy jobs, same order of slices:

| | late frames, frozen → running | worst frame | input delay p99, frozen → running | time for 50 jobs |
|---|---|---|---|---|
| plain (today) | 0.0% → 15.8% | 2825 ms | 0.8 → 76.5 ms | 40 s |
| `adopt` (default), run 1 | 0.0% → 0.6% | 25 ms | 0.8 → 4.3 ms | 47 s |
| `adopt` (default), run 2 | 0.9% → 0.4% | 36 ms | 0.8 → 2.4 ms | 53 s |
| `adopt --feedback`, run 1 | 5.1% → 1.0% | 42 ms | 9.5 → 2.0 ms | 70 s |
| `adopt --feedback`, run 2 | 0.0% → 0.4% | 25 ms | 0.7 → 3.6 ms | 53 s |

Lag freezing adds nothing on a calm PC (run 1's 70 s did not repeat), so it stays opt-in.

### Real use: one chat under `adopt --pid` (2026-10-29)

8 full Go rebuilds (`go build -a`, own cache each) started from a real Claude Code chat, while a
stand-in game started outside that chat (by WMI, so `adopt` can't touch it) ran for 50 s. Other
chats kept the CPU at 76–97% busy throughout.

| | builds took | late frames | worst frame | frame p99 | input delay p99 |
|---|---|---|---|---|---|
| plain, run 1 | 21.4 s | 1.9% | 63 ms | 23.2 ms | 4.3 ms |
| plain, run 2 | 22.9 s | 3.9% | 89 ms | 34.5 ms | 7.4 ms |
| `adopt --pid`, run 1 | 45.5 s | 0.1% | 24 ms | 4.6 ms | 0.9 ms |
| `adopt --pid`, run 2 | 42.5 s | 0.0% | 18 ms | 7.6 ms | 1.1 ms |
| `adopt --pid`, run 3 | 46.0 s | 0.7% | 53 ms | 13.5 ms | 2.1 ms |

The game stays smooth, but the protected chat's builds take about twice as long: at idle
priority they wait behind the other chats' unprotected normal-priority work. Protecting one
session out of many penalizes that session, so turn `adopt` on for every session at once, not
one at a time.

Testing trap: `adopt --pid` also adopts the harness's shells that already exist, and adopted
work keeps its low priority after `adopt` stops. A "plain" run started from the same shell
afterwards is not plain; start each run from a fresh shell.

### Lanes: a 48-thread chat against a 2-thread chat (2026-10-29)

`fairshare bench lanes`: each chat in its own idle-priority job, 5 s per slice, in the order
shown and back. The share is how much of the 2 cores it asked for the light chat got.

| how the heavy chat is held back | light chat's share | heavy chat |
|---|---|---|
| not at all (Windows alone) | 28%, 30% | 22.2, 23.3 cores |
| equal CPU weight per job | 65%, 70% | 21.9, 22.6 cores |
| hard cap at 22 of 24 cores | 28–53% | 21.7–21.9 cores |
| light chat at below-normal priority | 100%, 100% | 20.8, 21.9 cores |
| heavy chat's threads at lowest priority | 97%, 96% | 20.1, 20.9 cores |

The last row is what `adopt --balance` does: every chat stays at idle priority, below the game, as
measured above. In the end-to-end test (`TestLanesGiveALightChatItsShare`, two fake chats under
`adopt`), the light chat got 98% of what it asked for, against 28% without `--balance`.

The thread shell's hook costs 19 ms a call, against agent-guard's 49 ms (node), on the same two
calls alternated 15 times each. Inside a chat it logs 5–6 ms of its own work.

### Before the bench fix (2026-10-28)

50 realistic jobs (JS work on 4 threads, 20 small-file reads per unit, a child process every 20
units):

| | late frames, jobs frozen | late frames, jobs running | worst frame | input delay p99 |
|---|---|---|---|---|
| plain (today) | 2.4% | 27.6% | 1281 ms | 63 ms |
| `run` | 5.0% | 4.7% | 69 ms | 11 ms |
| plain under `adopt` | 1.6% | 1.9% | 39 ms | 5 ms |

2 jobs shaped like the plant-fitting jobs (`--kind math`: 22 threads each of dense math that never
waits), while another session's real fitting jobs held the PC at 100%:

| | late frames, frozen → running | worst frame | input delay p99, frozen → running |
|---|---|---|---|
| plain | 59.9% → 55.7% | 1141 ms | 47 → 100 ms |
| `run` | 38.9% → 54.5% | 168 ms | 36 → 67 ms |

Fair removes the second-long freezes but did not remove all added delay for this kind of job. Not
yet known whether that is the background drifting or Windows' starvation boost (a thread at idle
priority that has waited ~4 s is briefly run above normal priority, so thousands of always-busy
idle threads add up). `adopt --feedback` also freezes the youngest agent work while a
normal-priority wake-up sensor runs late (95th percentile over 3 ms; thaws under 1.5 ms), the same
way memory freezing works. It is off by default until measured on a calm PC: the sensor can't
tell who causes the lag, so while unwrapped sessions hold the PC at 100% it freezes all wrapped
work but the oldest. A guard process thaws everything if `adopt` is killed while work is frozen.

50 sessions in 10 worktrees running the same check on the same code at once:

| | time | runs |
|---|---|---|
| no cache | 107 s | 50 |
| `--cache`, first time | 11.8 s | 1 (49 waited and shared it) |
| `--cache`, again | 5.6 s | 0 (50 reused) |

## Develop

See `AGENTS.md`.

## License

Public domain under [The Unlicense](UNLICENSE): use, change, and share it for any purpose.
