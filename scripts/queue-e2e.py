# The line end to end, through the built fairshare.exe in a private home: real files, real lock,
# the real notify command run by fairshare. The reading is written by hand so "full" and "room" are
# certain whatever the PC is doing. Usage: python -I queue-e2e.py <fairshare.exe>
import json, os, shutil, subprocess, sys, tempfile
from datetime import datetime, timezone

exe = sys.argv[1]
home = tempfile.mkdtemp(prefix="fs-queue-")
env = dict(os.environ, FAIRSHARE_HOME=home)
told = os.path.join(home, "told.jsonl")
notify = os.path.join(home, "notify.py")
open(notify, "w").write("import sys\nopen(r'%s','a').write(sys.stdin.read()+'\\n')\n" % told)
json.dump({"queue_notify": [sys.executable, "-I", notify], "queue_hold_minutes": 10}, open(os.path.join(home, "settings.json"), "w"))
fails = 0


def reading(demand):
    r = {"machine": "test-pc", "os": "windows", "at": datetime.now(timezone.utc).isoformat().replace("+00:00", "Z"),
         "processors": 24, "reserved": 4, "capacity": 20, "short_seconds": 60, "long_seconds": 600,
         "busy_short": min(demand, 20), "busy_long": min(demand, 20), "waiting_short": max(0, demand - 20), "waiting_long": max(0, demand - 20),
         "demand_short": demand, "demand_long": demand, "mem_total_gb": 64, "mem_avail_gb": 30, "disk_path": "C:\\", "disk_free_gb": 60,
         "presence": {"state": "active", "idle_minutes": 0}}
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

shutil.rmtree(home, ignore_errors=True)
print("failures:", fails)
sys.exit(1 if fails else 0)
