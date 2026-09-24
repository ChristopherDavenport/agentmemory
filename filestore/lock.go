package filestore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// claimSuffix names the file a writer links the lock to while it takes
// a dead holder's lock over: lockName + claimSuffix + the holder's
// fingerprint. See [takeover].
const claimSuffix = ".taken-"

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
		mine, taken, err := createLock(path)
		if err != nil {
			return nil, err
		}
		if taken {
			return func() { releaseLock(path, mine) }, nil
		}
		holder, herr := readLock(path)
		switch {
		case herr != nil && errors.Is(herr, os.ErrNotExist):
			continue // released between our attempt and the read
		case herr == nil && holder.stale():
			gone, err := takeover(path, holder)
			if err != nil {
				return nil, err
			}
			if gone {
				// The stale lock is removed; the next create decides
				// which of the writers that found it holds the store.
				continue
			}
			// Another writer is taking the same lock over, or it has
			// been taken over already: look again rather than assume.
		}
		// A live holder, a takeover in flight, or a lock whose holder
		// cannot be read yet because it is still being written: wait.
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

// createLock creates the lock file and writes this process's identity
// into it. It reports false when another holder has it.
func createLock(path string) (LockInfo, bool, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return LockInfo{}, false, nil
		}
		return LockInfo{}, false, fmt.Errorf("filestore: create lock %s: %w", path, err)
	}
	host, _ := os.Hostname()
	info := LockInfo{PID: os.Getpid(), Host: host, Since: time.Now().UTC().Round(0)}
	werr := json.NewEncoder(f).Encode(info)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		os.Remove(path)
		return LockInfo{}, false, fmt.Errorf("filestore: write lock %s: %w", path, werr)
	}
	return info, true, nil
}

// takeover removes a lock whose holder is dead, and reports whether it
// is gone. Removing it is the one step that must be exclusive: a writer
// that removes a lock it has not proved is the dead one removes the
// lock a writer that got there first now holds, and since the journal's
// next sequence number is read from its tail under this lock, both then
// append the same one and a reader resuming from it never sees one of
// the writes.
//
// os.Link is the only exclusive primitive the standard library offers
// on both Linux and macOS: it fails when the new name exists. So the
// taker links the lock to a name derived from the dead holder's
// identity, which exactly one writer's link can create, and then reads
// that name to prove it linked the lock it inspected rather than a
// fresh one a faster taker had already put there. Only that writer
// removes the lock, and only the holder of a name nobody else can hold
// may do so, which makes the removal a compare-and-swap. A writer that
// loses the link, or that linked a lock which had moved on, changes
// nothing and looks again.
//
// A taker that dies between the link and the removal leaves the claim
// beside the lock; nothing is lost and nothing is written twice, but
// that dead holder's lock is no longer taken over, so writes report
// [ErrLocked] naming it until [Store.BreakLock], which removes both.
func takeover(path string, holder LockInfo) (bool, error) {
	claim := path + claimSuffix + holder.fingerprint()
	if err := os.Link(path, claim); err != nil {
		switch {
		case errors.Is(err, os.ErrExist):
			return false, nil // another writer is taking this lock over
		case errors.Is(err, os.ErrNotExist):
			return true, nil // released or taken over between the read and the link
		}
		// A file system with no links to give, or a directory this
		// process may not write: the lock cannot be taken over here,
		// and a writer that cannot take it over is a writer waiting on
		// a holder it will never outlive. Say so as [ErrLocked], which
		// names the holder and points at BreakLock, rather than as an
		// error about a file the caller did not ask for.
		return false, fmt.Errorf("%w: %s held by pid %d on %s since %s, which no longer runs, and the lock cannot be claimed (%v); BreakLock removes it",
			ErrLocked, path, holder.PID, holder.Host, holder.Since.Format(time.RFC3339), err)
	}
	// The claim names whatever the lock named at the moment of the
	// link. When that is not the holder read a moment ago, a faster
	// writer has taken the lock over and this is a second name for its
	// lock, which must not be removed; dropping the name leaves it
	// whole.
	again, err := readLock(claim)
	if err != nil || !again.same(holder) {
		os.Remove(claim)
		return false, nil
	}
	rerr := os.Remove(path)
	os.Remove(claim)
	if rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
		return false, fmt.Errorf("filestore: remove stale lock %s: %w", path, rerr)
	}
	return true, nil
}

// releaseLock removes the lock when it is still, provably, this
// holder's. A lock taken over while this process held it belongs to
// its new holder, and one being written by a new holder reads as
// empty; removing either would let two writers write at once. A lock
// this writer cannot prove is its own is left to go stale, which the
// next writer on this host takes over.
func releaseLock(path string, mine LockInfo) {
	info, err := readLock(path)
	if err != nil || !info.same(mine) {
		return
	}
	os.Remove(path)
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

// same reports whether two reads of a lock file name one holder. A
// host and a PID repeat; the start time, to the nanosecond, does not.
func (l LockInfo) same(o LockInfo) bool {
	return l.PID == o.PID && l.Host == o.Host && l.Since.Equal(o.Since)
}

// fingerprint is a short, file-name-safe digest of the holder's
// identity, so the name a taker claims is derived from the lock it is
// taking over and from nothing else.
func (l LockInfo) fingerprint() string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s|%d|%d", l.Host, l.PID, l.Since.UnixNano()))
	return hex.EncodeToString(sum[:8])
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

// BreakLock removes the store's lock whoever holds it, and any claim a
// takeover left behind. Use it when a write reports [ErrLocked] and the
// caller has confirmed the holder is gone, for example a process on
// another host that crashed. Breaking the lock of a live writer lets
// two processes write at once.
func (s *Store) BreakLock() error {
	if err := os.Remove(s.lockPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("filestore: remove lock: %w", err)
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("filestore: read %s: %w", s.dir, err)
	}
	for _, de := range entries {
		if !strings.HasPrefix(de.Name(), lockName+claimSuffix) {
			continue
		}
		if err := os.Remove(filepath.Join(s.dir, de.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("filestore: remove takeover claim: %w", err)
		}
	}
	return nil
}
