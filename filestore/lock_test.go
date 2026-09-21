package filestore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentmemory"
)

// deadPID returns the PID of a process that has run and exited, so a
// lock naming it is stale on this host.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Skipf("no /bin/sh to make a dead PID with: %v", err)
	}
	return cmd.Process.Pid
}

// plantStaleLock writes a lock file naming a holder that is dead on
// this host, as a writer killed mid-write leaves behind.
func plantStaleLock(t *testing.T, dir string, pid int) {
	t.Helper()
	host, _ := os.Hostname()
	data, err := json.Marshal(LockInfo{PID: pid, Host: host, Since: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, lockName), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

// raceRounds runs rounds of writers against a fresh store, each writer
// with its own handle as a separate process would have, with or without
// a dead holder's lock waiting for them. It returns how many rounds
// ended with a repeated sequence number in the journal, how many
// records were repeated over all, and how many records were written.
func raceRounds(t *testing.T, rounds, writers int, planted bool) (dupRounds, dupSeqs, records int) {
	t.Helper()
	ctx := context.Background()
	pid := 0
	if planted {
		pid = deadPID(t)
	}
	for round := 0; round < rounds; round++ {
		dir := filepath.Join(t.TempDir(), "memory")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if planted {
			plantStaleLock(t, dir, pid)
		}
		var wg sync.WaitGroup
		errs := make(chan error, writers)
		for w := 0; w < writers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				s, err := Open(dir, WithLockTimeout(5*time.Second))
				if err != nil {
					errs <- err
					return
				}
				if _, err := s.Put(agentmemory.WithSession(ctx, fmt.Sprintf("w%d", w)),
					agentmemory.Entry{Scope: "user", Name: fmt.Sprintf("n-%d", w), Content: "x"}); err != nil {
					errs <- fmt.Errorf("writer %d: %w", w, err)
				}
			}(w)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Errorf("round %d: %v", round, err)
		}
		s, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[uint64]int{}
		n := 0
		for c, err := range s.Journal(ctx, 0) {
			if err != nil {
				t.Errorf("round %d: journal: %v", round, err)
				continue
			}
			seen[c.Seq]++
			n++
		}
		records += n
		dup := 0
		for _, k := range seen {
			if k > 1 {
				dup += k - 1
			}
		}
		if dup > 0 {
			dupRounds++
			dupSeqs += dup
			if dupRounds == 1 {
				t.Logf("round %d: %d journal records, %d distinct sequence numbers", round, n, len(seen))
			}
		}
	}
	return dupRounds, dupSeqs, records
}

// TestTakeoverRace is the regression for the race the round 2 probe
// found: several writers that find one dead holder's lock at the same
// time must not all take it over, because the sequence number is read
// from the journal's tail under that lock and two holders then append
// the same one. A reader resuming from a repeated sequence number never
// sees one of the writes.
func TestTakeoverRace(t *testing.T) {
	const rounds, writers = 40, 8
	dupRounds, dupSeqs, records := raceRounds(t, rounds, writers, true)
	t.Logf("with a dead holder's lock: %d rounds of %d writers, %d records, %d rounds repeated a sequence number, %d records over all",
		rounds, writers, records, dupRounds, dupSeqs)
	if dupRounds != 0 {
		t.Errorf("%d of %d rounds repeated a sequence number (%d records); the takeover is not atomic", dupRounds, rounds, dupSeqs)
	}
	if want := rounds * writers; records != want {
		t.Errorf("journals hold %d records over all, want %d; a write was lost", records, want)
	}
}

// TestTakeoverRaceControl is the same writers without a lock to take
// over: the control the probe reported 0 for in every run.
func TestTakeoverRaceControl(t *testing.T) {
	const rounds, writers = 16, 8
	dupRounds, dupSeqs, records := raceRounds(t, rounds, writers, false)
	t.Logf("with no lock waiting: %d rounds of %d writers, %d records, %d rounds repeated a sequence number, %d records over all",
		rounds, writers, records, dupRounds, dupSeqs)
	if dupRounds != 0 {
		t.Errorf("%d of %d rounds repeated a sequence number (%d records)", dupRounds, rounds, dupSeqs)
	}
}

// TestTakeoverCannotClaim is the file system that gives no links, or
// the directory this process may not write: a stale lock cannot be
// taken over there, and a writer must be told so as ErrLocked, which
// names the holder and points at BreakLock, rather than with an error
// about a file it never asked for.
func TestTakeoverCannotClaim(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes a read-only directory")
	}
	dir := filepath.Join(t.TempDir(), "memory")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	pid := deadPID(t)
	plantStaleLock(t, dir, pid)
	s, err := Open(dir, WithLockTimeout(30*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	_, err = s.Put(context.Background(), agentmemory.Entry{Scope: "user", Name: "a", Content: "x"})
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("Put over a lock that cannot be claimed = %v, want ErrLocked", err)
	}
	for _, want := range []string{fmt.Sprintf("pid %d", pid), "BreakLock"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// TestReleaseKeepsAnotherHolder checks that a writer releases only a
// lock it can prove is its own: one taken over while it held it, or
// one another writer is part way through creating, belongs to that
// writer, and removing it would let two writers write at once.
func TestReleaseKeepsAnotherHolder(t *testing.T) {
	host, _ := os.Hostname()
	other := LockInfo{PID: os.Getpid(), Host: host, Since: time.Now().UTC().Add(-time.Minute)}
	tests := []struct {
		name  string
		write func(t *testing.T, path string)
	}{
		{"taken over while we held it", func(t *testing.T, path string) {
			data, err := json.Marshal(other)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"half written by its new holder", func(t *testing.T, path string) {
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := open(t)
			release, err := s.acquire(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			tt.write(t, s.lockPath())
			release()
			if _, err := os.Stat(s.lockPath()); err != nil {
				t.Errorf("the release removed another holder's lock: %v", err)
			}
			if err := s.BreakLock(); err != nil {
				t.Fatal(err)
			}
		})
	}
	// A lock that is still ours is removed, as every other test relies
	// on.
	s := open(t)
	release, err := s.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	release()
	if h, _ := s.LockHolder(); h != nil {
		t.Errorf("the release left our own lock behind: %+v", h)
	}
}

// TestTakeoverIsExclusive checks the takeover directly: with a dead
// holder's lock planted, one writer takes it over and the others find
// the lock it now holds rather than a free path.
func TestTakeoverIsExclusive(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "memory")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	plantStaleLock(t, dir, deadPID(t))
	s, err := Open(dir, WithLockTimeout(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	release, err := s.acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire over a dead holder's lock: %v", err)
	}
	// The lock now names this process, and a second handle waits for it
	// rather than taking it over again.
	holder, err := s.LockHolder()
	if err != nil || holder == nil || holder.PID != os.Getpid() {
		t.Fatalf("LockHolder after the takeover = %+v, %v", holder, err)
	}
	other, err := Open(dir, WithLockTimeout(30*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Put(context.Background(), agentmemory.Entry{Scope: "user", Name: "a", Content: "x"}); err == nil {
		t.Error("a second writer took over a live holder's lock")
	}
	release()
	if h, _ := s.LockHolder(); h != nil {
		t.Errorf("lock left behind after release: %+v", h)
	}
	// No claim files are left beside it.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, de := range entries {
		if strings.HasPrefix(de.Name(), lockName+".") {
			t.Errorf("takeover left %s behind", de.Name())
		}
	}
}
