package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Noisemaker111/fairshare/internal/windows"
)

// A held write handle marks a live waiter; Windows closes it when the waiter dies.
type slotTicket struct {
	file *os.File
	name string
}

func openSlotQueueFile(path string, creation uint32) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_WRITE, windows.FILE_SHARE_READ, nil, creation, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), path), nil
}

func lockSlotQueue(start time.Time, maxWait time.Duration) (*os.File, error) {
	for {
		f, err := openSlotQueueFile(filepath.Join(slotsDir(), "queue.lock"), windows.OPEN_ALWAYS)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, syscall.Errno(32)) {
			return nil, err
		}
		if maxWait > 0 && time.Since(start) >= maxWait {
			return nil, errBusy
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// Under the queue lock, remove only tickets whose owners no longer hold them.
func liveSlotTickets() ([]string, error) {
	entries, err := os.ReadDir(slotsDir())
	if err != nil {
		return nil, err
	}
	var live []string
	for _, entry := range entries {
		name := entry.Name()
		if len(name) != 25 || !strings.HasSuffix(name, ".wait") {
			continue
		}
		if _, err := strconv.ParseUint(name[:20], 10, 64); err != nil {
			continue
		}
		path := filepath.Join(slotsDir(), name)
		stale, err := openSlotQueueFile(path, windows.OPEN_EXISTING)
		if err == nil {
			stale.Close()
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return nil, err
			}
			continue
		}
		if os.IsNotExist(err) {
			continue
		}
		if !errors.Is(err, syscall.Errno(32)) {
			return nil, err
		}
		live = append(live, name)
	}
	return live, nil // os.ReadDir returns filename order.
}

func newSlotTicket(start time.Time, maxWait time.Duration) (*slotTicket, error) {
	gate, err := lockSlotQueue(start, maxWait)
	if err != nil {
		return nil, err
	}
	defer gate.Close()
	live, err := liveSlotTickets()
	if err != nil {
		return nil, err
	}
	var next uint64 = 1
	if len(live) > 0 {
		n, err := strconv.ParseUint(live[len(live)-1][:20], 10, 64)
		if err != nil || n == ^uint64(0) {
			return nil, fmt.Errorf("fairshare slot queue: ticket sequence exhausted")
		}
		next = n + 1
	}
	name := fmt.Sprintf("%020d.wait", next)
	f, err := openSlotQueueFile(filepath.Join(slotsDir(), name), windows.CREATE_NEW)
	if err != nil {
		return nil, fmt.Errorf("fairshare slot queue: create ticket: %w", err)
	}
	return &slotTicket{file: f, name: name}, nil
}

// Call only while holding the queue lock: the filename cannot be reused before removal.
func (t *slotTicket) remove() error {
	if t.file == nil {
		return nil
	}
	err := t.file.Close()
	t.file = nil
	if removeErr := os.Remove(filepath.Join(slotsDir(), t.name)); removeErr != nil && !os.IsNotExist(removeErr) {
		return removeErr
	}
	return err
}
