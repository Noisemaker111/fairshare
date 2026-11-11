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
  fills the screen in the foreground and when the keyboard was last used. On Linux the machine's
  own numbers come from the kernel: `fairshare reading` reads its processor-time counters against
  the marks its earlier runs left (and for one second on a first run), and its memory numbers.
  Both give the same reading in the same units: processors, GB of memory, GB of disk. On both, a
  meter also measures which work uses what (below); on Linux the first question a machine is
  asked starts it.
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
  machine is.
- **Learns what work uses, and sizes nothing by a number somebody chose.** Every look, each
  process is put under the piece of work it belongs to: a thread (an agent runner and everything
  it started), a run (a command the pool ran here), or a job (work in a repo's worktree that no
  thread on the machine owns). Each piece's minutes are recorded, and a new piece counts as what
  work like it was measured using, the nearest kind first: a thread as its repo's threads on this
  machine, then every thread here; a run as that command in that repo, then the repo's runs, then
  every run; and, while this machine has measured little, as what another machine of the pool
  measured for it. Ten measured minutes, or one finished run, weigh as much as everything wider;
  minutes fade by half every three days, so a repo that changed is re-learned. `fairshare
  learned` lists it. Only where nothing like the work was ever measured anywhere does it start
  at the settings' floor (1 processor, 2 GB), or at its caller's `--cores`; both stop mattering as
  soon as there are minutes. Why: until 2026-11-11 a thread counted as one average per repo
  (appforgutters 1, owl-go 4.5) and a gate as `--cores 4` typed by its caller. Jon: "every thread
  and every process is different".
- **Follows work that leaves its thread.** A process stays with the piece of work it started
  under after the processes between them are gone, and one found with nothing above it is placed
  by the T3 thread it was started for (Linux) or the folder it runs in. That is most of the load:
  measured on a boat host on 2026-11-11, six threads' runners and their living children used
  0.05 processors while the machine ran at 3.2, because the land a thread starts in the
  background outlives the shell that started it. Before this, none of that counted as any
  thread's.
- **Counts open work as what it is expected to use, not what it uses this minute.** Six threads
  waiting on their models before each runs a gate are not an idle machine. Each open piece is
  expected to use what its own minutes say (the recent ones most), and until it has enough of
  them, what work like it used. What counts as in use is the higher of the processors busy now
  and that expectation. And because open work does not all peak in the same minute, room is kept
  for its swing taken together (the spreads of the pieces add as squares), not for every piece's
  peak: a steady build needs its average, one bursty thread needs well over its average, ten
  bursty threads need far less than ten peaks.
- **Judges memory the same way, not by adding up peaks.** A kind of work is learned as three
  numbers: what a minute of it holds on average, how far its minutes stray from that, and the
  most one minute held (`fairshare learned`: GB, +/-, MOST). An open piece is expected to hold
  what its own minutes held, the recent ones most. What counts as taken is the memory in use,
  plus whatever the open work is expected to hold beyond what it holds now, plus room kept once
  for it to hold more. That room is the larger of two things: how far the one piece that reaches
  furthest goes above what it is expected to hold, less the memory the machine keeps free anyway
  (`mem_reserve_fraction`); and `learn_mem_margin` times the swing of the pieces taken together
  (how many are high in the same minute is chance); and never more than every piece at its most
  at once. So two things always hold: what work is expected to hold, with its ordinary swing,
  leaves the reserve free; and the most one piece was ever seen holding still fits in the
  machine. That most is its rarest minute, and a rare minute is what a reserve is for.
  A new piece adds what it is expected to hold and whatever more has to be kept once it is one
  of them. Several pieces started together count as one piece that many times the size: they
  are one command and reach their most together. Why: until 2026-11-11 every open thread
  counted as the most it might reach, added up, and a new thread as the most any thread ever
  held. Jon's PC refused launches with 36 of 64 GB free ("a new thread counts as 21.1 GB on top
  of 36.5 GB that work let in or open here is still expected to take") while its seven threads
  held 0.5 to 6 GB each. Later that night it refused again with 35 of 64 GB free and 4 of 22
  processors in use, a 1 GB thread too: one Owl thread four minutes old might build Rust at 25
  GB, as one minute in 2,500 had, and 16 to 24 GB was kept for that minute on top of the 8 GB
  reserve, two margins against the same event. That evening's launch now counts as 2.9 GB on
  top of 12.2, 43 of the 56 GB that may be taken; the night's as 9.1 on top of 12.7, 51 of 56.
  With 26 GB free that night it is still refused, because 25 GB more would not fit in the PC.
- **Counts a launch only until it is seen running.** Work let in a moment ago counts at its
  estimate until the meter sees it (a new piece of its kind and repo), then as itself. A run
  counts for exactly as long as it runs. Work that never shows up stops counting after 5
  minutes (`settle_minutes`), which is also all a machine with no meter running has.
- **Puts urgent work first.** A launch with `--urgent`, or whose title holds one of
  `urgent_words` ("production", "hotfix", "outage", ...), is judged against the work that would not
  step aside for it: the user's own and Windows' on the agent processors, and agent sessions started
  since an urgent hold began. When it is let in, Owl's keeper holds every agent session older than it
  at idle priority (its `yield_before`, told for three minutes and again every minute), so Windows
  runs the urgent session first and the older work on what it leaves. Nothing is paused or ended.
  The hold stands for 20 minutes, then while work of the repo the urgent launch was for, begun
  since, is still running, and never past two hours. The processors kept for the user stay theirs:
  urgent work is held off them like all agent work. Why: on 2026-11-11 a production fix for
  AppForGutters was refused room because two Owl Rust ports' gates used 17 of the 18 agent
  processors, and had to go to a host that cannot deploy.
  A hold exists only once Owl has taken it. An Owl without the setting answers with an error and
  exit 0; fairshare reads the answer, says `degraded:` and records no hold. Why: the same
  afternoon the PC's Owl did not have `yield_before` yet (owl-go #1161 was still open), a hold
  for a deploy stood 45 minutes with nothing held, and any agent session started since kept it
  standing, whoever's it was.
- **Keeps the desktop for the person at it.** Work that opens windows on the screen, moves the
  pointer or captures it says so (`fairshare pool run --needs desktop -- <command>`, or
  `fairshare room --desktop`) and starts only while nobody is at the PC, one piece at a time; a
  run waits for that with `--wait <minutes>`. Work already started is never stopped. The meter
  also times Windows' window manager (how long a frame of the desktop takes to draw: `screen` in
  `fairshare status`, `frame_ms` and `frame_worst_ms` in the reading and the history), so a
  desktop that stalls shows as that and not as a guess about processors. Why: on 2026-11-11 the
  PC lagged about five seconds at a time while Jon worked, with processors, memory and disk all
  showing room. Three gates of Owl's port were driving fixture windows, dragging and capturing
  the screen on his desktop at once; the window manager stalled 25 times in 100 seconds, 0.2 to
  2 seconds each, and the window he was in sat blocked waiting on it. Keeping 8 processors for
  him instead of 2 changed nothing; stopping those tests ended it within the minute.
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
and removes the task. On Linux there is nothing to install: copy `fairshare-linux` anywhere. The
first question it is asked starts its meter, apart from the command that asked; the meter ends
when the file it was started from is replaced, so a new build takes over at the next question.
`fairshare install --uninstall` stops it, and `FAIRSHARE_METER=off` keeps it from starting.

## Commands

```
fairshare status                 what the machine has, what work is taking, and whether a thread fits
fairshare room --checkout --take --label "<what>" [--repo <name>] [--urgent]
                                 the answer a launch asks for: exit 0 room, 1 no room, 2 cannot tell
fairshare room --kind run --what "<command>" --repo <name>
                                 the same for a run of a command (the pool asks this)
fairshare room ... --desktop     the work drives the desktop: room only while nobody is at the PC, one at a time
fairshare room ... --queue --key <k> --launcher <thread> --request <file|->
                                 no room: wait in the line; the launcher is told when its turn comes
fairshare queue                  the line, in order; `queue drop <id>` takes a launch out
fairshare room --reading <file>  the same rule on a reading from another machine
fairshare reading                this machine's reading as JSON
fairshare history --hours 24     how full it ran, how long work waited, the reserve unused
fairshare learned                what each kind of work was measured using: threads by repo, runs by command
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
fairshare pool place --repo owl-go --folder --take --no-wake --key K --launcher T --request - --json
                                 a launch: it waits in the line when no machine has room, and goes to the
                                 machine held for it when it is sent again
fairshare pool wake t3-host-2    wake a machine that `--no-wake` named asleep
fairshare pool run --repo appforgutters --commit <sha> -- bash -c "bun install && bun run gate"
                                 a gate or a build, run where there is room, on exactly that commit
```

- **Each machine answers for itself.** The pool asks every member `fairshare room`, the question a
  launch already asks one machine, and compares the answers. The processors kept for the person at
  the PC are out of the pool before the pool sees them, and a machine's own settings, line and
  measured work decide its answer.
- **The most room wins.** Of the machines that have room, the work goes to the one with the most
  processors left once it is counted. `--take` counts it there until it is seen running, and the
  answer that takes it is the one that decides, so two launches at once cannot both be told yes
  on room for one.
- **Each machine sizes the work, and the pool passes the news.** The same gate is not the same
  load on a 24-processor PC and an 8-processor host, so each machine counts work as what it
  measured itself. The machine that measured a piece of work most is the one the others are told
  about (`seen.json` on the machine that places), so a machine that just joined does not count a
  gate it has never run as next to nothing; its own minutes take over as it gets them. A
  placement's answer says what the work counts as on each machine and what that came from.
  `--cores` and `--mem-gb` are a caller's guess, used only where nothing like the work was ever
  measured.
- **Plan-hours.** A machine with a `wake` command that does not answer is asleep. It is woken
  only when no machine that is awake has room, and when it has an `hours` command, only while
  that prints more plan-hours left than are kept. How many are kept is the member's `keep_hours`,
  or more when the command says so itself (`{"left": n, "keep": k, "why": "..."}`): a plan that
  has to last to its reset keeps more on a day that used much, and only the command knows the
  plan. `fairshare pool` shows a machine that may not be woken now. Hours that cannot be read are
  never spent.
- **What work needs.** `--needs windows,deploy` keeps work on machines that are that OS or were
  joined with `--can deploy`; `--among a,b` on the machines named; `--where env=<id>` on the
  machine whose `extra` holds that, which is how a caller says "that machine" without knowing any
  machine's name. A run in a repo, and a launch placed with `--folder`, goes only to a machine
  that has the repo's folder, checked on the machine at that moment.
- **A launch can wait.** `pool place` with `--key`, `--launcher` and `--request` is a launch: when
  no machine has room it joins the line (`fairshare queue`) for the machines open to it, and it
  does not pass a launch already waiting for the same machine unless it is urgent. The meter asks
  the pool for the front of the line (no `queue_place` command is needed), holds the room on the
  machine that has it, counts it there, and tells the launcher; the same launch sent again goes
  to that machine. A machine held asleep is woken by `pool place`, or with `--no-wake` named for
  the caller, who wakes it with `fairshare pool wake <name>`. Why: until 2026-11-11 a hook did
  all of this, asked each host by its folder and knew every host by name.
- **A run.** `pool run` runs the command in the repo's folder on the machine picked, through that
  machine's own fairshare, and its exit is the command's (69 when the pool could not run it).
  The command is the work: it is learned under its own words (without the shell in front and the
  commit ids and long numbers that change from run to run), with how long it lasts, and it counts
  on its machine for as long as it runs. A placement that is run by its caller says so with
  `pool place --run "<command>"`. `--as "<name>"` names a run whose words are a wrapper's (a
  script sent with every call). `--placed <file>` writes where it went before it runs, for a
  caller that has to fetch what the run left there. `--elsewhere` is for a caller that can run
  the command here itself: when this machine has the most room nothing is run and the exit is 70.
  `--quiet N` and `--most N` give a run up when it has printed nothing for N minutes or has run
  for N minutes: the machine that runs it keeps the time, ends it and everything it started, and
  says why (exit 69), so a run nobody is listening to any more still ends; the placing machine
  waits 90 seconds longer for the case where nothing comes back at all. Why: on 2026-11-11 a
  landing gate on a starved boat host ran 38.9 minutes and ended with no verdict.
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
| `hours`, `keep_hours` | prints its plan-hours left (a number, `{"left": n}`, or `{"left": n, "keep": k, "why": "..."}`); it is woken only while more are left than are kept |
| `can` | what it can do beyond its OS, for `--needs` |
| `work`, `repos` | the folder that holds its repos, and folders for repos kept somewhere else |
| `busy_dir` | a folder its idle watch reads for running work |
| `extra` | handed back with a placement, for the caller (a T3 environment id, a project id per repo), and what `--where` matches |

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
| `thread_cores`, `thread_mem_gb` | 1, 2 | what a piece of work counts as where nothing like it was ever measured |
| `thread_disk_gb` | 5 | what a checkout counts as |
| `settle_minutes` | 5 | how long work let in counts at its estimate when it is never seen running |
| `learn_trust_minutes`, `learn_trust_runs` | 10, 1 | the measured minutes, and the finished runs, of a kind of work that weigh as much as everything wider known about it |
| `learn_half_life_days` | 3 | measured minutes count for half after this long |
| `learn_recent_minutes` | 30 | the same for a running piece of work's own minutes: what it did lately says most about what it does next |
| `learn_margin` | 1 | how much of the open work's swing, taken together, is kept free; 0 keeps none |
| `learn_mem_margin` | 2 | the same for memory, and more: work short of processors runs slower, work short of memory is ended. 0 keeps only how far one piece reaches |
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
| `runners` | claude.exe, codex.exe, opencode.exe | the programs that are one agent thread each, named with or without `.exe` |

`repo_days` and `repo_min_minutes` are no longer used; a settings file that still has them is
read as before.

## What it measures on Windows, exactly

- Processors in use: each logical processor's idle time against its total, from one
  `NtQuerySystemInformation` call. Processors from the reserve up are the agents'.
- Threads waiting: the threads in the ready, standby or deferred-ready state at a look, from the
  one call that lists every process, counted only when 90% or more of the processors they may
  use were busy over that look. Shown, and kept in the history; not part of the room decision.
- Agent work: processes at below-normal priority or lower, which is how Owl holds every agent
  process. The user's programs: everything else in the user's own session.
- What a process used: its processor time at each look against the look before. The meter keeps
  a handle on every process from the first look that sees it (one that can only read its
  times), so when the process ends, what it used after the last look and the moment it ended
  are still read: a run is measured to its last second. A process that begins and ends between
  two looks is seen by none; its time is in the processor totals, and is shared among the agent
  processes that were seen beginning or ending in that look, equally, so it lands on the work
  that starts short processes. That share is the one estimate in the meter. Measured on
  2026-11-11 with a look every 5 s: 68 to 73% of the processor time in use was in processes
  alive at a look, 3 to 4% was the last seconds of processes that had just ended, and 23 to 29%
  was never seen (compiler jobs, test binaries, git); looking four times a second still missed
  13% and cost 4% of a processor.
- The desktop: how long Windows' window manager takes to draw a frame (`DwmFlush`, called from
  the meter's own process five times a second), while someone is at the PC. About one refresh
  of the screen when the desktop is well; seconds when something stalls it.
- Which work: a process belongs to the nearest runner or run above it by its parents, and keeps
  that after they are gone. An agent process with nothing above it is placed by the folder it
  runs in (read from its process parameters, once).
- Memory: free physical memory, or free commit when that is less; the least seen in the last
  minute. Disk: the home folder's drive.
- Full screen: what Windows reports for notifications, or a borderless window covering its whole
  monitor. A maximized window is not full screen.

## What it measures on Linux, exactly

- The machine: processors in use from `/proc/stat` between two marks, memory from
  `/proc/meminfo`, a container's own limits from its cgroup. No meter is needed for these.
- Which work: the meter reads every `/proc/<pid>/stat` each look. A process's time there includes
  its ended children's, so a compiler job that started and ended between two looks is counted,
  under the process that started it. A process with nothing above it is placed by
  `T3CODE_THREAD_ID` in its environment, then by its folder.
- Memory of a piece of work is the resident memory of its processes added up, which counts
  shared pages more than once.

## Measured on a boat host (2026-11-11, 8 processors, 16 GB, six threads open)

| | by the runners' own process trees | with detached work followed |
|---|---|---|
| processors the six threads used, 4 minutes | 0.05 | 3.2 (one thread's land: vitest, a local backend, a browser) |
| what a new thread of that repo then counted as | the floor, 1.0 | what the repo's threads were measured using |

## Measured on Jon's PC (2026-11-10, Ryzen 9 3900X, 24 logical processors, 64 GB)

| | the count that applied | what fairshare measured |
|---|---|---|
| 7 PC threads, 5 building Rust | 7 of 7: all let in | work wanted 36.0 of 16 processors: 15.3 in use, 20.7 waiting |
| The same PC 4 minutes later | 7 of 7: an eighth refused | 5.0 of 22 in use, none waiting: room |
| Boat host `t3-host-2`, a landing gate running | 0 of 4 threads: room | work wanted 28.1 of 8 processors |
| Processors kept for Jon, away 45 minutes | 8, fixed | 4, then 2: 22 of 24 for agent work |

The meter and its numbers agree with Windows' own per-processor counters to the percent. One look
costs two system calls; the hook that asks takes 20 ms on the PC and 0.7 s for a boat host.

## Measured on Jon's PC (2026-11-11): what made it lag, and what did not

| | a frame of the desktop | the window he was in |
|---|---|---|
| No agent load | 22 ms, slowest 44 | answers in 0.1 ms |
| Agent work filling the PC (12 burners, 3 full Go rebuilds), 8 processors kept | 24 to 34 ms, slowest 64 to 172 | 0.1 ms, slowest 1 to 7 |
| The same load, 2 kept | 37 to 46 ms, slowest 72 to 94 | 0.1 ms, slowest 4 to 15 |
| Owl's port gates driving the desktop from three worktrees, 8 kept, 4 of them idle | 25 stalls of 0.2 to 2.0 s in 100 s | blocked on the window manager 0.4 to 2.5 s at a time, 82 times in 7 minutes |

| a two-process, 12-second run, PC 17 to 21 of 22 processors busy | what its processes say they used | what the meter learned |
|---|---|---|
| look every 5 s, before | 23.5 processor-seconds | 17.1 (27% low, and 10 s of its 13) |
| look every 5 s, now | 22.1 and 23.9 | 24.6 and 26.8 |
| look every 1 s, now | 23.4 | 25.0 |

## Develop

See `AGENTS.md`.

## License

Public domain under [The Unlicense](UNLICENSE): use, change, and share it for any purpose.
