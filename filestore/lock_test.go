package filestore

import (
	"context"
	"encoding/json"
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
				if err := s.Put(agentmemory.WithSession(ctx, fmt.Sprintf("w%d", w)),
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
	if err := other.Put(context.Background(), agentmemory.Entry{Scope: "user", Name: "a", Content: "x"}); err == nil {
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
