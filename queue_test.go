package main

import (
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
	q, told := advance(clone(), pc(22, 20, 30, 60, "active"), s, t0.Add(3*time.Minute), Need{})
	if len(told) != 0 || q[0].State != "waiting" {
		t.Fatalf("a full PC offered room: %+v", told)
	}
	// Room for two (limit 14.4; 11 in use, each counts as 1): the first two, in order, not the third.
	q, told = advance(clone(), pc(11.5, 11.5, 30, 60, "active"), s, t0.Add(3*time.Minute), Need{})
	if len(told) != 2 || told[0].Entry.ID != 1 || told[1].Entry.ID != 2 || told[0].Event != "offer" || q[2].State != "waiting" {
		t.Fatalf("room for two told %+v and left %+v", told, q)
	}
	// The first cannot fit (no disk for its checkout): the line stops there, though the second would fit.
	q, told = advance(clone(), pc(2, 2, 30, 16, "active"), s, t0.Add(3*time.Minute), Need{})
	if len(told) != 0 {
		t.Fatalf("a launch passed the one ahead of it: %+v", told)
	}
	// Room held for a launch counts against the next look, so it is not given away twice.
	q, _ = advance(clone(), pc(12.5, 12.5, 30, 60, "active"), s, t0.Add(3*time.Minute), Need{})
	if q[0].State != "offered" || q[1].State != "waiting" {
		t.Fatalf("room for one: %+v", q)
	}
	q, told = advance(q, pc(12.5, 12.5, 30, 60, "active"), s, t0.Add(4*time.Minute), Need{})
	if len(told) != 0 || q[1].State != "waiting" {
		t.Fatalf("the held room was offered again: %+v", told)
	}
	// An offer nobody took in the hold time goes to the back, and the next gets the room.
	q, told = advance(q, pc(12.5, 12.5, 30, 60, "active"), s, t0.Add(14*time.Minute), Need{})
	if len(told) != 1 || told[0].Entry.ID != 2 || q[len(q)-1].ID != 1 || q[len(q)-1].Offers != 1 {
		t.Fatalf("after a lapsed offer: told %+v, line %+v", told, q)
	}
	// After queue_offers untaken offers it leaves the line and its launcher is told so.
	lost := []queued{{ID: 9, Key: "z", Launcher: "m", Title: "ignored", Count: 1, QueuedAt: t0, State: "offered", OfferedAt: t0, Offers: s.QueueOffers - 1}}
	q, told = advance(lost, pc(22, 22, 30, 60, "active"), s, t0.Add(11*time.Minute), Need{})
	if len(q) != 0 || len(told) != 1 || told[0].Event != "dropped" {
		t.Fatalf("an ignored launch stayed: %+v %+v", q, told)
	}
	// A day in the line is the most.
	q, told = advance(clone(), pc(22, 22, 30, 60, "active"), s, t0.Add(25*time.Hour), Need{})
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
	if v := decide(pc(5, 5, 30, 60, "active"), s, Ask{}, lineNeed(line, false)); v.OK {
		t.Fatalf("an arrival took the room the line waits for: %s", v.Reason)
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
