# The line end to end, through the built fairshare.exe in a private home: real files, real lock,
# the real notify command run by fairshare. The reading is written by hand so "full" and "room" are
# certain whatever the PC is doing. Usage: python -I queue-e2e.py <fairshare program>
# (Windows and Linux; on Linux there is no Owl to tell, so that one check is left to Windows)
import json, os, shutil, subprocess, sys, tempfile
from datetime import datetime, timezone

# The whole path: a bare name is looked for on PATH, where the installed fairshare is, and a
# check of the build must never be a check of what is installed.
exe = os.path.abspath(sys.argv[1])
home = tempfile.mkdtemp(prefix="fs-queue-")
env = dict(os.environ, FAIRSHARE_HOME=home, FAIRSHARE_READING=os.path.join(home, "reading.json"), FAIRSHARE_METER="off")
env.pop("FAIRSHARE_RESERVE", None)
told = os.path.join(home, "told.jsonl")
notify = os.path.join(home, "notify.py")
open(notify, "w").write("import sys\nopen(r'%s','a').write(sys.stdin.read()+'\\n')\n" % told)
json.dump({"queue_notify": [sys.executable, "-I", notify], "queue_hold_minutes": 10}, open(os.path.join(home, "settings.json"), "w"))
fails = 0


def reading(demand, firm=None, free=30, work=None):
    r = {"machine": "test-pc", "os": "windows", "at": datetime.now(timezone.utc).isoformat().replace("+00:00", "Z"),
         "processors": 24, "reserved": 4, "capacity": 20, "short_seconds": 60, "long_seconds": 600,
         "busy_short": min(demand, 20), "busy_long": min(demand, 20), "waiting_short": max(0, demand - 20), "waiting_long": max(0, demand - 20),
         "demand_short": demand, "demand_long": demand, "mem_total_gb": 64, "mem_avail_gb": free, "disk_path": "C:\\", "disk_free_gb": 60,
         "presence": {"state": "active", "idle_minutes": 0},
         "firm_short": demand if firm is None else firm, "firm_long": demand if firm is None else firm}
    if work:
        r["work"] = work
    json.dump(r, open(os.path.join(home, "reading.json"), "w"))


def fs(*args, stdin=None):
    done = subprocess.run([exe, *args], capture_output=True, text=True, env=env, input=stdin, stdin=None if stdin is not None else subprocess.DEVNULL)
    return done.returncode, (done.stdout + done.stderr).strip()


def check(name, ok, detail=""):
    global fails
    fails += 0 if ok else 1
    print("OK  " if ok else "FAIL", name, "" if ok else "-> " + detail[:400])


def launch(key, title, launcher="thread-A"):
    return fs("room", "--threads", "7", "--take", "--label", title, "--queue", "--key", key, "--launcher", launcher, "--request", "-",
              stdin=json.dumps({"title": title, "message": "the whole brief"}))


reading(30)  # full: 30 wanted of 20
code, out = launch("k1", "[pc] first")
check("a full PC refuses and puts the launch in the line, place 1", code == 1 and "in the line, place 1" in out, out)
code, out = launch("k2", "[pc] second", "thread-B")
check("the next goes behind it", code == 1 and "place 2" in out, out)
code, out = launch("k1", "[pc] first")
check("sending the first again does not add it twice", code == 1 and "already in the line, place 1 of 2" in out, out)
saved = json.load(open(os.path.join(home, "queue", "1.json")))
check("the whole request is kept", saved.get("message") == "the whole brief", str(saved))
code, out = fs("queue", "move")
check("no room: nobody is told", not os.path.exists(told), out)

reading(15)  # limit 18 (90% of 20); a thread counts as 15/7 = 2.1: one fits (17.1), two do not (19.3)
code, out = fs("queue", "move")
notices = [json.loads(l) for l in open(told) if l.strip()] if os.path.exists(told) else []
check("room for one: the first in line is told, by the notify command", len(notices) == 1 and notices[0]["event"] == "offer"
      and notices[0]["entry"]["title"] == "[pc] first" and notices[0]["entry"]["launcher"] == "thread-A", out + str(notices))
code, out = fs("room", "--threads", "7", "--take", "--label", "[pc] queue jumper", "--queue", "--key", "k9", "--launcher", "thread-C")
check("a new launch cannot take the room held for the line; it joins the back", code == 1 and "place 3" in out, out)
code, out = launch("k2", "[pc] second", "thread-B")
check("the second is still told to wait", code == 1 and "already in the line" in out, out)
code, out = launch("k1", "[pc] first")
check("the first, sent again, goes straight in", code == 0 and "waited in the line" in out, out)
code, out = fs("queue")
check("it has left the line", "[pc] first" not in out and "[pc] second" in out, out)
grants = json.load(open(os.path.join(home, "grants.json")))
check("and counts as let in until its load shows", len(grants) == 1 and grants[0]["label"] == "[pc] first", str(grants))
code, out = fs("queue", "move")
check("the room it took is not offered to the next", len([l for l in open(told) if l.strip()]) == 1, out)

reading(5)  # plenty
code, out = fs("queue", "move")
notices = [json.loads(l) for l in open(told) if l.strip()]
check("with room for all, the rest are told in order", [n["entry"]["title"] for n in notices[1:]] == ["[pc] second", "[pc] queue jumper"], str(notices))
code, out = fs("queue", "drop", "3")
check("a launch can be taken out of the line", code == 0 and "left the line" in out, out)
code, out = launch("k2", "[pc] second", "thread-B")
check("the second goes in", code == 0, out)
code, out = fs("queue")
check("the line is empty", "Nothing is waiting" in out, out)

# Break it: no notify command, and the refusal has to say nobody will be told.
json.dump({}, open(os.path.join(home, "settings.json"), "w"))
reading(30)
code, out = launch("k5", "[pc] untold")
check("without a notify command the refusal says degraded", code == 1 and "degraded: queue_notify is not set" in out, out)

# One line for several machines: a launch that can run on a host is asked about through the
# queue_place command, held where that says, and never passes a launch waiting for the same machine.
fs("queue", "drop", "1")
place = os.path.join(home, "place.py")
asked = os.path.join(home, "asked.jsonl")
open(place, "w").write("import sys, json\nq = json.load(sys.stdin)\nopen(r'%s','a').write(json.dumps(q['usable'])+'\\n')\n"
                       "print(json.dumps({'machine': 'host-b' if 'host-b' in q['usable'] else ''}))\n" % asked)
json.dump({"queue_notify": [sys.executable, "-I", notify], "queue_place": [sys.executable, "-I", place], "queue_place_seconds": 0.01},
          open(os.path.join(home, "settings.json"), "w"))
open(told, "w").close()
reading(30)
launch("p1", "[pc] needs this PC")
code, out = fs("queue", "add", "--key", "h1", "--launcher", "thread-C", "--label", "runs anywhere", "--machines", "pc,host-a,host-b",
               "--request", "-", stdin=json.dumps({"title": "runs anywhere"}))
check("a launch that can run on three machines joins the same line, behind the PC's", code == 0 and json.loads(out).get("place") == 2, out)
code, out = fs("queue", "add", "--key", "h2", "--launcher", "thread-D", "--label", "runs anywhere too", "--machines", "pc,host-a,host-b")
fs("queue", "move")
line = {e["key"]: e for e in json.loads(fs("queue", "--json")[1])}
usable = [json.loads(l) for l in open(asked)] if os.path.exists(asked) else []
check("it is asked about without the PC, which the launch ahead waits for", usable[:1] == [["host-a", "host-b"]], str(usable))
check("room is held for it on the host that was picked, past the PC's line", line["h1"]["state"] == "offered" and line["h1"]["machine"] == "host-b"
      and line["p1"]["state"] == "waiting", str(line))
notices = [json.loads(l) for l in open(told) if l.strip()]
check("its launcher is told which machine", [(n["entry"]["launcher"], n["entry"]["machine"]) for n in notices if n["event"] == "offer"][:1] == [("thread-C", "host-b")], str(notices))
code, out = fs("queue", "claim", "--key", "h1")
check("sent again, it leaves the line for that machine", code == 0 and json.loads(out).get("machine") == "host-b", out)
code, out = fs("queue", "claim", "--key", "p1")
check("a launch still waiting is told its place", code == 1 and json.loads(out).get("state") == "waiting", out)
code, out = fs("queue", "add", "--key", "p1", "--launcher", "thread-A", "--label", "[pc] needs this PC", "--machines", "pc,host-b")
line = {e["key"]: e for e in json.loads(fs("queue", "--json")[1])}
check("a waiting launch sent again with more machines keeps its place and gains them", json.loads(out).get("place") == 1
      and line["p1"]["machines"] == ["pc", "host-b"], out)

# Urgent first. Two Owl gates fill 17 of the 20 agent processors; Jon's and Windows' own work on
# them is 1. Owl is a stand-in that writes down what it was told, so the real one is never asked.
told_owl = os.path.join(home, "owl-told.txt")
fake_owl = os.path.join(home, "owl.cmd")
open(fake_owl, "w").write("@echo %%* >> \"%s\"\r\n" % told_owl)
json.dump({"owl_path": fake_owl}, open(os.path.join(home, "settings.json"), "w"))
for name in ("queue.json", "grants.json", "urgent.json"):
    if os.path.exists(os.path.join(home, name)):
        os.remove(os.path.join(home, name))
# What the meter learned: 400 minutes of AppForGutters threads at 0.9 processors, 400 of Owl threads at 5.
at = datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")
json.dump({"thread|appforgutters|": {"at": at, "minutes": 400, "cores": 360, "sq": 324, "mem_gb": 600, "mem_peak": 1.5},
           "thread|owl-go|": {"at": at, "minutes": 400, "cores": 2000, "sq": 10000, "mem_gb": 3000, "mem_peak": 7.5}},
          open(os.path.join(home, "learned.json"), "w"))
reading(19, firm=1)
code, out = fs("room", "--threads", "4", "--take", "--repo", "owl-go", "--label", "Port the engine")
check("an ordinary launch is refused while the gates fill the PC, counted as an Owl thread", code == 1 and "no room" in out
      and "owl-go threads here were measured at 5.0 processors" in out.lower(), out)
code, out = fs("room", "--threads", "4", "--take", "--repo", "appforgutters", "--label", "AppForGutters production fix: uploads failing")
check("a production fix gets room, counted as what its repo's threads were measured using", code == 0
      and "appforgutters threads here were measured at 0.9" in out and "production" in out, out)
if os.name == "nt":
    check("and the answer says the other agent work is held at idle priority", "held at idle priority" in out and "degraded" not in out, out)
    hold = json.load(open(os.path.join(home, "urgent.json")))
    said = open(told_owl).read() if os.path.exists(told_owl) else ""
    check("Owl is told to hold older sessions at idle, for three minutes", "agents set yield_before %d --for 3m --real-home" % int(datetime.fromisoformat(hold["since"].replace("Z", "+00:00")).timestamp()) in said, said)
else:
    # This reading says Windows, but the program asked is the Linux one, and nothing holds agent
    # work back there: a hold that nothing keeps is not said and not recorded.
    check("where nothing can hold the other work back, the answer says so and no hold is recorded", "degraded: Owl was not told" in out
          and "held at idle priority" not in out and not os.path.exists(os.path.join(home, "urgent.json")), out)
decided = [json.loads(l) for l in open(os.path.join(home, "decisions.jsonl")) if l.strip()]
check("the decision is recorded as urgent", decided[-1].get("urgent") is True and decided[-1].get("repo") == "appforgutters", str(decided[-1]))
reading(19, firm=18.5)
code, out = fs("room", "--urgent", "--take", "--label", "second fix")
check("urgent does not pass work that will not step aside", code == 1 and "Even with other agent work held back" in out, out)
# Break it: Owl is missing, so nothing can hold the rest back, and the answer has to say so.
json.dump({"owl_path": os.path.join(home, "no-owl.exe")}, open(os.path.join(home, "settings.json"), "w"))
reading(19, firm=1)
code, out = fs("room", "--urgent", "--take", "--label", "third fix")
check("with no Owl to tell, the urgent answer says degraded", code == 0 and "degraded: Owl was not told" in out, out)

# Memory. Jon's PC on 2026-11-11, when it refused launches: 64 GB with 36 free, and seven threads
# open that hold 0.5 to 6 GB each. Each thread is written as the meter writes it: what it holds,
# what it is expected to hold, how far its minutes stray, and the most it is expected to reach.
for name in ("queue.json", "grants.json", "urgent.json", "settings.json"):
    if os.path.exists(os.path.join(home, name)):
        os.remove(os.path.join(home, name))
shutil.rmtree(os.path.join(home, "queue"), ignore_errors=True)
since = datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")


def piece(kind, repo, what, holds, expected, swing, most):
    return {"kind": kind, "repo": repo, "what": what, "since": since, "now": 0.2, "cores": 0.5, "minutes": 30, "expect": 0.5, "swing": 0.5,
            "mem_gb": holds, "expect_mem_gb": expected, "mem_swing": swing, "mem_most": most, "processes": 4}


seven = [piece("thread", "owl-go", "keeper", 0.6, 2.2, 3.4, 14.3), piece("thread", "owl-go", "frames", 0.5, 2.2, 3.4, 14.2),
         piece("thread", "owl-go", "port-engine", 2.0, 2.6, 2.4, 7.7), piece("thread", "owl-go", "desktop-tests", 6.0, 2.1, 2.1, 8.9),
         piece("thread", "appforgutters", "uploads", 0.5, 0.6, 0.3, 0.7), piece("thread", "appforgutters", "invoices", 0.8, 0.8, 0.2, 0.9),
         piece("thread", "fairshare", "memory", 1.2, 1.2, 0.5, 1.9)]
# What the meter learned there: a thread holds 3 GB on average, give or take 4, and the most one
# ever held was 21.2; an Owl thread 2.7, give or take 3.7, and 18.4 at its most.
json.dump({"thread||": {"at": at, "minutes": 1900, "cores": 2200, "sq": 3500, "mem_gb": 5700, "mem_sq": 47500, "mem_peak": 21.2},
           "thread|owl-go|": {"at": at, "minutes": 700, "cores": 1150, "sq": 2300, "mem_gb": 1890, "mem_sq": 14686, "mem_peak": 18.4}},
          open(os.path.join(home, "learned.json"), "w"))
reading(6, free=36, work=seven)
code, out = fs("room", "--threads", "7", "--repo", "gutter-quotes", "--json")
v = json.loads(out) if out.startswith("{") else {"mem": {}, "sized": {}, "open": {}}
m = v["mem"]
at_their_most = sum(w["mem_most"] - w["mem_gb"] for w in seven)
check("the rule before would refuse it: every open thread at its most at once is %.1f GB more, and the new thread %.1f, on 28.0 in use: %.1f of the 56 that may be taken"
      % (at_their_most, v["sized"].get("mem_most", 0), 28 + at_their_most + v["sized"].get("mem_most", 0)),
      at_their_most > 35 and v["sized"].get("mem_most", 0) > 21 and 28 + at_their_most + v["sized"]["mem_most"] > 56, out)
check("36 of 64 GB free with seven threads open takes a thread: %.1f GB more is counted for the open threads and %.1f for the new one, %.1f of 56"
      % (m.get("pending", 0), m.get("need", 0), m.get("used", 0) + m.get("pending", 0) + m.get("need", 0)),
      code == 0 and v["ok"] and m["ok"] and 11 <= m["pending"] <= 13 and 5 <= m["need"] <= 6 and abs(v["open"]["mem_kept"] - 11.59) < 0.06
      and abs(v["open"]["mem_reach"] - 12.1) < 0.06, out)
code, out = fs("status")
check("and `fairshare status` shows what was counted", "holds 11.6 GB, expected to hold 11.7, 11.6 kept for when it holds more" in out, out)
# The rarest minute of one piece lands in the 8 GB the PC keeps free, so it is not kept a second
# time on top of it: with 26 GB free the launch that was refused while it was kept twice goes in.
reading(6, free=26, work=seven)
code, out = fs("room", "--threads", "7", "--repo", "gutter-quotes", "--json")
v = json.loads(out) if out.startswith("{") else {"mem": {}, "sized": {}, "open": {}}
m = v["mem"]
kept_twice = 38 + (11.7 + 12.1 - 11.6) + 3 + (18.2 - 12.1)
check("with 26 GB free the most a new thread reaches still fits in the PC (38 in use, %.1f more counted, of 56): let in, where keeping it on top of the reserve made %.1f"
      % (m.get("pending", 0) + m.get("need", 0), kept_twice), code == 0 and v["ok"] and kept_twice > 56
      and 38 + (11.7 - 11.6) + v["sized"]["mem_most"] <= 64, out)
reading(6, free=24, work=seven)
code, out = fs("room", "--threads", "7", "--repo", "gutter-quotes")
check("with 24 GB free the seven's ordinary swing would land in the memory that is kept free: refused, and the answer says what was counted", code == 1
      and "24.0 GB of 64 GB of memory is free, 8.0 GB is kept free" in out and "(never every piece at its most at once)" in out
      and "The furthest one piece was seen reaching is 12.1 GB above that: the memory kept free takes the first of it." in out, out)
# Really out of memory, with the same 36 GB free: four builds let in a minute ago hold 0.4 GB each
# so far and are measured at 14. Nothing here is a peak; it is what they are on their way to.
reading(6, free=36, work=[piece("run", "owl-go", "cargo test --workspace", 0.4, 14, 1, 15) for _ in range(4)])
code, out = fs("room", "--threads", "3", "--repo", "gutter-quotes")
check("four builds that have not taken their memory yet leave no room, however free the PC looks", code == 1
      and "The 4 pieces of work open here hold 1.6 GB and are expected to hold 56.0" in out, out)
# And a run that holds more than is free for as long as it runs.
json.dump({"run|owl-go|cargo test --workspace": {"at": at, "minutes": 300, "cores": 600, "sq": 1200, "mem_gb": 9000, "mem_sq": 270000, "mem_peak": 30.5, "works": 30, "seconds": 18000}},
          open(os.path.join(home, "learned.json"), "w"))
reading(6, free=36)
code, out = fs("room", "--kind", "run", "--repo", "owl-go", "--what", "cargo test --workspace")
check("a build measured at 30 GB does not go into 28", code == 1 and "this run counts as 30." in out and "processors agent work may use" not in out, out)
# Break it: with learn_mem_margin so high that every piece is kept at its most, the rule is the
# old one again, and the evening's launch is refused.
json.dump({"thread||": {"at": at, "minutes": 1900, "cores": 2200, "sq": 3500, "mem_gb": 5700, "mem_sq": 47500, "mem_peak": 21.2},
           "thread|owl-go|": {"at": at, "minutes": 700, "cores": 1150, "sq": 2300, "mem_gb": 1890, "mem_sq": 14686, "mem_peak": 18.4}},
          open(os.path.join(home, "learned.json"), "w"))
json.dump({"learn_mem_margin": 1000}, open(os.path.join(home, "settings.json"), "w"))
reading(6, free=36, work=seven)
code, out = fs("room", "--threads", "7", "--repo", "gutter-quotes")
check("kept at every piece's most, the same launch is refused as it was that evening", code == 1 and "a new thread counts as 21.1 GB" in out and "on top of 37.0 GB" in out, out)

shutil.rmtree(home, ignore_errors=True)
print("failures:", fails)
sys.exit(1 if fails else 0)
