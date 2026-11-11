# What work uses, learned, end to end: the built fairshare with its real meter in a private home,
# real processes that burn real processors, a real run through the pool, and a thread whose job
# detaches from it the way a land does. Nothing is written by hand: every number the checks read
# was measured by the meter, and is held against what the burning processes themselves say they
# used, so the checks mean the same on a PC that is full as on one that is idle (2026-11-11: on a
# PC with 20 of its 22 processors busy a two-process burner really got 1.28, and a check that
# wanted "about 2" failed a meter that was right).
# Usage: python -I learn-e2e.py <fairshare program>      (Linux and Windows; about two minutes)
# To see it fail: LEARN_E2E_BREAK=1 keeps the meter from starting, and the checks on what was
# measured fail.
import json, os, shutil, subprocess, sys, tempfile, time

exe = os.path.abspath(sys.argv[1])
root = tempfile.mkdtemp(prefix="fs-learn-")
home = os.path.join(root, "home")
os.makedirs(home)
py = sys.executable
fails = 0
windows = os.name == "nt"
truth = os.path.join(root, "truth")
os.makedirs(truth)
env = dict(os.environ, FAIRSHARE_HOME=home, FAIRSHARE_RESERVE="measure", LEARN_E2E_TRUTH=truth)
for name in ("FAIRSHARE_READING", "FAIRSHARE_METER", "T3CODE_THREAD_ID"):
    env.pop(name, None)
if os.environ.get("LEARN_E2E_BREAK"):
    env["FAIRSHARE_METER"] = "off"
# A look every second, so a minute of the meter's life fits in this script. "fakeagent" stands in
# for an agent runner.
json.dump({"sample_seconds": 1, "runners": ["fakeagent"]}, open(os.path.join(home, "settings.json"), "w"))

# burn.py <processes> <seconds>: that many processes, each using one processor for that long and
# then writing down the processor-seconds it itself used.
burn = os.path.join(root, "burn.py")
open(burn, "w").write(
    "import os, subprocess, sys, time\n"
    "if sys.argv[1] == 'one':\n"
    "    end = time.time() + float(sys.argv[2])\n"
    "    while time.time() < end: pass\n"
    "    open(os.path.join(os.environ['LEARN_E2E_TRUTH'], '%d-%d' % (os.getpid(), time.time() * 1000)), 'w').write(str(time.process_time()))\n"
    "else:\n"
    "    kids = [subprocess.Popen([sys.executable, __file__, 'one', sys.argv[2]]) for _ in range(int(sys.argv[1]))]\n"
    "    [k.wait() for k in kids]\n")


def burned():
    """The processor-seconds the burners that have ended say they used, each counted once."""
    total = 0.0
    for name in os.listdir(truth):
        path = os.path.join(truth, name)
        try:
            total += float(open(path).read())
            os.remove(path)
        except (OSError, ValueError):
            pass
    return total


# hold.py <MB> <seconds> <seconds>: holds that much memory for the first stretch, gives it back,
# idles for the second, and writes down the most it itself held, as the system counts it for the
# process: resident memory on Linux, private memory on Windows, which is what each meter reads.
hold = os.path.join(root, "hold.py")
held_file = os.path.join(root, "held")
open(hold, "w").write(
    "import os, sys, time\n"
    "block = bytearray(int(sys.argv[1]) << 20)\n"
    "for i in range(0, len(block), 4096): block[i] = 1\n"
    "time.sleep(float(sys.argv[2]))\n"
    "most = len(block) / 2**30\n"
    "try:\n"
    "    if os.name == 'nt':\n"
    "        import ctypes, ctypes.wintypes as w\n"
    "        class Counters(ctypes.Structure):\n"
    "            _fields_ = [('cb', w.DWORD), ('faults', w.DWORD)] + [(n, ctypes.c_size_t) for n in ('peak_set', 'set', 'a', 'b', 'c', 'd', 'private', 'peak_private')]\n"
    "        c = Counters(); c.cb = ctypes.sizeof(c)\n"
    "        ctypes.windll.kernel32.GetCurrentProcess.restype = w.HANDLE\n"
    "        ctypes.windll.psapi.GetProcessMemoryInfo.argtypes = [w.HANDLE, ctypes.c_void_p, w.DWORD]\n"
    "        if ctypes.windll.psapi.GetProcessMemoryInfo(ctypes.windll.kernel32.GetCurrentProcess(), ctypes.byref(c), c.cb): most = c.peak_private / 2**30\n"
    "    else:\n"
    "        import resource\n"
    "        most = resource.getrusage(resource.RUSAGE_SELF).ru_maxrss / 2**20\n"
    "except Exception: pass\n"
    "del block\n"
    "open(r'%s', 'w').write(str(most))\n"
    "time.sleep(float(sys.argv[3]))\n" % held_file)
repo = os.path.join(root, "work", "demo")
tree = os.path.join(root, ".t3", "worktrees", "demo", "feature-x")
os.makedirs(repo)
os.makedirs(tree)


def fs(*args):
    done = subprocess.run([exe, *args], capture_output=True, text=True, env=env, stdin=subprocess.DEVNULL)
    return done.returncode, done.stdout.strip(), done.stderr.strip()


def check(name, ok, detail=""):
    global fails
    fails += 0 if ok else 1
    print("OK  " if ok else "FAIL", name, "" if ok else "-> " + str(detail)[:900])


def status():
    code, out, err = fs("status", "--json")
    try:
        return json.loads(out)
    except ValueError:
        return {"error": out + err}


def until(what, seconds=12):
    """Ask again until what() holds, since the meter looks once a second."""
    end = time.time() + seconds
    while time.time() < end:
        got = what()
        if got:
            return got
        time.sleep(0.5)
    return what()


def learned():
    code, out, err = fs("learned", "--json")
    try:
        return json.loads(out)
    except ValueError:
        return []


def row(kind, what=None):
    for r in learned():
        if r["kind"] == kind and r.get("repo") == "demo" and (what is None or what in r.get("what", "")):
            return r
    return None


meter = None
try:
    if windows:
        # Windows' meter is installed on the PC; here it runs in this private home, and never
        # tells Owl (FAIRSHARE_RESERVE=measure).
        if not os.environ.get("LEARN_E2E_BREAK"):
            meter = subprocess.Popen([exe, "meter"], env=env, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        until(lambda: "reading" in status(), 20)
    else:
        st = status()
        check("on Linux the first question a machine is asked starts its meter, and says so", any("has been started" in n for n in st.get("reading", {}).get("notes", [])), st)
        check("and from then on the reading names the open work, and no longer says it is unmeasured", until(lambda: os.path.exists(os.path.join(home, "work.json")))
              and not [n for n in status().get("reading", {}).get("notes", []) if "piece by piece" in n], status().get("reading", {}).get("notes"))
    code, out, err = fs("pool", "join", "--name", "here", "--repos", "demo=" + repo)
    check("this machine joins the pool", code == 0, out + err)

    # A run nobody has measured: it starts at the floor, and is measured while it runs.
    going = subprocess.Popen([exe, "pool", "run", "--repo", "demo", "--wait", "3", "--", py, burn, "2", "12"], env=env, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, text=True)
    # On a full machine the run waits for room before it starts (--wait 3), so this waits as long.
    seen = until(lambda: [w for w in status().get("reading", {}).get("work", []) if w["kind"] == "run" and w.get("repo") == "demo" and w["now"] >= 0.6] or going.poll() is not None, 200)
    seen = seen if isinstance(seen, list) else []
    check("a run is seen running as itself: its own processes, and the processors they burn", seen and seen[0]["repo"] == "demo" and seen[0]["processes"] >= 3, status().get("reading", {}).get("work"))
    said = going.communicate()[1]
    check("the first run of a command counts as the settings' floor, since nothing like it was measured", going.returncode == 0 and "counts as 1.0 processors" in said, said)
    got = until(lambda: row("run", "burn.py 2 12"))
    used = burned()
    counted = got["cores"] * got["minutes"] * 60 if got else 0
    check("when it ends, what was learned for that command is what its processes say they used (%.1f processor-seconds; the meter counted %.1f) over the 12 seconds it ran" % (used, counted),
          got and used > 4 and 0.85 * used <= counted <= 1.3 * used + 1 and 11.5 <= got["seconds"] <= 20 and got["works"] >= 1, got or learned())

    # The same command again counts as what it was measured using.
    done = subprocess.run([exe, "pool", "run", "--repo", "demo", "--wait", "3", "--", py, burn, "2", "3"], capture_output=True, text=True, env=env, stdin=subprocess.DEVNULL)
    check("another command of the repo counts as the repo's runs, not as the floor", done.returncode == 0 and "demo runs here were measured" in done.stderr, done.stderr)
    # Asked, not run: the answer says what the command counts as whether or not this machine has room now.
    code, out, err = fs("pool", "place", "--json", "--repo", "demo", "--cores", "7", "--run", " ".join([py, burn, "2", "12"]))
    me = (json.loads(out).get("machines") or [{}])[0] if out.startswith("{") else {}
    check("the measured command counts as itself the second time, nearer what it was measured using than the 7 its caller guessed with --cores",
          got and "burn.py 2 12` in demo here" in me.get("from", "") and 0.8 * got["cores"] <= me.get("counted", 0) < (got["cores"] + 7) / 2, out + err)
    check("and a run that ended leaves nothing counted: no mark, no grant", until(lambda: os.listdir(os.path.join(home, "marks")) == [] and status()["cpu"]["pending"] == 0), status().get("cpu"))

    # Memory, on a real process: a run that holds 300 MB for ten seconds and next to nothing for
    # the hundred after. It goes on beside the checks below and is read when it has ended.
    holding = subprocess.Popen([exe, "pool", "run", "--repo", "demo", "--wait", "3", "--", py, hold, "300", "10", "100"], env=env, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, text=True)
    held_now = until(lambda: [w for w in status().get("reading", {}).get("work", []) if w["kind"] == "run" and "hold.py" in w.get("what", "") and w["mem_gb"] >= 0.25] or holding.poll() is not None, 200)
    check("a run that holds 300 MB is seen holding it", isinstance(held_now, list) and held_now, status().get("reading", {}).get("work"))

    # The rule for work that drives the desktop, asked of this machine as it is now.
    code, out, err = fs("room", "--kind", "run", "--desktop", "--json")
    v = json.loads(out) if out.startswith("{") else {}
    state = v.get("reading", {}).get("presence", {}).get("state")
    check("work that drives the desktop is let in only while nobody is at the machine's screen (now: %s)" % state,
          state and v.get("desktop", {}).get("ok") == (state == "away") and (v["desktop"]["ok"] or "It drives the desktop" in v["reason"]), out + err)

    if not windows:
        # A thread whose work detaches from it, the way a land does: the runner starts two jobs
        # through a shell that exits at once, so both hang under init. One runs in the thread's
        # folder; the other runs elsewhere and only carries the thread's id.
        agent = os.path.join(root, "fakeagent")
        open(agent, "w").write("#!/bin/sh\ncd '%s'\n( setsid '%s' '%s' 1 14 >/dev/null 2>&1 & )\n( cd /tmp && setsid '%s' '%s' 1 14 >/dev/null 2>&1 & )\nsleep 22\n" % (tree, py, burn, py, burn))
        os.chmod(agent, 0o755)
        runner = subprocess.Popen([agent], env=dict(env, T3CODE_THREAD_ID="mcp:learn-e2e"), stdin=subprocess.DEVNULL)
        mine = lambda: [w for w in status().get("reading", {}).get("work", []) if w["kind"] == "thread" and w.get("repo") == "demo"]
        seen = until(lambda: [w for w in mine() if w["now"] >= 1.2], 14)
        work = status().get("reading", {}).get("work", [])
        check("work that detached from its thread still counts as that thread's: about 2 processors, where the runner's own tree uses none",
              seen and seen[0]["what"] == "feature-x" and seen[0]["processes"] >= 5, work)
        check("and is not left over as a job of nobody's", not [w for w in work if w["kind"] == "job" and w.get("repo") == "demo"], work)
        runner.wait()
        got = until(lambda: (lambda r: r if r and r["works"] >= 1 else None)(row("thread")), 20)
        check("what the thread and its detached work used is what was learned for the repo's threads",
              got and got["cores"] >= 0.5 and got["minutes"] >= 0.3, got or learned())
        code, out, err = fs("room", "--repo", "demo", "--json")
        v = json.loads(out) if out.startswith("{") else {}
        check("so a new thread of that repo counts as what its threads were measured using", code == 0 and v.get("sized", {}).get("from") == "demo threads here"
              and v["cpu"]["need"] >= 0.5 and "demo threads here were measured at" in v.get("reason", ""), out + err)

    # What the run that held memory was learned as: three numbers, not one. It held its most for a
    # tenth of its time, so a minute of it holds far less on average than its most.
    holding.communicate()
    got = until(lambda: (lambda r: r if r and r["works"] >= 1 else None)(row("run", "hold.py 300 10 100")), 20)
    try:
        said = float(open(held_file).read())
    except (OSError, ValueError):
        said = 0.0
    check("the most it was learned to hold is what the process itself says it held (%.2f GB; the meter counted %.2f)" % (said, got["mem_most"] if got else 0),
          got and said >= 0.29 and 0.9 * said <= got["mem_most"] <= said + 0.2, got or learned())
    check("and what it is expected to hold is its average over its minutes (%.2f GB, give or take %.2f), not its most" % ((got["mem_gb"], got["mem_swing"]) if got else (0, 0)),
          got and got["mem_gb"] <= 0.8 * got["mem_most"] and got["mem_swing"] >= 0.03, got or learned())
    code, out, err = fs("room", "--kind", "run", "--repo", "demo", "--what", got["what"] if got else "", "--json")
    v = json.loads(out) if out.startswith("{") else {}
    sized = v.get("sized", {})
    check("so another run of it is expected to hold its average and to reach its most", got and "hold.py" in sized.get("from", "")
          and sized["mem_gb"] <= 0.85 * sized["mem_most"] and sized["mem_most"] >= 0.8 * said and v["mem"]["need"] >= sized["mem_gb"] - 0.02, out + err)

    if not windows:
        code, out, err = fs("install", "--uninstall")
        check("the meter stops when told", until(lambda: not os.path.exists(os.path.join(home, "work.json")), 6), out + err)
finally:
    open(os.path.join(home, "meter.stop"), "w").close()
    if meter is not None:
        try:
            meter.wait(timeout=10)
        except subprocess.TimeoutExpired:
            meter.kill()
    time.sleep(1.5)
    shutil.rmtree(root, ignore_errors=True)
print("failures:", fails)
sys.exit(1 if fails else 0)
