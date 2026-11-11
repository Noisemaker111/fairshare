# The pool end to end, through the built fairshare in private homes: three machines, each its own
# home and its own reading, reached the way a real member is (a command put in front of the
# command), with a real wake command, a real plan-hours command, real git checkouts and a stand-in
# for Hona/mesh that prints what mesh's SPEC says it prints. Readings are written by hand so "full"
# and "room" are certain whatever this machine is doing.
# Usage: python -I pool-e2e.py <fairshare program>      (Windows and Linux)
import json, os, shutil, subprocess, sys, tempfile, time
from datetime import datetime, timezone

exe = os.path.abspath(sys.argv[1])
root = tempfile.mkdtemp(prefix="fs-pool-")
py = sys.executable
fails = 0
home = {n: os.path.join(root, n) for n in ("pc", "host-a", "host-b", "meshed")}
for h in home.values():
    os.makedirs(h)

# A machine: its home, and whether it answers. `via.py <home> <command...>` runs the command there.
via = os.path.join(root, "via.py")
open(via, "w").write(
    "import os, subprocess, sys\n"
    "home = sys.argv[1]\n"
    "if os.path.exists(os.path.join(home, 'asleep')):\n"
    "    sys.stderr.write('connect: the machine is stopped\\n'); sys.exit(255)\n"
    "env = dict(os.environ, FAIRSHARE_HOME=home, FAIRSHARE_READING=os.path.join(home, 'reading.json'))\n"
    "sys.exit(subprocess.run(sys.argv[2:], env=env).returncode)\n")
wake = os.path.join(root, "wake.py")
open(wake, "w").write(
    "import os, sys\n"
    "home = sys.argv[1]\n"
    "open(os.path.join(home, 'wakes'), 'a').write('woke\\n')\n"
    "os.path.exists(os.path.join(home, 'asleep')) and os.remove(os.path.join(home, 'asleep'))\n")
hours = os.path.join(root, "hours.py")
open(hours, "w").write("import os, sys\nprint(open(os.path.join(sys.argv[1], 'hours')).read())\n")
# Hona/mesh, as far as the pool uses it: `status --json --fast` and `exec --no-stall <device> <command...>`.
mesh = os.path.join(root, "mesh.py")
open(mesh, "w").write(
    "import json, os, subprocess, sys\n"
    "root = os.path.dirname(os.path.abspath(__file__))\n"
    "state = json.load(open(os.path.join(root, 'mesh.json')))\n"
    "if sys.argv[1:4] == ['status', '--json', '--fast']:\n"
    "    print(json.dumps(state)); sys.exit(0)\n"
    "if sys.argv[1:3] == ['exec', '--no-stall']:\n"
    "    device = [d for d in state['devices'] if d['name'] == sys.argv[3]]\n"
    "    if not device or not device[0]['online']:\n"
    "        sys.stderr.write('mesh: %s: offline\\n' % sys.argv[3]); sys.exit(255)\n"
    "    sys.exit(subprocess.run([sys.executable, '-I', os.path.join(root, 'via.py'), os.path.join(root, sys.argv[3])] + sys.argv[4:]).returncode)\n"
    "sys.exit(2)\n")

readings = {}


def reading(name, demand, processors=8, reserved=0, state="none", system="linux"):
    readings[name] = (demand, processors, reserved, state, system)


def stamp():
    """Every reading again, as of now: a machine never answers from an old one."""
    for name, (demand, processors, reserved, state, system) in readings.items():
        capacity = processors - reserved
        r = {"machine": name, "os": system, "at": datetime.now(timezone.utc).isoformat().replace("+00:00", "Z"),
             "processors": processors, "reserved": reserved, "capacity": capacity, "short_seconds": 60, "long_seconds": 600,
             "busy_short": demand, "busy_long": demand, "demand_short": demand, "demand_long": demand, "firm_short": demand, "firm_long": demand,
             "mem_total_gb": 64, "mem_avail_gb": 40, "disk_path": root, "disk_free_gb": 200,
             "presence": {"state": state, "idle_minutes": 0}}
        json.dump(r, open(os.path.join(home[name], "reading.json"), "w"))


def env_of(at):
    return dict(os.environ, FAIRSHARE_HOME=home[at], FAIRSHARE_READING=os.path.join(home[at], "reading.json"), FAIRSHARE_METER="off")


def fs(*args, at="pc"):
    stamp()
    env = env_of(at)
    done = subprocess.run([exe, *args], capture_output=True, text=True, env=env, stdin=subprocess.DEVNULL)
    return done.returncode, done.stdout.strip(), done.stderr.strip()


def placed(*args, at="pc"):
    code, out, err = fs("pool", "place", "--json", *args, at=at)
    try:
        return code, json.loads(out), out + err
    except ValueError:
        return code, {}, out + err


def check(name, ok, detail=""):
    global fails
    fails += 0 if ok else 1
    print("OK  " if ok else "FAIL", name, "" if ok else "-> " + str(detail)[:700])


def lines(path):
    return [l for l in open(path).read().splitlines() if l.strip()] if os.path.exists(path) else []


def git(folder, *args):
    return subprocess.run(["git", "-C", folder, "-c", "user.name=t", "-c", "user.email=t@example.invalid", *args], capture_output=True, text=True).stdout.strip()


# One repo, on the PC in a folder of its own name and on host-a under its folder of repos.
pc_repo = os.path.join(root, "pc-checkouts", "demo")
os.makedirs(pc_repo)
git(pc_repo, "init", "-q", "-b", "main")
open(os.path.join(pc_repo, "file.txt"), "w").write("one\n")
git(pc_repo, "add", "."), git(pc_repo, "commit", "-q", "-m", "one")
first = git(pc_repo, "rev-parse", "HEAD")
open(os.path.join(pc_repo, "file.txt"), "w").write("two\n")
git(pc_repo, "commit", "-q", "-am", "two")
host_work = os.path.join(root, "host-a-work")
os.makedirs(host_work)
subprocess.run(["git", "clone", "-q", pc_repo, os.path.join(host_work, "demo")], capture_output=True)
host_repo = os.path.join(host_work, "demo")
busy = os.path.join(home["host-a"], "gates")

# The PC: 24 processors, 4 kept for the person at it, so 20 for agent work filled to 90% (18).
reading("pc", 2, processors=24, reserved=4, state="active", system="windows")
reading("host-a", 0.5)
reading("host-b", 0.5)
reading("meshed", 0.5)


def words(*argv):
    return json.dumps(list(argv))


code, out, err = fs("pool")
check("with no machine joined the pool says so and how to join", code == 2 and "fairshare pool join" in err, out + err)
code, out, err = fs("pool", "join", "--name", "pc", "--can", "gpu,deploy", "--repos", "demo=" + pc_repo)
check("this machine joins with one command", code == 0 and "pc joined the pool: windows, 24 processors, this machine" in out, out + err)
code, out, err = fs("pool", "join", "--name", "host-a", "--via", words(py, "-I", via, home["host-a"]), "--fairshare", exe,
                    "--work", host_work, "--busy-dir", busy, "--extra", "t3_env=env-a")
check("another machine joins with one command, reached through its via", code == 0 and "host-a joined the pool: linux, 8 processors" in out, out + err)
code, out, err = fs("pool", "join", "--name", "host-b", "--via", words(py, "-I", via, home["host-b"]), "--fairshare", exe,
                    "--wake", words(py, "-I", wake, home["host-b"]), "--hours", words(py, "-I", hours, home["host-b"]), "--keep-hours", "10")
check("a machine with plan-hours joins the same way", code == 0 and "The pool has 3: pc, host-a, host-b" in out, out + err)
code, out, err = fs("pool", "join", "--name", "ghost", "--via", words(py, "-I", via, os.path.join(root, "nowhere")), "--fairshare", os.path.join(root, "no-such-program"))
check("a machine that does not answer does not join", code == 2 and "did not join" in err and "ghost" not in fs("pool")[1], out + err)

open(os.path.join(home["host-b"], "asleep"), "w").close()
open(os.path.join(home["host-b"], "hours"), "w").write("40")
code, out, err = fs("pool")
check("the pool is one place: what the awake machines have together, and who sleeps",
      code == 0 and "3 machines, 2 awake" in out and "32 processors, 28 of them for agent work" in out and "Asleep, woken only when the others are full: host-b" in out, out + err)
check("the processors kept for the person at the PC are not the pool's", any(l.split()[:6] == ["pc", "room", "windows", "24", "4", "20"] for l in out.splitlines()), out)
check("a sleeping machine's plan-hours are shown", "40.0 plan-hours left" in out, out)

code, pl, raw = placed("--label", "a thread")
check("an idle PC takes it: the most processors left (15.0 against 6.5)", code == 0 and pl.get("machine") == "pc" and pl.get("local") is True
      and [m["free"] for m in pl["machines"][:2]] == [15.0, 6.5], raw)

# Jon plays: 12 of 24 kept for him, 12 for agent work filled to 75% (9), 8.5 in use.
reading("pc", 8.5, processors=24, reserved=12, state="full-screen", system="windows")
code, pl, raw = placed("--label", "a thread", "--take")
check("while a game holds the PC the host takes it, and nobody named a machine", code == 0 and pl.get("machine") == "host-a" and pl.get("took") is True
      and "pc is full (8.5 of 9.0 in use)" in pl.get("reason", ""), raw)
check("the caller gets the member's own fields back", pl.get("extra") == {"t3_env": "env-a"}, raw)
grants = json.load(open(os.path.join(home["host-a"], "grants.json")))
check("it counts on the host until its load shows", len(grants) == 1 and grants[0]["label"] == "a thread", grants)
check("the sleeping host was not woken for it", not lines(os.path.join(home["host-b"], "wakes")), raw)
code, pl, raw = placed("--needs", "gpu")
check("work that needs the PC's GPU waits for the PC and wakes nobody", code == 1 and not lines(os.path.join(home["host-b"], "wakes"))
      and "host-a is not open to it (cannot do gpu)" in pl.get("reason", ""), raw)
code, pl, raw = placed("--needs", "linux")
check("work that needs Linux never lands on the PC", code == 0 and pl.get("machine") == "host-a", raw)

reading("pc", 2, processors=24, reserved=4, state="active", system="windows")
code, pl, raw = placed("--among", "host-a")
check("work that may use only some machines stays on them", code == 0 and pl.get("machine") == "host-a", raw)
reading("host-a", 7.5)  # 7.5 in use, a thread just let in counts as 1 more: over its 8
code, pl, raw = placed("--among", "host-a,host-b", "--no-wake")
check("asked not to wake, the pool names the sleeping machine for the caller to wake", code == 0 and pl.get("machine") == "host-b" and pl.get("asleep") is True
      and not lines(os.path.join(home["host-b"], "wakes")), raw)

reading("pc", 17.9, processors=24, reserved=4, state="active", system="windows")
code, pl, raw = placed("--label", "needs a wake", "--take")
check("only with every awake machine full is the sleeping host woken, and it takes the work", code == 0 and pl.get("machine") == "host-b" and pl.get("woke") is True
      and lines(os.path.join(home["host-b"], "wakes")) == ["woke"], raw)
check("and the work counts there", [g["label"] for g in json.load(open(os.path.join(home["host-b"], "grants.json")))] == ["needs a wake"], raw)

open(os.path.join(home["host-b"], "asleep"), "w").close()
open(os.path.join(home["host-b"], "hours"), "w").write('{"left": 8}')
code, pl, raw = placed("--label", "no hours for it")
check("with 8 plan-hours left and 10 kept the host stays asleep and the pool is full", code == 1 and not pl.get("ok")
      and "8.0 plan-hours are left and 10.0 are kept" in pl.get("reason", "") and len(lines(os.path.join(home["host-b"], "wakes"))) == 1, raw)
open(os.path.join(home["host-b"], "hours"), "w").write("who knows")
code, pl, raw = placed()
check("plan-hours that cannot be read are never spent", code == 1 and "could not be read" in pl.get("reason", "")
      and len(lines(os.path.join(home["host-b"], "wakes"))) == 1, raw)

# Runs: a command goes where there is room, in the repo's folder there.
reading("pc", 2, processors=24, reserved=4, state="active", system="windows")
reading("host-a", 0.5)
where = [py, "-c", "import os; print(os.path.realpath(os.getcwd()))"]
code, out, err = fs("pool", "run", "--repo", "demo", "--", *where)
check("a run with room on the PC runs in the PC's folder for the repo", code == 0 and out == os.path.realpath(pc_repo) and "pc takes it" in err, out + err)
code, out, err = fs("pool", "run", "--repo", "demo", "--", py, "-c", "import sys; sys.exit(7)")
check("the command's exit is the run's exit", code == 7 and "exit 7" in err, out + err)
# Work that drives the desktop (opens windows on it, moves the pointer, captures the screen): only
# on the machine that has one, only while nobody is at it, and one piece at a time.
code, out, err = fs("pool", "run", "--repo", "demo", "--needs", "desktop", "--", *where)
check("a run that drives the desktop does not start while someone is at the PC, and no Linux host takes it", code == 69 and out == ""
      and "someone is at the PC" in err and "host-a is not open to it (has no desktop)" in err, out + err)
reading("pc", 2, processors=24, reserved=4, state="away", system="windows")
ask = "import subprocess, sys; r = subprocess.run([sys.argv[1], 'room', '--kind', 'run', '--desktop'], capture_output=True, text=True); print(r.returncode, r.stdout.strip())"
code, out, err = fs("pool", "run", "--repo", "demo", "--needs", "desktop", "--", py, "-c", ask, exe)
# The second is refused for the first either way: as work let in a moment ago (a reading that does
# not list it yet, as here on Windows) or as the open run itself (Linux reads the run's mark).
check("with nobody at the PC it runs there, and while it runs no second one may start", code == 0 and "pc takes it" in err
      and out.startswith("1 ") and ("It drives the desktop, and work let in a moment ago is about to drive it" in out
                                    or "It drives the desktop, and the run driving the desktop in demo" in out), out + err)
code, out, err = fs("room", "--kind", "run", "--desktop")
check("once it has ended the desktop is free again", code == 0, out + err)
reading("pc", 2, processors=24, reserved=4, state="active", system="windows")
code, out, err = fs("pool", "run", "--repo", "demo", "--", *where)
check("a run that does not drive the desktop is not held for it", code == 0 and "pc takes it" in err, out + err)
reading("pc", 17.9, processors=24, reserved=4, state="active", system="windows")
code, out, err = fs("pool", "run", "--repo", "demo", "--", *where)
check("with the PC full the same run goes to the host, in the host's folder", code == 0 and out == os.path.realpath(host_repo) and "host-a takes it" in err, out + err)
show = "import os, subprocess; print(subprocess.run(['git', 'rev-parse', 'HEAD'], capture_output=True, text=True).stdout.strip()); print(open('file.txt').read().strip()); print(os.listdir(r'%s'))" % busy
code, out, err = fs("pool", "run", "--repo", "demo", "--commit", first, "--label", "a gate", "--", py, "-c", show)
got = out.splitlines()
check("a gate runs on exactly the commit asked for, not what the folder has checked out", code == 0 and got[:2] == [first, "one"], out + err)
check("while it runs the host's idle watch can see it", len(got) == 3 and "pool-" in got[2], out)
check("and nothing is left behind: no mark, no extra checkout, the folder as it was", os.listdir(busy) == [] and len(git(host_repo, "worktree", "list").splitlines()) == 1
      and open(os.path.join(host_repo, "file.txt")).read().strip() == "two", os.listdir(busy))
code, out, err = fs("pool", "run", "--repo", "elsewhere", "--", *where)
check("a repo no machine has is not run anywhere", code == 69 and "no folder" in err, out + err)
reading("host-a", 7.9)
code, out, err = fs("pool", "run", "--repo", "demo", "--", *where)
check("a run with the whole pool full does not run, and says who is full", code == 69 and "No machine in the pool has room" in err and out == "", out + err)
record = [json.loads(l) for l in lines(os.path.join(home["pc"], "decisions.jsonl"))]
runs = [r for r in record if r.get("event") == "pool run"]
check("every placement and run is on the record, with how long each took", len(runs) == 6 and runs[-1]["machine"] == "host-a" and runs[-1]["commit"] == first
      and all("seconds" in r for r in record if r.get("event") == "pool place"), runs)

# The line. A launch (it has a key and a launcher) that no machine has room for waits for the
# machines open to it, and with no queue_place command the pool itself finds it a machine.
notify = os.path.join(root, "notify.py")
open(notify, "w").write("import os, sys\nopen(os.path.join(os.path.dirname(os.path.abspath(__file__)), 'told.jsonl'), 'a').write(sys.stdin.read().strip() + '\\n')\n")
json.dump({"queue_notify": [py, "-I", notify], "queue_place_seconds": 0.01}, open(os.path.join(home["pc"], "settings.json"), "w"))
asked = os.path.join(root, "launch.json")
open(asked, "w").write('{"title": "a waiting launch"}')
reading("pc", 17.9, processors=24, reserved=4, state="active", system="windows")
reading("host-a", 7.9)
launch = ("--label", "a waiting launch", "--repo", "demo", "--folder", "--take", "--no-wake", "--key", "k1", "--launcher", "thread-1", "--request", asked)
code, pl, raw = placed(*launch)
check("a launch no machine has room for joins the line, for the machines open to it", code == 1 and pl.get("queued") is True and pl.get("place") == 1
      and "It is in the line, place 1 of 1, for pc, host-a" in pl.get("reason", "") and "thread-1" in pl.get("reason", ""), raw)
line = json.loads(fs("queue", "--json")[1])
check("with its request kept, and not for the machine that has no folder for its repo", len(line) == 1 and line[0]["machines"] == ["pc", "host-a"]
      and line[0].get("folder") is True and open(line[0]["request"]).read() == '{"title": "a waiting launch"}', line)
code, pl, raw = placed(*launch)
check("sent again while it waits it keeps its place", code == 1 and pl.get("queued") is True and "already in the line, place 1 of 1" in pl.get("reason", "")
      and len(json.loads(fs("queue", "--json")[1])) == 1, raw)
reading("host-a", 0.5)
code, pl, raw = placed("--label", "behind it", "--repo", "demo", "--folder", "--take", "--no-wake", "--key", "k2", "--launcher", "thread-2")
check("a launch that arrives when room has appeared does not pass the one waiting for the same machines", code == 1 and pl.get("queued") is True and pl.get("place") == 2
      and "launches already wait in the line for it" in raw, raw)
code, pl, raw = placed("--label", "hotfix for production", "--repo", "demo", "--folder")
check("an urgent one does", code == 0 and pl.get("machine") == "host-a", raw)
code, out, err = fs("queue", "move")
told = [json.loads(l) for l in lines(os.path.join(root, "told.jsonl"))]
check("the pool finds the first in line a machine, and its launcher is told", len(told) >= 1 and told[0]["event"] == "offer" and told[0]["entry"]["key"] == "k1"
      and told[0]["entry"]["machine"] == "host-a", str(told) + out + err)
check("the room is counted on that machine", "a waiting launch" in [g.get("label") for g in json.load(open(os.path.join(home["host-a"], "grants.json")))], out + err)
code, pl, raw = placed(*launch)
check("sent again, it goes to the machine held for it, with that machine's own fields", code == 0 and pl.get("machine") == "host-a" and pl.get("took") is True
      and "waited in the line" in pl.get("reason", "") and pl.get("extra") == {"t3_env": "env-a"}
      and "k1" not in [e["key"] for e in json.loads(fs("queue", "--json")[1])], raw)
# A launch only a sleeping machine could take waits for it, and is offered it once it may be woken.
for name in ("queue.json", "grants.json"):
    for h in ("pc", "host-a", "host-b"):
        os.path.exists(os.path.join(home[h], name)) and os.remove(os.path.join(home[h], name))
os.path.exists(os.path.join(root, "told.jsonl")) and os.remove(os.path.join(root, "told.jsonl"))
open(os.path.join(home["host-b"], "asleep"), "w").close()
open(os.path.join(home["host-b"], "hours"), "w").write('{"left": 40, "keep": 40, "why": "86 used in the last 24 hours, and 14 a day lasts to the reset"}')
sleeper = ("--label", "for the sleeper", "--among", "host-b", "--take", "--no-wake", "--key", "k3", "--launcher", "thread-3")
woken = len(lines(os.path.join(home["host-b"], "wakes")))
code, pl, raw = placed(*sleeper)
check("how many plan-hours are kept can be the hours command's to say, with its reason: 40 left, 40 kept, no wake", code == 1 and pl.get("queued") is True
      and "40.0 plan-hours are left and 40.0 are kept (86 used in the last 24 hours, and 14 a day lasts to the reset)" in pl.get("reason", "")
      and "for host-b" in pl.get("reason", ""), raw)
code, out, err = fs("pool")
check("and `fairshare pool` shows that it may not be woken now", "40.0 plan-hours left, 40.0 kept: no wake now." in out, out + err)
fs("queue", "move")
check("it is not offered a machine that may not be woken", not lines(os.path.join(root, "told.jsonl")), lines(os.path.join(root, "told.jsonl")))
open(os.path.join(home["host-b"], "hours"), "w").write('{"left": 40, "keep": 12, "why": "2 used in the last 24 hours"}')
fs("queue", "move")
told = [json.loads(l) for l in lines(os.path.join(root, "told.jsonl"))]
check("once the plan-hours allow a wake it is offered the sleeping machine, which is not woken for the offer", len(told) == 1 and told[0]["entry"]["machine"] == "host-b"
      and len(lines(os.path.join(home["host-b"], "wakes"))) == woken, told)
code, pl, raw = placed(*sleeper)
check("sent again and asked not to wake, the caller is told the machine sleeps", code == 0 and pl.get("machine") == "host-b" and pl.get("asleep") is True
      and len(lines(os.path.join(home["host-b"], "wakes"))) == woken, raw)
code, out, err = fs("pool", "wake", "host-b", "--json")
got = json.loads(out) if out.startswith("{") else {}
check("`pool wake` wakes a machine by its name, with its own wake command", code == 0 and got.get("ok") is True and got.get("machine") == "host-b"
      and len(lines(os.path.join(home["host-b"], "wakes"))) == woken + 1, out + err)
code, out, err = fs("pool", "wake", "host-a")
check("a machine that answers already is not woken again", code == 0 and "host-a is awake" in out, out + err)
code, out, err = fs("pool", "wake", "nobody")
check("and a name that is no machine is an error", code == 2 and "no machine nobody in the pool" in err, out + err)
# "That machine" without its name: by what its extra holds.
reading("pc", 2, processors=24, reserved=4, state="active", system="windows")
code, pl, raw = placed("--where", "t3_env=env-a")
check("work that asks for the machine holding a T3 environment goes there, though the PC has more room", code == 0 and pl.get("machine") == "host-a"
      and "pc is not open to it (not the machine this work asks for (t3_env))" in pl.get("reason", ""), raw)
code, pl, raw = placed("--where", "t3_env=no-such")
check("and nowhere when no machine holds it", code == 1 and not pl.get("ok"), raw)
# A run whose caller can run it here itself: it runs only when it goes elsewhere.
went = os.path.join(root, "placed.json")
reading("host-a", 7.9)
code, out, err = fs("pool", "run", "--repo", "demo", "--elsewhere", "--placed", went, "--as", "Demo  Gate", "--", *where)
left = [g for g in (json.load(open(os.path.join(home["pc"], "grants.json"))) if os.path.exists(os.path.join(home["pc"], "grants.json")) else []) if g.get("kind") == "run"]
check("with the most room on this machine, `pool run --elsewhere` runs nothing, exits 70 and leaves nothing counted", code == 70 and out == ""
      and json.load(open(went)).get("local") is True and left == [], out + err + str(left))
reading("pc", 17.9, processors=24, reserved=4, state="active", system="windows")
reading("host-a", 0.5)
code, out, err = fs("pool", "run", "--repo", "demo", "--elsewhere", "--placed", went, "--as", "Demo  Gate", "--", *where)
last = [json.loads(l) for l in lines(os.path.join(home["pc"], "decisions.jsonl"))][-1]
check("with room elsewhere it runs there, the placement is written first, and it is learned under the name given", code == 0 and out == os.path.realpath(host_repo)
      and json.load(open(went)).get("machine") == "host-a" and json.load(open(went)).get("extra") == {"t3_env": "env-a"} and last.get("what") == "demo gate", out + err + str(last))
# As it was: the line empty, nothing counted, host-b asleep with hours nobody can read.
for name in ("queue.json", "grants.json", "settings.json"):
    for h in ("pc", "host-a", "host-b"):
        os.path.exists(os.path.join(home[h], name)) and os.remove(os.path.join(home[h], name))
open(os.path.join(home["host-b"], "asleep"), "w").close()
open(os.path.join(home["host-b"], "hours"), "w").write("who knows")

# What work takes is learned, each machine's own, and the pool passes it on. host-a has run this
# repo's gate for an hour at 6 processors; the PC has never run it.
gate = "bun run gate"
hour = {"at": datetime.now(timezone.utc).isoformat().replace("+00:00", "Z"), "minutes": 60, "cores": 360, "sq": 2160, "mem_gb": 300, "mem_peak": 5, "works": 12, "seconds": 3600}
json.dump({"run|demo|" + gate: hour, "run|demo|": hour, "run||": hour}, open(os.path.join(home["host-a"], "learned.json"), "w"))
reading("pc", 2, processors=24, reserved=4, state="active", system="windows")
reading("host-a", 0.5)
code, pl, raw = placed("--repo", "demo", "--run", gate)
m = {x["name"]: x for x in pl.get("machines", [])}
check("a run counts on each machine as what that machine measured for the command: 6 processors where it ran, the floor where it never did",
      code == 0 and 5.8 <= m["host-a"].get("counted", 0) <= 6.6 and "`bun run gate` in demo here" in m["host-a"].get("from", "") and m["pc"].get("counted") == 1.0, raw)
code, pl, raw = placed("--repo", "demo", "--run", gate)
m = {x["name"]: x for x in pl.get("machines", [])}
check("the pool passes it on: the PC, which never ran it, now counts it as what the host measured", code == 0 and m["pc"].get("counted", 0) >= 4
      and "on host-a" in m["pc"].get("from", "") and "there it counts as" in pl.get("reason", "").lower(), raw)
code, pl, raw = placed("--repo", "demo", "--run", "bun run lint")
m = {x["name"]: x for x in pl.get("machines", [])}
check("another command of the repo is other work: the PC is told nothing about it", code == 0 and m["pc"].get("counted") == 1.0, raw)
code, pl, raw = placed("--repo", "demo", "--run", gate, "--cores", "1", "--among", "host-a")
m = {x["name"]: x for x in pl.get("machines", [])}
check("a caller's own `--cores 1` does not replace an hour of measurement", code == 0 and m["host-a"].get("counted", 0) >= 5.5, raw)
code, pl, raw = placed("--repo", "nowhere-measured", "--run", "make all", "--cores", "3", "--among", "pc")
m = {x["name"]: x for x in pl.get("machines", [])}
check("and stands only where nothing like the work was ever measured", code == 0 and m["pc"].get("counted") == 3.0, raw)
if os.name != "nt":
    # A run counts for as long as it runs, not for a set time (a machine without a meter knows
    # its runs by the marks they leave; on Windows the meter follows the mark).
    before = json.loads(fs("status", "--json", at="host-a")[1])["cpu"]["pending"]
    going = subprocess.Popen([exe, "pool", "run", "--repo", "demo", "--among", "host-a", "--", py, "-c", "import time; time.sleep(4)"],
                             env=env_of("pc"), stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    time.sleep(2)
    code, out, err = fs("status", "--json", at="host-a")
    st = json.loads(out) if out.startswith("{") else {}
    work = st.get("reading", {}).get("work", [])
    check("while a run runs its machine counts it as open work, not as a launch still to come", len(work) == 1 and work[0]["kind"] == "run"
          and work[0]["repo"] == "demo" and st["open"]["pieces"] == 1 and st["cpu"]["pending"] == before, str(work) + str(st.get("cpu")) + err)
    going.wait()
    code, out, err = fs("status", "--json", at="host-a")
    st = json.loads(out) if out.startswith("{") else {}
    check("and once it ended it counts as nothing: no open work, no mark, no grant left behind", going.returncode == 0 and not st.get("reading", {}).get("work")
          and st["cpu"]["pending"] == before and os.listdir(os.path.join(home["host-a"], "marks")) == [], str(st.get("cpu")) + err)
# A machine still on the fairshare from before work had a kind is asked the old question.
old = os.path.join(root, "old-via.py")
open(old, "w").write(
    "import json, os, subprocess, sys\n"
    "args = json.loads(bytes.fromhex(sys.argv[-1])).get('args', [])\n"
    "if '--kind' in args or '--seen' in args:\n"
    "    sys.stderr.write('flag provided but not defined: -kind\\nUsage of room:\\n'); sys.exit(2)\n"
    "sys.exit(subprocess.run([sys.executable, '-I', os.path.join(os.path.dirname(os.path.abspath(__file__)), 'via.py')] + sys.argv[1:]).returncode)\n")
pool = json.load(open(os.path.join(home["pc"], "pool.json")))
kept_via = [m for m in pool["members"] if m["name"] == "host-a"][0]["via"]
for member in pool["members"]:
    if member["name"] == "host-a":
        member["via"] = [py, "-I", old, home["host-a"]]
json.dump(pool, open(os.path.join(home["pc"], "pool.json"), "w"))
code, pl, raw = placed("--repo", "demo", "--run", gate, "--among", "host-a")
m = {x["name"]: x for x in pl.get("machines", [])}
check("a machine on the build before this one still answers: it is asked the old way", code == 0 and pl.get("machine") == "host-a" and m["host-a"].get("old") is True, raw)
for member in pool["members"]:
    if member["name"] == "host-a":
        member["via"] = kept_via
json.dump(pool, open(os.path.join(home["pc"], "pool.json"), "w"))
reading("pc", 17.9, processors=24, reserved=4, state="active", system="windows")
reading("host-a", 7.9)

# A machine that cannot tell is not a machine that is full.
readings.pop("pc")
os.remove(os.path.join(home["pc"], "reading.json"))
code, pl, raw = placed()
check("when a machine cannot tell and the rest are full, the pool cannot tell either", code == 2 and [m["state"] for m in pl.get("machines", [])][:2] == ["degraded", "full"], raw)
reading("host-a", 0.5)
code, pl, raw = placed()
check("and when another has room the work goes there", code == 0 and pl.get("machine") == "host-a", raw)
# A carrier that wakes a machine just by being used: the member's own check is asked first.
open(os.path.join(home["host-b"], "state"), "w").write("boat-host: host-b stopped")
state = os.path.join(root, "state.py")
open(state, "w").write("import os, sys\nprint(open(os.path.join(sys.argv[1], 'state')).read())\n")
os.path.exists(os.path.join(home["host-b"], "asleep")) and os.remove(os.path.join(home["host-b"], "asleep"))
pool = json.load(open(os.path.join(home["pc"], "pool.json")))
for m in pool["members"]:
    if m["name"] == "host-b":
        m["awake"], m["awake_match"] = [py, "-I", state, home["host-b"]], r" (ready|idle|running)\b"
        m["via"] = [py, "-I", os.path.join(root, "via-counted.py"), home["host-b"]]
json.dump(pool, open(os.path.join(home["pc"], "pool.json"), "w"))
open(os.path.join(root, "via-counted.py"), "w").write(
    "import os, subprocess, sys\n"
    "open(os.path.join(sys.argv[1], 'reached'), 'a').write('reached\\n')\n"
    "sys.exit(subprocess.run([sys.executable, '-I', os.path.join(os.path.dirname(os.path.abspath(__file__)), 'via.py')] + sys.argv[1:]).returncode)\n")
open(os.path.join(home["host-b"], "hours"), "w").write("3")
reading("host-a", 7.9)
code, pl, raw = placed()
states = {m["name"]: m["state"] for m in pl.get("machines", [])}
check("a machine its own check calls stopped is asleep, and nothing is sent through its carrier", states.get("host-b") == "asleep"
      and not lines(os.path.join(home["host-b"], "reached")), raw)
open(os.path.join(home["host-b"], "state"), "w").write("boat-host: host-b ready t3-env=1")
code, pl, raw = placed()
check("once its check calls it ready it is asked like any machine", code == 0 and pl.get("machine") == "host-b"
      and len(lines(os.path.join(home["host-b"], "reached"))) == 1, raw)
reading("host-a", 0.5)
code, out, err = fs("pool", "leave", "host-b")
check("a machine leaves with one command", code == 0 and "2 machines remain" in out, out + err)

# Hona/mesh: the mesh's devices are the pool's machines, reached through `mesh exec`.
os.makedirs(os.path.join(root, "blade"))
os.makedirs(os.path.join(root, "air"))
home["blade"], home["air"] = os.path.join(root, "blade"), os.path.join(root, "air")
reading("blade", 6, processors=16, system="windows")
reading("air", 0.5)


def mesh_is(*devices):
    json.dump({"self": "meshed", "devices": [{"name": "meshed", "self": True, "os": "linux", "online": True}] + list(devices)}, open(os.path.join(root, "mesh.json"), "w"))


mesh_is({"name": "blade", "self": False, "os": "windows", "online": True}, {"name": "air", "self": False, "os": "macos", "online": False})
code, out, err = fs("pool", "join", "--mesh", "--mesh-program", words(py, "-I", mesh), at="meshed")
check("the pool follows the mesh with one command", code == 0 and "3 devices, 3 machines in the pool" in out, out + err)
pool = json.load(open(os.path.join(home["meshed"], "pool.json")))
blade = [m for m in pool["members"] if m["name"] == "blade"][0]
check("a mesh device is reached through `mesh exec`", blade["via"][-3:] == ["exec", "--no-stall", "blade"] and blade["from"] == "mesh", blade)
blade["fairshare"] = exe  # where fairshare is on that device
json.dump(pool, open(os.path.join(home["meshed"], "pool.json"), "w"))
code, pl, raw = placed(at="meshed")
states = {m["name"]: m["state"] for m in pl.get("machines", [])}
check("work is placed across the mesh: blade's 9 free processors beat this machine's 6.5", code == 0 and pl.get("machine") == "blade"
      and states == {"meshed": "room", "blade": "room", "air": "unreachable"}, raw)
mesh_is({"name": "blade", "self": False, "os": "windows", "online": True}, {"name": "air", "self": False, "os": "macos", "online": True},
        {"name": "vps", "self": False, "os": "linux", "online": False})
code, out, err = fs("pool", at="meshed")
check("a device that joins the mesh is in the pool with no further command", code == 0 and "4 machines" in out and "vps" in out, out + err)
kept = [m for m in json.load(open(os.path.join(home["meshed"], "pool.json")))["members"] if m["name"] == "blade"][0]
check("and what was set on a device by hand is kept", kept.get("fairshare") == exe, kept)
mesh_is({"name": "blade", "self": False, "os": "windows", "online": True})
code, out, err = fs("pool", at="meshed")
check("a device that leaves the mesh leaves the pool", code == 0 and "2 machines" in out and "air" not in out, out + err)

shutil.rmtree(root, ignore_errors=True)
print("failures:", fails)
sys.exit(1 if fails else 0)
