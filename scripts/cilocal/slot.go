package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// At most CI_LOCAL_SLOTS (default 1) full runs at once on a machine, across
// every worktree, agent and repository cloned from this template: another
// run waits, says so, and starts when a slot frees, never failing for it.
// A slot is an flock on a file in the user's cache directory, so a run that
// dies frees its slot with it.

// slotWaitLog is how often a waiting run says it is still waiting.
const slotWaitLog = time.Minute

// slotPoll is how often a waiting run tries the slots again.
const slotPoll = 2 * time.Second

func slotDir() (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("slots: %w", err)
	}
	dir := filepath.Join(cache, "cilocal", "slots")
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return "", fmt.Errorf("slots: %w", err)
	}
	return dir, nil
}

func slotCount() int {
	if count, err := strconv.Atoi(os.Getenv("CI_LOCAL_SLOTS")); err == nil && count > 0 {
		return count
	}
	return 1
}

// tryAnySlot takes a free slot without waiting; nil when every slot is held.
func tryAnySlot() (*os.File, error) {
	dir, err := slotDir()
	if err != nil {
		return nil, err
	}
	for index := range slotCount() {
		file, err := os.OpenFile(filepath.Join(dir, fmt.Sprintf("slot-%d.lock", index)), os.O_CREATE|os.O_RDWR, fileMode)
		if err != nil {
			return nil, fmt.Errorf("slots: %w", err)
		}
		if syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
			_ = file.Truncate(0)
			_, _ = fmt.Fprintf(file, "%d %s\n", os.Getpid(), time.Now().Format(time.RFC3339))
			return file, nil
		}
		_ = file.Close()
	}
	return nil, nil
}

// takeSlot waits for a slot; release it with releaseSlot.
func takeSlot(ctx context.Context) (*os.File, error) {
	started := time.Now()
	lastSaid := time.Time{}
	for {
		file, err := tryAnySlot()
		if err != nil || file != nil {
			return file, err
		}
		if time.Since(lastSaid) >= slotWaitLog {
			logf("waiting for a free run slot (another make ci-local holds the CI_LOCAL_SLOTS=%d on this machine; %s so far)", slotCount(), time.Since(started).Round(time.Second))
			lastSaid = time.Now()
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("slots: %w", ctx.Err())
		case <-time.After(slotPoll):
		}
	}
}

func releaseSlot(file *os.File) {
	if file != nil {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}
}
