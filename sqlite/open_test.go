package sqlite

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentmemory"
)

// TestFreshOpenFromProcesses opens one path that does not exist yet
// from several processes at the same instant, as a daemon starting its
// channels on first install does. A goroutine test cannot see the
// defect this pins: within one process the driver's connections share
// the file's locks, and a fresh database failed with SQLITE_BUSY only
// across processes, a millisecond in, under a five second busy timeout.
// Every child must open, write and close.
func TestFreshOpenFromProcesses(t *testing.T) {
	if os.Getenv("SQLITE_OPEN_CHILD") != "" {
		t.Skip("child process")
	}
	const rounds, procs = 5, 4
	for round := range rounds {
		path := filepath.Join(t.TempDir(), "memory.db")
		// The children wait for one wall-clock instant, so their opens
		// overlap however long each takes to start.
		at := time.Now().Add(500 * time.Millisecond).UnixNano()
		var wg sync.WaitGroup
		errs := make([]error, procs)
		outs := make([][]byte, procs)
		for p := range procs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				cmd := exec.Command(os.Args[0], "-test.run=^TestFreshOpenChild$", "-test.v")
				cmd.Env = append(os.Environ(),
					"SQLITE_OPEN_CHILD=1",
					"SQLITE_OPEN_PATH="+path,
					"SQLITE_OPEN_AT="+strconv.FormatInt(at, 10),
					"SQLITE_OPEN_NAME="+fmt.Sprintf("p%d", p))
				outs[p], errs[p] = cmd.CombinedOutput()
			}()
		}
		wg.Wait()
		for p := range procs {
			if errs[p] != nil || !strings.Contains(string(outs[p]), "PASS") {
				t.Errorf("round %d, process %d: %v\n%s", round, p, errs[p], outs[p])
			}
		}
		s := openStore(t, path)
		es, err := s.List(t.Context(), "user")
		if err != nil {
			t.Fatal(err)
		}
		if len(es) != procs {
			t.Errorf("round %d: %d entries, want one from each of %d processes", round, len(es), procs)
		}
	}
}

// TestFreshOpenChild is the child half of TestFreshOpenFromProcesses.
func TestFreshOpenChild(t *testing.T) {
	path := os.Getenv("SQLITE_OPEN_PATH")
	if os.Getenv("SQLITE_OPEN_CHILD") == "" || path == "" {
		t.Skip("run by TestFreshOpenFromProcesses")
	}
	at, err := strconv.ParseInt(os.Getenv("SQLITE_OPEN_AT"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(time.Unix(0, at)))
	start := time.Now()
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open failed after %v: %v", time.Since(start).Round(time.Millisecond), err)
	}
	defer s.Close()
	if _, err := s.Put(t.Context(), agentmemory.Entry{Scope: "user", Name: os.Getenv("SQLITE_OPEN_NAME"), Content: "opened"}); err != nil {
		t.Fatal(err)
	}
}
