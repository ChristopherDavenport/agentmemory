package filestore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// ErrLocked is returned by a write when another process held the
// store's lock for longer than the store waits. The error's message
// names the holder; [Store.BreakLock] removes a lock the caller has
// decided is stale.
var ErrLocked = errors.New("filestore: store is locked by another process")

// LockInfo describes the holder of the store's lock.
type LockInfo struct {
	PID   int       `json:"pid"`
	Host  string    `json:"host,omitempty"`
	Since time.Time `json:"since"`
}

// DefaultLockTimeout is how long a write waits for a live holder to
// release the lock before returning [ErrLocked]. A write holds the
// lock for one file write, so the wait is for a burst of writers, not
// a session.
const DefaultLockTimeout = 2 * time.Second

// lockName is the lock file at the store's root. The leading dot keeps
// it out of the scope directories' entries, which are kebab-case.
const lockName = ".lock"

// acquire takes the store's lock: the in-process mutex first, so two
// handles in one process do not contend on the file, then the lock
// file. A lock left by a process on this host that no longer runs is
// taken over; a live holder is waited for until ctx is done or the
// timeout passes, and then reported as [ErrLocked] with its identity.
// The returned function releases both.
func (s *Store) acquire(ctx context.Context) (func(), error) {
	s.mu.Lock()
	release, err := acquireFile(ctx, s.lockPath(), s.lockTimeout)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	return func() {
		release()
		s.mu.Unlock()
	}, nil
}

func acquireFile(ctx context.Context, path string, timeout time.Duration) (func(), error) {
	deadline := time.Now().Add(timeout)
	wait := time.Millisecond
	for {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			host, _ := os.Hostname()
			info := LockInfo{PID: os.Getpid(), Host: host, Since: time.Now().UTC().Round(0)}
			werr := json.NewEncoder(f).Encode(info)
			if cerr := f.Close(); werr == nil {
				werr = cerr
			}
			if werr != nil {
				os.Remove(path)
				return nil, fmt.Errorf("filestore: write lock %s: %w", path, werr)
			}
			return func() { os.Remove(path) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("filestore: create lock %s: %w", path, err)
		}
		holder, herr := readLock(path)
		switch {
		case herr != nil && errors.Is(herr, os.ErrNotExist):
			continue // released between our attempt and the read
		case herr == nil && holder.stale():
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("filestore: remove stale lock %s: %w", path, err)
			}
			continue
		}
		// A live holder, or a lock whose holder cannot be read yet
		// because it is still being written: wait.
		if time.Now().After(deadline) {
			if herr != nil {
				return nil, fmt.Errorf("%w: %s (unreadable lock: %v)", ErrLocked, path, herr)
			}
			return nil, fmt.Errorf("%w: %s held by pid %d on %s since %s", ErrLocked, path, holder.PID, holder.Host, holder.Since.Format(time.RFC3339))
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
		if wait < 50*time.Millisecond {
			wait *= 2
		}
	}
}

func readLock(path string) (LockInfo, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return LockInfo{}, err
	}
	var info LockInfo
	if len(data) == 0 {
		// Created but not yet written by its holder.
		return info, errors.New("empty")
	}
	if err := json.Unmarshal(data, &info); err != nil {
		return LockInfo{}, err
	}
	return info, nil
}

// stale reports whether the holder is a process on this host that no
// longer runs. A holder on another host is never stale: this process
// cannot tell, and the caller must break the lock deliberately.
func (l LockInfo) stale() bool {
	host, _ := os.Hostname()
	if l.Host != host || l.PID <= 0 {
		return false
	}
	return !processAlive(l.PID)
}

// LockHolder reports who holds the store's lock, or nil when it is
// free. It reads the lock file, so a write in progress in this process
// is reported as held by this process.
func (s *Store) LockHolder() (*LockInfo, error) {
	info, err := readLock(s.lockPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("filestore: read lock: %w", err)
	}
	return &info, nil
}

// BreakLock removes the store's lock whoever holds it. Use it when a
// write reports [ErrLocked] and the caller has confirmed the holder is
// gone, for example a process on another host that crashed. Breaking
// the lock of a live writer lets two processes write at once.
func (s *Store) BreakLock() error {
	if err := os.Remove(s.lockPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("filestore: remove lock: %w", err)
	}
	return nil
}
