package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestTheLineMovesInOrderWhenThereIsRoom(t *testing.T) {
	s := defaultSettings()
	t0 := time.Date(2026, 11, 11, 3, 0, 0, 0, time.UTC)
	line := []queued{
		{ID: 1, Key: "a", Launcher: "m", Title: "first", Count: 1, Checkout: true, QueuedAt: t0, State: "waiting", Need: Need{Cores: 3}},
		{ID: 2, Key: "b", Launcher: "m", Title: "second", Count: 1, QueuedAt: t0.Add(time.Minute), State: "waiting", Need: Need{Cores: 3}},
		{ID: 3, Key: "c", Launcher: "n", Title: "third", Count: 1, QueuedAt: t0.Add(2 * time.Minute), State: "waiting", Need: Need{Cores: 3}},
	}
	clone := func() []queued { return append([]queued(nil), line...) }

	// A full PC: nobody is offered, nobody is told.
	q, told := advance(clone(), pc(22, 20, 30, 60, "active"), s, t0.Add(3*time.Minute), Need{}, nil)
	if len(told) != 0 || q[0].State != "waiting" {
		t.Fatalf("a full PC offered room: %+v", told)
	}
	// Room for two (limit 14.4; 11 in use, each counts as 1): the first two, in order, not the third.
	q, told = advance(clone(), pc(11.5, 11.5, 30, 60, "active"), s, t0.Add(3*time.Minute), Need{}, nil)
	if len(told) != 2 || told[0].Entry.ID != 1 || told[1].Entry.ID != 2 || told[0].Event != "offer" || q[2].State != "waiting" {
		t.Fatalf("room for two told %+v and left %+v", told, q)
	}
	// The first cannot fit (no disk for its checkout): the line stops there, though the second would fit.
	q, told = advance(clone(), pc(2, 2, 30, 16, "active"), s, t0.Add(3*time.Minute), Need{}, nil)
	if len(told) != 0 {
		t.Fatalf("a launch passed the one ahead of it: %+v", told)
	}
	// Room held for a launch counts against the next look, so it is not given away twice.
	q, _ = advance(clone(), pc(12.5, 12.5, 30, 60, "active"), s, t0.Add(3*time.Minute), Need{}, nil)
	if q[0].State != "offered" || q[1].State != "waiting" {
		t.Fatalf("room for one: %+v", q)
	}
	q, told = advance(q, pc(12.5, 12.5, 30, 60, "active"), s, t0.Add(4*time.Minute), Need{}, nil)
	if len(told) != 0 || q[1].State != "waiting" {
		t.Fatalf("the held room was offered again: %+v", told)
	}
	// An offer nobody took in the hold time goes to the back, and the next gets the room.
	q, told = advance(q, pc(12.5, 12.5, 30, 60, "active"), s, t0.Add(14*time.Minute), Need{}, nil)
	if len(told) != 1 || told[0].Entry.ID != 2 || q[len(q)-1].ID != 1 || q[len(q)-1].Offers != 1 {
		t.Fatalf("after a lapsed offer: told %+v, line %+v", told, q)
	}
	// After queue_offers untaken offers it leaves the line and its launcher is told so.
	lost := []queued{{ID: 9, Key: "z", Launcher: "m", Title: "ignored", Count: 1, QueuedAt: t0, State: "offered", OfferedAt: t0, Offers: s.QueueOffers - 1}}
	q, told = advance(lost, pc(22, 22, 30, 60, "active"), s, t0.Add(11*time.Minute), Need{}, nil)
	if len(q) != 0 || len(told) != 1 || told[0].Event != "dropped" {
		t.Fatalf("an ignored launch stayed: %+v %+v", q, told)
	}
	// A day in the line is the most.
	q, told = advance(clone(), pc(22, 22, 30, 60, "active"), s, t0.Add(25*time.Hour), Need{}, nil)
	if len(q) != 0 || len(told) != 3 {
		t.Fatalf("day-old launches stayed: %+v", q)
	}
}

func TestANewArrivalPassesTheLineOnlyWithRoomForEveryoneInIt(t *testing.T) {
	line := []queued{{ID: 1, Need: Need{Cores: 3}, State: "waiting"}, {ID: 2, Need: Need{Cores: 4}, State: "offered"}}
	if n := lineNeed(line, false); n.Cores != 7 {
		t.Fatalf("the line counts as %.0f toward a new arrival, want 7", n.Cores)
	}
	if n := lineNeed(line, true); n.Cores != 4 {
		t.Fatalf("held room counts as %.0f toward the head, want 4", n.Cores)
	}
	s := defaultSettings()
	// 5 in use, limit 14.4: alone a new thread fits (6), behind a line worth 7 it still fits (13),
	// behind one worth 9 it does not (15).
	if v := decide(pc(5, 5, 30, 60, "active"), s, Ask{}, lineNeed(line, false)); !v.OK {
		t.Fatalf("room for the line and the arrival was refused: %s", v.Reason)
	}
	line[0].Need.Cores = 5
	v := decideBehind(pc(5, 5, 30, 60, "active"), s, Ask{}, Need{Cores: 1}, lineNeed(line, false))
	if v.OK {
		t.Fatalf("an arrival took the room the line waits for: %s", v.Reason)
	}
	// The refusal says which part is the line and which is threads just let in.
	for _, want := range []string{"last 5 minutes count as 1.0 more", "ahead of it in the line count as 9.0 more"} {
		if !strings.Contains(v.Reason, want) {
			t.Fatalf("reason lacks %q: %s", want, v.Reason)
		}
	}
}

func TestTheLineIsKeptOnDiskUnderItsLock(t *testing.T) {
	t.Setenv("FAIRSHARE_HOME", t.TempDir())
	now := time.Now()
	err := withLock(func() error {
		return saveQueue([]queued{{ID: 4, Key: "k", Launcher: "m", Title: "t", Count: 1, QueuedAt: now, State: "waiting"}})
	})
	if err != nil {
		t.Fatal(err)
	}
	q := loadQueue()
	if at, e := position(q, "k"); at != 1 || e == nil || e.ID != 4 || nextID(q) != 5 {
		t.Fatalf("read back %+v", q)
	}
	if at, e := position(q, "other"); at != 0 || e != nil {
		t.Fatal("found a launch that is not in the line")
	}
}

func TestTheLineIsInOrderPerMachine(t *testing.T) {
	s := defaultSettings()
	t0 := time.Date(2026, 11, 11, 4, 0, 0, 0, time.UTC)
	hosts := []string{"pc", "t3-host", "t3-host-2"}
	line := []queued{
		{ID: 1, Key: "a", Title: "needs the PC", Count: 1, QueuedAt: t0, State: "waiting"},
		{ID: 2, Key: "b", Title: "also needs the PC", Count: 1, QueuedAt: t0, State: "waiting"},
		{ID: 3, Key: "c", Title: "runs anywhere", Count: 1, Machines: hosts, QueuedAt: t0, State: "waiting"},
		{ID: 4, Key: "d", Title: "runs anywhere too", Count: 1, Machines: hosts, QueuedAt: t0, State: "waiting"},
		{ID: 5, Key: "e", Title: "a third PC launch", Count: 1, QueuedAt: t0, State: "waiting"},
	}
	var asked [][]string
	// One host has room for one launch; the PC is full.
	room := 1
	place := func(e queued, local *Verdict, usable []string) string {
		asked = append(asked, usable)
		if local != nil && local.OK {
			return "pc"
		}
		if room > 0 && len(usable) > 0 {
			room--
			return "t3-host-2"
		}
		return ""
	}
	q, told := advance(append([]queued(nil), line...), pc(30, 30, 30, 60, "active"), s, t0.Add(time.Minute), Need{}, place)
	// The PC-only launches wait and close the PC; the first that runs anywhere goes to the host,
	// past them, and is asked without the PC; the second finds no room and closes the hosts.
	if len(told) != 1 || told[0].Entry.ID != 3 || told[0].Entry.Machine != "t3-host-2" {
		t.Fatalf("told %+v", told)
	}
	if len(asked) != 2 || len(asked[0]) != 2 || asked[0][0] != "t3-host" {
		t.Fatalf("the launch that runs anywhere was asked with %v, want the hosts only", asked)
	}
	if q[0].State != "waiting" || q[3].State != "waiting" || q[4].State != "waiting" {
		t.Fatalf("line after: %+v", q)
	}
	// Room held on a host does not count against the PC, and room held here does.
	if n := lineNeed([]queued{{State: "offered", Machine: "t3-host-2", Need: Need{Cores: 5}}, {State: "offered", Machine: "pc", Need: Need{Cores: 2}}}, true); n.Cores != 2 {
		t.Fatalf("room held counts as %.0f here, want 2", n.Cores)
	}
	// A look that does not ask the other machines keeps their place: nothing behind takes the PC.
	q, told = advance(append([]queued(nil), line[2:]...), pc(2, 2, 30, 60, "active"), s, t0.Add(time.Minute), Need{}, nil)
	if len(told) != 0 || q[2].State != "waiting" {
		t.Fatalf("a PC launch went ahead of launches that could use the PC and were not asked: %+v", told)
	}
	// When the PC has room, a launch that runs anywhere may be given it.
	q, told = advance(append([]queued(nil), line[2:3]...), pc(2, 2, 30, 60, "active"), s, t0.Add(time.Minute), Need{}, place)
	if len(told) != 1 || told[0].Entry.Machine != "pc" || q[0].Need.Cores != 1 {
		t.Fatalf("with room on the PC: %+v", told)
	}
}

// The manager's goals say which waiting launch matters most; the line follows that, and a launch
// nobody ranked stays behind the ranked ones in the order it came.
func TestTheLineFollowsTheOrderItIsGiven(t *testing.T) {
	line := []queued{{ID: 1, State: "waiting"}, {ID: 2, State: "waiting"}, {ID: 3, State: "waiting"}, {ID: 4, State: "waiting"}, {ID: 5, State: "waiting"}}
	ids := func(q []queued) string {
		out := ""
		for _, e := range q {
			out += fmt.Sprint(e.ID)
		}
		return out
	}
	if got := ids(reorder(line, []int{4, 2})); got != "42135" {
		t.Fatalf("with 4 then 2 put first the line is %s, want 42135", got)
	}
	if got := ids(reorder(line, nil)); got != "12345" {
		t.Fatalf("with no order given the line is %s, want 12345", got)
	}
	if got := ids(reorder(line, []int{9, 5, 5})); got != "51234" {
		t.Fatalf("an id not in the line, or named twice, gave %s, want 51234", got)
	}
}
