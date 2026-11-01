package main

// A jobSet holds one piece of work in jobs: a main job and, for each process already inside some
// other job, a side job of its own. Windows refuses a process that is already in another job
// unless the new job is empty or within that job's tree: the Store build of PowerShell always
// runs in its package's job, and a program that made its own job (a `fairshare exec`) is in that
// one. Any of them can join a new empty job, which Windows nests inside the job it is in, and its
// children follow it. So nothing the work starts escapes being counted, prioritized, capped or
// stopped.

import (
	"slices"

	"golang.org/x/sys/windows"
)

type sideJob struct {
	h       windows.Handle
	release func()
}

type jobSet struct {
	main    windows.Handle
	side    []sideJob
	newSide func() (windows.Handle, func(), error) // an empty job with the same limits as main
	closed  JobStats                                 // side jobs already emptied and released
}

// handles lists the set's jobs, main first.
func (s *jobSet) handles() []windows.Handle {
	hs := []windows.Handle{s.main}
	for _, j := range s.side {
		hs = append(hs, j.h)
	}
	return hs
}

// has says whether the process is in one of the set's jobs.
func (s *jobSet) has(h windows.Handle) bool {
	for _, j := range s.handles() {
		if inJob(h, j) {
			return true
		}
	}
	return false
}

// place puts a process in the main job or a new side job. It returns the side job's handle (0
// when the main job took it), or the error when Windows refused both.
//
// The main job only takes a process that is in no job. Putting a process that is already in job X
// into an empty job nests that job inside X, and everything that joins it later is then inside X
// too: if X kills its processes when it closes (a test runner's does), finishing that one run
// would kill the rest of the chat's work. A side job nests where its process already lives, which
// is right, since only that process's own tree goes in it.
func (s *jobSet) place(h windows.Handle) (windows.Handle, error) {
	var err error
	if !inJob(h, 0) {
		if err = windows.AssignProcessToJobObject(s.main, h); err == nil {
			return 0, nil
		}
	}
	side, release, err2 := s.newSide()
	if err2 != nil {
		return 0, or2(err, err2)
	}
	if err2 = windows.AssignProcessToJobObject(side, h); err2 != nil {
		release()
		return 0, err2
	}
	s.side = append(s.side, sideJob{side, release})
	return side, nil
}

// pids lists every live process in the set, and releases side jobs whose processes have all ended
// (their totals stay in stats).
func (s *jobSet) pids() []uint32 {
	out := jobPIDs(s.main)
	s.side = slices.DeleteFunc(s.side, func(j sideJob) bool {
		ps := jobPIDs(j.h)
		out = append(out, ps...)
		if len(ps) > 0 {
			return false
		}
		s.closed = addStats(s.closed, jobStats(j.h))
		j.release()
		return true
	})
	return out
}

// stats adds up every job the set has had.
func (s *jobSet) stats() JobStats {
	t := s.closed
	for _, j := range s.handles() {
		t = addStats(t, jobStats(j))
	}
	return t
}

// release lets go of every side job (the main job is the owner's).
func (s *jobSet) release() {
	for _, j := range s.side {
		j.release()
	}
	s.side = nil
}

func or2(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

func addStats(a, b JobStats) JobStats {
	a.CPUSeconds += b.CPUSeconds
	a.Processes += b.Processes
	a.ReadMB += b.ReadMB
	a.WriteMB += b.WriteMB
	a.AllIOMB += b.AllIOMB
	a.PeakMB = max(a.PeakMB, b.PeakMB)
	return a
}
