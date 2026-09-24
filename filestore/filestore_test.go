package filestore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentmemory"
	"github.com/ChristopherDavenport/agentmemory/storetest"
)

func TestStore(t *testing.T) {
	storetest.Run(t, storetest.Options{
		New: func(t *testing.T) agentmemory.Store {
			s, err := Open(filepath.Join(t.TempDir(), "memory"))
			if err != nil {
				t.Fatal(err)
			}
			return s
		},
		Reopen: func(t *testing.T, s agentmemory.Store) agentmemory.Store {
			again, err := Open(s.(*Store).Dir())
			if err != nil {
				t.Fatal(err)
			}
			return again
		},
	})
}

func TestStoreSmallBound(t *testing.T) {
	storetest.Run(t, storetest.Options{
		New: func(t *testing.T) agentmemory.Store {
			s, err := Open(t.TempDir(), WithMaxEntryBytes(256))
			if err != nil {
				t.Fatal(err)
			}
			return s
		},
	})
}

var fixed = time.Date(2026, 9, 20, 10, 0, 0, 123456789, time.UTC)

func open(t *testing.T, opts ...Option) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "memory"), append([]Option{WithClock(func() time.Time { return fixed })}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestLayout checks the files a person sees: the entry with its
// frontmatter, the index, the journal line and the lock's absence.
func TestLayout(t *testing.T) {
	ctx := agentmemory.WithSession(context.Background(), "sess-1")
	s := open(t)
	e := agentmemory.Entry{Scope: "user", Name: "style", Content: "Short answers.\n", Meta: map[string]string{"type": "feedback", "description": "How to answer"}}
	if _, err := s.Put(ctx, e); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, agentmemory.Entry{Scope: "user", Name: "bare", Content: "no newline"}); err != nil {
		t.Fatal(err)
	}
	wantEntry := "---\nname: style\nupdated: 2026-09-20T10:00:00.123456789Z\ndescription: How to answer\ntype: feedback\n---\nShort answers.\n"
	if got := readFile(t, filepath.Join(s.Dir(), "user", "style.md")); got != wantEntry {
		t.Errorf("style.md =\n%s\nwant\n%s", got, wantEntry)
	}
	if got := readFile(t, filepath.Join(s.Dir(), "user", "bare.md")); !strings.HasSuffix(got, "\n---\nno newline") {
		t.Errorf("bare.md = %q", got)
	}
	wantIndex := "# user\n\n- [bare](bare.md)\n- [style](style.md) — How to answer\n"
	if got := readFile(t, filepath.Join(s.Dir(), "user", "INDEX.md")); got != wantIndex {
		t.Errorf("INDEX.md =\n%s\nwant\n%s", got, wantIndex)
	}
	journal := readFile(t, filepath.Join(s.Dir(), "journal.jsonl"))
	lines := strings.Split(strings.TrimSuffix(journal, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("journal has %d lines: %q", len(lines), journal)
	}
	var c agentmemory.Change
	if err := json.Unmarshal([]byte(lines[0]), &c); err != nil {
		t.Fatal(err)
	}
	if c.Seq != 1 || c.Session != "sess-1" || c.Prev != "" || c.Entry.Name != "style" || c.Entry.Hash != agentmemory.Hash(e.Content) || !c.At.Equal(fixed) {
		t.Errorf("journal record = %+v", c)
	}
	if _, err := os.Stat(s.lockPath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("lock left behind: %v", err)
	}
	// The cursor beside the journal: where the store has read to, and
	// the hashes it knows, so a person's edit is noticed without
	// reading the journal again.
	var state journalState
	if err := json.Unmarshal([]byte(readFile(t, s.statePath())), &state); err != nil {
		t.Fatal(err)
	}
	if state.Seq != 2 || state.Off != int64(len(journal)) || len(state.Entries) != 2 {
		t.Errorf("state = %+v, journal is %d bytes", state, len(journal))
	}
	if got := state.Entries["user/style"]; got.Hash != agentmemory.Hash(e.Content) || got.Meta == "" {
		t.Errorf("state entry = %+v", got)
	}
	if _, err := s.Forget(ctx, "user", "style"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir(), "user", "style.md")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("forgotten file remains: %v", err)
	}
	if got := readFile(t, filepath.Join(s.Dir(), "user", "INDEX.md")); got != "# user\n\n- [bare](bare.md)\n" {
		t.Errorf("INDEX.md after Forget =\n%s", got)
	}
	if _, err := s.Forget(ctx, "user", "bare"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(s.Dir(), "user", "INDEX.md")); got != "# user\n\nNo entries.\n" {
		t.Errorf("INDEX.md after last Forget =\n%s", got)
	}
	// Temporary files never linger.
	entries, _ := os.ReadDir(filepath.Join(s.Dir(), "user"))
	for _, de := range entries {
		if strings.HasPrefix(de.Name(), ".tmp-") {
			t.Errorf("temporary file left: %s", de.Name())
		}
	}
}

// TestHandWritten reads files a person made: no frontmatter, an
// unknown key, a name that is not kebab-case, a stray directory.
func TestHandWritten(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	dir := filepath.Join(s.Dir(), "user")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"plain.md":    "Just content.\n",
		"keys.md":     "---\nname: wrong\nType: Ignored\ndescription:   padded  \nweird line\n---\nBody\n",
		"Notes.md":    "not an entry",
		"rule.md":     "---\nnot frontmatter, a rule\n",
		"README.txt":  "not an entry either",
		"INDEX.md":    "# user\n",
		"empty-fm.md": "---\n---\nafter an empty block",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	es, err := s.List(ctx, "user")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]agentmemory.Entry{}
	for _, e := range es {
		got[e.Name] = e
	}
	if len(got) != 4 {
		t.Errorf("List = %d entries %v, want plain, keys, rule, empty-fm", len(got), keysOf(got))
	}
	if e := got["plain"]; e.Content != "Just content.\n" || e.Meta != nil || e.Updated.IsZero() {
		t.Errorf("plain = %+v", e)
	}
	if e := got["keys"]; e.Content != "Body\n" || len(e.Meta) != 1 || e.Meta["description"] != "padded" {
		t.Errorf("keys = %+v", e)
	}
	if e := got["rule"]; e.Content != files["rule.md"] {
		t.Errorf("rule = %+v", e)
	}
	if e := got["empty-fm"]; e.Content != "after an empty block" {
		t.Errorf("empty-fm = %+v", e)
	}
	if _, err := s.Get(ctx, "user", "Notes"); !errors.Is(err, agentmemory.ErrInvalid) {
		t.Errorf("Get(Notes) = %v, want ErrInvalid", err)
	}
	if _, err := s.Get(ctx, "../user", "plain"); !errors.Is(err, agentmemory.ErrInvalid) {
		t.Errorf("Get(../user) = %v, want ErrInvalid", err)
	}
}

func keysOf(m map[string]agentmemory.Entry) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestReconcile(t *testing.T) {
	ctx := agentmemory.WithSession(context.Background(), "sess-1")
	s := open(t)
	for _, e := range []agentmemory.Entry{
		{Scope: "user", Name: "edited", Content: "before", Meta: map[string]string{"description": "d"}},
		{Scope: "user", Name: "removed", Content: "gone"},
		{Scope: "user", Name: "same", Content: "same"},
		{Scope: "project", Name: "forgotten", Content: "x"},
	} {
		if _, err := s.Put(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Forget(ctx, "project", "forgotten"); err != nil {
		t.Fatal(err)
	}
	// Nothing changed outside: nothing to reconcile.
	if changes, err := s.Reconcile(ctx); err != nil || len(changes) != 0 {
		t.Fatalf("Reconcile on a clean store = %v, %v", changes, err)
	}
	// A person edits, adds, removes and revives.
	write := func(scope, name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(s.Dir(), scope, name+".md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("user", "edited", "---\nname: edited\ndescription: d\n---\nafter")
	write("user", "added", "By hand.\n")
	write("project", "forgotten", "revived")
	write("project", "too-big", strings.Repeat("x", agentmemory.DefaultMaxEntryBytes+1))
	if err := os.Remove(filepath.Join(s.Dir(), "user", "removed.md")); err != nil {
		t.Fatal(err)
	}
	changes, err := s.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range changes {
		if c.Source != agentmemory.SourceReconciled {
			t.Errorf("%s/%s: Source = %q, want %q", c.Entry.Scope, c.Entry.Name, c.Source, agentmemory.SourceReconciled)
		}
		got = append(got, fmt.Sprintf("%d %s/%s deleted=%v prev=%v session=%q", c.Seq, c.Entry.Scope, c.Entry.Name, c.Entry.Deleted, c.Prev != "", c.Session))
	}
	want := []string{
		"6 project/forgotten deleted=false prev=false session=\"\"",
		"7 user/added deleted=false prev=false session=\"\"",
		"8 user/edited deleted=false prev=true session=\"\"",
		"9 user/removed deleted=true prev=true session=\"\"",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("Reconcile =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if changes[2].Prev != agentmemory.Hash("before") || changes[2].Entry.Hash != agentmemory.Hash("after") {
		t.Errorf("edited: prev %s hash %s", changes[2].Prev, changes[2].Entry.Hash)
	}
	if changes[3].Entry.Content != "gone" {
		t.Errorf("removed tombstone content = %q", changes[3].Entry.Content)
	}
	// The journal now agrees with the files, so a second run is a no-op,
	// and the indexes list what is there.
	if again, err := s.Reconcile(ctx); err != nil || len(again) != 0 {
		t.Errorf("second Reconcile = %v, %v", again, err)
	}
	if got := readFile(t, filepath.Join(s.Dir(), "user", "INDEX.md")); got != "# user\n\n- [added](added.md)\n- [edited](edited.md) — d\n- [same](same.md)\n" {
		t.Errorf("INDEX.md after Reconcile =\n%s", got)
	}
	// The store's own writes continue the sequence.
	if _, err := s.Put(ctx, agentmemory.Entry{Scope: "user", Name: "next", Content: "n"}); err != nil {
		t.Fatal(err)
	}
	var last agentmemory.Change
	for c, err := range s.Journal(ctx, 0) {
		if err != nil {
			t.Fatal(err)
		}
		last = c
	}
	if last.Seq != 10 || last.Entry.Name != "next" {
		t.Errorf("last record = %+v", last)
	}
}

// TestReconcileClosesTheGap is the case Reconcile alone could not
// reach: the agent writes over a person's edit before anything has
// reconciled. The person's version is recorded first, by nobody, so
// the chain is whole and nothing they wrote is lost.
func TestReconcileClosesTheGap(t *testing.T) {
	ctx := agentmemory.WithSession(context.Background(), "sess-agent")
	s := open(t)
	if _, err := s.Put(ctx, agentmemory.Entry{Scope: "user", Name: "profile", Content: "Chris. Timezone Europe/London.\n"}); err != nil {
		t.Fatal(err)
	}
	// A person edits the file by hand.
	byHand := "---\nname: profile\n---\nChris. Prefers Go. Timezone Europe/London.\n"
	if err := os.WriteFile(filepath.Join(s.Dir(), "user", "profile.md"), []byte(byHand), 0o644); err != nil {
		t.Fatal(err)
	}
	// The agent writes over it without reconciling first.
	rec, err := s.Put(ctx, agentmemory.Entry{Scope: "user", Name: "profile", Content: "Chris. Lives in Bristol.\n"})
	if err != nil {
		t.Fatal(err)
	}
	changes := journalOf(t, s)
	if len(changes) != 3 {
		t.Fatalf("journal = %d records, want the write, the person's version and the write over it", len(changes))
	}
	person := changes[1]
	if person.Source != agentmemory.SourceReconciled || person.Session != "" ||
		person.Entry.Content != "Chris. Prefers Go. Timezone Europe/London.\n" ||
		person.Prev != changes[0].Entry.Hash || person.Replaced != changes[0].Entry.Hash {
		t.Errorf("the person's version = %+v", person)
	}
	if rec.Seq != 3 || rec.Prev != person.Entry.Hash || rec.Replaced != person.Entry.Hash || rec.Session != "sess-agent" {
		t.Errorf("the agent's write = %+v", rec)
	}
	// The chain is whole, so an auditor finds no gap.
	if lost, err := agentmemory.LostUpdates(ctx, s, 0); err != nil || len(lost) != 0 {
		t.Errorf("LostUpdates = %+v, %v", lost, err)
	}
	// And a Reconcile afterwards has nothing to add.
	if again, err := s.Reconcile(ctx); err != nil || len(again) != 0 {
		t.Errorf("Reconcile after the write = %v, %v", again, err)
	}
	// The same holds for a file a person removed and for one they
	// added, when the agent writes to the name next.
	if err := os.Remove(filepath.Join(s.Dir(), "user", "profile.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, agentmemory.Entry{Scope: "user", Name: "profile", Content: "Chris.\n"}); err != nil {
		t.Fatal(err)
	}
	changes = journalOf(t, s)
	tomb := changes[3]
	if !tomb.Entry.Deleted || tomb.Source != agentmemory.SourceReconciled || tomb.Session != "" ||
		tomb.Entry.Content != "Chris. Lives in Bristol.\n" {
		t.Errorf("the tombstone for the file the person removed = %+v", tomb)
	}
	if last := changes[4]; last.Prev != "" || last.Replaced != "" || last.Session != "sess-agent" {
		t.Errorf("the write after it = %+v", last)
	}
	if lost, err := agentmemory.LostUpdates(ctx, s, 0); err != nil || len(lost) != 0 {
		t.Errorf("LostUpdates after the removal = %+v, %v", lost, err)
	}
}

// TestReconcileReadsFromItsCursor checks that Reconcile reads the
// journal from where it left off and not from the beginning: a record
// it has already seen is damaged in place, and a run that re-read the
// journal would lose what that record said and report the entry as one
// a person had added.
func TestReconcileReadsFromItsCursor(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	for _, name := range []string{"one", "two", "three"} {
		if _, err := s.Put(ctx, agentmemory.Entry{Scope: "user", Name: name, Content: name + "\n"}); err != nil {
			t.Fatal(err)
		}
	}
	if changes, err := s.Reconcile(ctx); err != nil || len(changes) != 0 {
		t.Fatalf("Reconcile on a clean store = %v, %v", changes, err)
	}
	if _, err := os.Stat(s.statePath()); err != nil {
		t.Fatalf("no cursor beside the journal: %v", err)
	}
	// Damage the second record in place, keeping the file's length so
	// the offsets after it still hold and the first line, which the
	// cursor is identified by, is untouched.
	data := []byte(readFile(t, s.journalPath()))
	first := bytes.IndexByte(data, '\n') + 1
	second := first + bytes.IndexByte(data[first:], '\n')
	copy(data[first:second], bytes.Repeat([]byte("x"), second-first))
	if err := os.WriteFile(s.journalPath(), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if changes, err := s.Reconcile(ctx); err != nil || len(changes) != 0 {
		t.Errorf("Reconcile read the journal again: %v, %v", changes, err)
	}
	// A run that finds nothing to do and has nothing to catch up on
	// writes no cursor either, so the call a product makes before every
	// render costs a read and no write.
	before, err := os.Stat(s.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(s.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("a Reconcile with nothing to do rewrote the cursor")
	}
	// Without the cursor it does read the journal whole, and then the
	// damaged record is a record it never saw.
	if err := os.Remove(s.statePath()); err != nil {
		t.Fatal(err)
	}
	changes, err := s.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Entry.Name != "two" || changes[0].Source != agentmemory.SourceReconciled {
		t.Errorf("Reconcile without a cursor = %+v", changes)
	}
	// And a cursor it had to rebuild is written back, whatever it
	// found, or every later run rebuilds it again.
	if _, err := os.Stat(s.statePath()); err != nil {
		t.Errorf("the rebuilt cursor was not written: %v", err)
	}
	if err := os.Remove(s.statePath()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.statePath()); err != nil {
		t.Errorf("a rebuild that found nothing wrote no cursor: %v", err)
	}
}

// TestFailedWriteJournalsNothing holds Put and Forget to what the
// contract says: a write that fails writes nothing. The record of a
// person's edit is part of the write, so it waits for the entry file
// to land; a scope directory this process cannot write is the failure
// that used to leave the journal a record longer.
func TestFailedWriteJournalsNothing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes a read-only directory")
	}
	ctx := agentmemory.WithSession(context.Background(), "sess-agent")
	s := open(t)
	if _, err := s.Put(ctx, agentmemory.Entry{Scope: "user", Name: "profile", Content: "Chris.\n"}); err != nil {
		t.Fatal(err)
	}
	// A person edits the file, so the next write has that to record.
	if err := os.WriteFile(filepath.Join(s.Dir(), "user", "profile.md"), []byte("---\nname: profile\n---\nChris. Prefers Go.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	scope := filepath.Join(s.Dir(), "user")
	if err := os.Chmod(scope, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(scope, 0o755) })
	before := len(journalOf(t, s))
	if _, err := s.Put(ctx, agentmemory.Entry{Scope: "user", Name: "profile", Content: "Chris. Lives in Bristol.\n"}); err == nil {
		t.Fatal("Put into a directory this process cannot write succeeded")
	}
	if after := len(journalOf(t, s)); after != before {
		t.Errorf("a failed Put grew the journal from %d records to %d", before, after)
	}
	if _, err := s.Forget(ctx, "user", "profile"); err == nil {
		t.Fatal("Forget from a directory this process cannot write succeeded")
	}
	if after := len(journalOf(t, s)); after != before {
		t.Errorf("a failed Forget grew the journal from %d records to %d", before, after)
	}
	// With the directory writable again, the person's edit is still
	// there to be recorded.
	if err := os.Chmod(scope, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, agentmemory.Entry{Scope: "user", Name: "profile", Content: "Chris. Lives in Bristol.\n"}); err != nil {
		t.Fatal(err)
	}
	changes := journalOf(t, s)
	if len(changes) != before+2 {
		t.Fatalf("journal = %d records, want the person's version and the write", len(changes))
	}
	if person := changes[before]; person.Source != agentmemory.SourceReconciled || person.Entry.Content != "Chris. Prefers Go.\n" {
		t.Errorf("the person's version = %+v", person)
	}
	if lost, err := agentmemory.LostUpdates(ctx, s, 0); err != nil || len(lost) != 0 {
		t.Errorf("LostUpdates = %+v, %v", lost, err)
	}
}

// TestCursorNoticesANewJournal replaces the journal with another one
// that is no shorter, as a backup restored over it or a merge would.
// The cursor's offset then points into the middle of a record that is
// not the one it was written for, and the hashes it holds are about
// records this journal does not have, so it has to be rebuilt: the
// cursor names the journal's first line for exactly this.
func TestCursorNoticesANewJournal(t *testing.T) {
	ctx := context.Background()
	mine := open(t)
	for _, name := range []string{"a-one", "a-two"} {
		if _, err := mine.Put(ctx, agentmemory.Entry{Scope: "user", Name: name, Content: name + "\n"}); err != nil {
			t.Fatal(err)
		}
	}
	if changes, err := mine.Reconcile(ctx); err != nil || len(changes) != 0 {
		t.Fatalf("Reconcile on a clean store = %v, %v", changes, err)
	}
	// Another store's journal, longer than this one's and about other
	// entries.
	other := open(t)
	for _, name := range []string{"b-one", "b-two", "b-three", "b-four"} {
		if _, err := other.Put(ctx, agentmemory.Entry{Scope: "user", Name: name, Content: strings.Repeat(name+" ", 8)}); err != nil {
			t.Fatal(err)
		}
	}
	replacement := []byte(readFile(t, other.journalPath()))
	if mineLen := len(readFile(t, mine.journalPath())); len(replacement) < mineLen {
		t.Fatalf("the replacement journal is %d bytes, shorter than %d", len(replacement), mineLen)
	}
	if err := os.WriteFile(mine.journalPath(), replacement, 0o644); err != nil {
		t.Fatal(err)
	}
	changes, err := mine.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Four names the new journal knows and no file holds, and two files
	// it does not know.
	var tombstones, created int
	for _, c := range changes {
		if c.Source != agentmemory.SourceReconciled {
			t.Errorf("%s/%s: Source = %q", c.Entry.Scope, c.Entry.Name, c.Source)
		}
		if c.Entry.Deleted {
			tombstones++
		} else {
			created++
		}
	}
	if tombstones != 4 || created != 2 {
		t.Errorf("Reconcile after the journal was replaced = %d tombstones, %d creates; want 4 and 2", tombstones, created)
	}
	// The sequence continues the journal that is there, not the one the
	// cursor remembered.
	last := journalOf(t, mine)
	for i, c := range last {
		if c.Seq != uint64(i+1) {
			t.Fatalf("record %d has Seq %d", i, c.Seq)
		}
	}
	if len(last) != 4+6 {
		t.Errorf("journal = %d records, want the four it was replaced with and six more", len(last))
	}
}

func journalOf(t *testing.T, s *Store) []agentmemory.Change {
	t.Helper()
	var out []agentmemory.Change
	for c, err := range s.Journal(context.Background(), 0) {
		if err != nil {
			t.Fatalf("journal: %v", err)
		}
		out = append(out, c)
	}
	return out
}

// TestJournalDamage checks that a damaged line is reported and skipped
// and that a cut-off last line is ignored, by the reader and by the
// writer's sequence.
func TestJournalDamage(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	for i := 0; i < 3; i++ {
		if _, err := s.Put(ctx, agentmemory.Entry{Scope: "user", Name: fmt.Sprintf("n%d", i), Content: "c"}); err != nil {
			t.Fatal(err)
		}
	}
	data := readFile(t, s.journalPath())
	lines := strings.SplitAfter(data, "\n")
	lines[1] = "{not json\n"
	damaged := strings.Join(lines[:3], "") + `{"seq":4,"partial`
	if err := os.WriteFile(s.journalPath(), []byte(damaged), 0o644); err != nil {
		t.Fatal(err)
	}
	var seqs []uint64
	var errs []error
	for c, err := range s.Journal(ctx, 0) {
		if err != nil {
			errs = append(errs, err)
			continue
		}
		seqs = append(seqs, c.Seq)
	}
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "journal line 2") {
		t.Errorf("errors = %v", errs)
	}
	if fmt.Sprint(seqs) != "[1 3]" {
		t.Errorf("seqs = %v, want [1 3]", seqs)
	}
	// The next write follows the last complete record.
	if _, err := s.Put(ctx, agentmemory.Entry{Scope: "user", Name: "after", Content: "c"}); err != nil {
		t.Fatal(err)
	}
	seq, err := s.lastSeq()
	if err != nil || seq != 4 {
		t.Errorf("lastSeq = %d, %v; want 4", seq, err)
	}
	// Reading stops on request.
	n := 0
	for range s.Journal(ctx, 0) {
		n++
		break
	}
	if n != 1 {
		t.Errorf("break did not stop the read")
	}
}

// TestLastLine covers the backwards read over chunk boundaries.
func TestLastLine(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{"empty", "", ""},
		{"one line", "a\n", "a"},
		{"two lines", "a\nb\n", "b"},
		{"partial tail", "a\nb\nc", "b"},
		{"no newline", "abc", ""},
		{"long last", "a\n" + strings.Repeat("x", 20000) + "\n", strings.Repeat("x", 20000)},
		{"long first", strings.Repeat("x", 20000) + "\nb\n", "b"},
		{"long both", strings.Repeat("x", 9000) + "\n" + strings.Repeat("y", 9000) + "\n", strings.Repeat("y", 9000)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "j")
			if err := os.WriteFile(path, []byte(tt.data), 0o644); err != nil {
				t.Fatal(err)
			}
			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			got, err := lastLine(f)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Errorf("lastLine = %q (%d bytes), want %d bytes", truncate(string(got)), len(got), len(tt.want))
			}
		})
	}
}

func truncate(s string) string {
	if len(s) > 20 {
		return s[:20] + "…"
	}
	return s
}

func TestLock(t *testing.T) {
	ctx := context.Background()
	writeLock := func(t *testing.T, s *Store, info LockInfo) {
		t.Helper()
		data, _ := json.Marshal(info)
		if err := os.WriteFile(s.lockPath(), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	host, _ := os.Hostname()
	e := agentmemory.Entry{Scope: "user", Name: "a", Content: "x"}

	t.Run("dead holder is taken over", func(t *testing.T) {
		s := open(t, WithLockTimeout(50*time.Millisecond))
		// A PID this host will not have: the max on Linux is 4194304.
		writeLock(t, s, LockInfo{PID: 4194303 + 1<<20, Host: host, Since: time.Now()})
		if _, err := s.Put(ctx, e); err != nil {
			t.Fatalf("Put over a dead holder's lock: %v", err)
		}
		if h, _ := s.LockHolder(); h != nil {
			t.Errorf("lock left after the write: %+v", h)
		}
	})
	t.Run("live holder is named", func(t *testing.T) {
		s := open(t, WithLockTimeout(50*time.Millisecond))
		since := time.Now().UTC().Round(time.Second)
		writeLock(t, s, LockInfo{PID: os.Getpid(), Host: host, Since: since})
		start := time.Now()
		_, err := s.Put(ctx, e)
		if !errors.Is(err, ErrLocked) {
			t.Fatalf("Put under a live lock = %v, want ErrLocked", err)
		}
		if want := fmt.Sprintf("held by pid %d on %s since %s", os.Getpid(), host, since.Format(time.RFC3339)); !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name the holder %q", err, want)
		}
		if elapsed := time.Since(start); elapsed < 50*time.Millisecond || elapsed > 2*time.Second {
			t.Errorf("waited %v, want about the timeout", elapsed)
		}
		h, err := s.LockHolder()
		if err != nil || h == nil || h.PID != os.Getpid() {
			t.Errorf("LockHolder = %+v, %v", h, err)
		}
		if err := s.BreakLock(); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Put(ctx, e); err != nil {
			t.Errorf("Put after BreakLock: %v", err)
		}
	})
	t.Run("another host is never stale", func(t *testing.T) {
		s := open(t, WithLockTimeout(20*time.Millisecond))
		writeLock(t, s, LockInfo{PID: 1, Host: "elsewhere", Since: time.Now()})
		if _, err := s.Put(ctx, e); !errors.Is(err, ErrLocked) {
			t.Errorf("Put under another host's lock = %v, want ErrLocked", err)
		}
	})
	t.Run("unreadable lock is waited for", func(t *testing.T) {
		s := open(t, WithLockTimeout(20*time.Millisecond))
		if err := os.WriteFile(s.lockPath(), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := s.Put(ctx, e)
		if !errors.Is(err, ErrLocked) || !strings.Contains(err.Error(), "unreadable lock") {
			t.Errorf("Put under an empty lock = %v", err)
		}
	})
	t.Run("context bounds the wait", func(t *testing.T) {
		s := open(t, WithLockTimeout(time.Minute))
		writeLock(t, s, LockInfo{PID: os.Getpid(), Host: host, Since: time.Now()})
		ctx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
		defer cancel()
		if _, err := s.Put(ctx, e); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Put with a cancelled context = %v, want DeadlineExceeded", err)
		}
	})
	t.Run("free lock reports nil", func(t *testing.T) {
		s := open(t)
		if h, err := s.LockHolder(); h != nil || err != nil {
			t.Errorf("LockHolder = %+v, %v", h, err)
		}
		if err := s.BreakLock(); err != nil {
			t.Errorf("BreakLock on a free store: %v", err)
		}
	})
}

// TestTwoProcesses runs the shared-entry race from storetest across a
// process boundary: this process and a child, re-executed from the
// test binary, each write their own entries and append their facts to
// one shared entry through IfHash. The journal must be one contiguous
// chain and the shared entry must hold every fact from both.
func TestTwoProcesses(t *testing.T) {
	if os.Getenv("FILESTORE_CHILD") != "" {
		t.Skip("child process")
	}
	dir := filepath.Join(t.TempDir(), "memory")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := s.Put(ctx, agentmemory.Entry{Scope: "user", Name: "shared", Content: "facts:"}); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestChildProcess$", "-test.v")
	cmd.Env = append(os.Environ(), "FILESTORE_CHILD=1", "FILESTORE_DIR="+dir)
	out, errCh := make([]byte, 0), make(chan error, 1)
	go func() {
		var err error
		out, err = cmd.CombinedOutput()
		errCh <- err
	}()
	if err := childWork(dir, "parent"); err != nil {
		t.Errorf("parent: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "PASS") {
		t.Fatalf("child did not pass:\n%s", out)
	}
	shared, err := s.Get(ctx, "user", "shared")
	if err != nil {
		t.Fatal(err)
	}
	for _, who := range []string{"parent", "child"} {
		for i := 0; i < childWrites; i++ {
			if fact := fmt.Sprintf(" %s-%d", who, i); !strings.Contains(shared.Content, fact) {
				t.Errorf("shared entry lost %q", fact)
			}
			if _, err := s.Get(ctx, "user", fmt.Sprintf("%s-%d", who, i)); err != nil {
				t.Errorf("own entry: %v", err)
			}
		}
	}
	var n int
	last := map[string]string{}
	for c, err := range s.Journal(ctx, 0) {
		if err != nil {
			t.Fatal(err)
		}
		n++
		if c.Seq != uint64(n) {
			t.Errorf("record %d has Seq %d", n, c.Seq)
		}
		k := string(c.Entry.Scope) + "/" + c.Entry.Name
		if c.Prev != last[k] {
			t.Errorf("record %d (%s): Prev %q, want %q", n, k, c.Prev, last[k])
		}
		last[k] = c.Entry.Hash
	}
	if want := 1 + 2*childWrites*2; n != want {
		t.Errorf("journal has %d records, want %d", n, want)
	}
	if h, _ := s.LockHolder(); h != nil {
		t.Errorf("lock left behind: %+v", h)
	}
}

const childWrites = 12

// TestChildProcess is the other process of TestTwoProcesses. It runs
// only when re-executed with FILESTORE_CHILD set.
func TestChildProcess(t *testing.T) {
	dir := os.Getenv("FILESTORE_DIR")
	if os.Getenv("FILESTORE_CHILD") == "" || dir == "" {
		t.Skip("not a child process")
	}
	if err := childWork(dir, "child"); err != nil {
		t.Fatal(err)
	}
}

func childWork(dir, who string) error {
	s, err := Open(dir)
	if err != nil {
		return err
	}
	ctx := agentmemory.WithSession(context.Background(), who)
	for i := 0; i < childWrites; i++ {
		if _, err := s.Put(ctx, agentmemory.Entry{Scope: "user", Name: fmt.Sprintf("%s-%d", who, i), Content: who}); err != nil {
			return err
		}
		fact := fmt.Sprintf(" %s-%d", who, i)
		for attempt := 0; ; attempt++ {
			cur, err := s.Get(ctx, "user", "shared")
			if err != nil {
				return err
			}
			next := *cur
			next.Content += fact
			_, err = s.Put(ctx, next, agentmemory.IfHash(cur.Hash))
			if err == nil {
				break
			}
			if !errors.Is(err, agentmemory.ErrConflict) || attempt == 500 {
				return err
			}
		}
	}
	return nil
}

func TestFrontmatterRoundTrip(t *testing.T) {
	tests := []agentmemory.Entry{
		{Name: "a", Content: "plain\n"},
		{Name: "b", Content: "no newline"},
		{Name: "c", Content: "---\nlooks like frontmatter\n---\nbody", Meta: map[string]string{"description": "with: colon", "z-key": "", "a-key": "first"}},
		{Name: "d", Content: "\n\nleading blank lines"},
		{Name: "e", Content: "süß ✓ 日本\n"},
	}
	for _, e := range tests {
		e.Updated = fixed
		d := decode(encode(e))
		if d.content != e.Content {
			t.Errorf("%s: content %q, want %q", e.Name, d.content, e.Content)
		}
		if !d.updated.Equal(fixed) {
			t.Errorf("%s: updated %v", e.Name, d.updated)
		}
		if len(d.meta) != len(e.Meta) {
			t.Errorf("%s: meta %v, want %v", e.Name, d.meta, e.Meta)
		}
		for k, v := range e.Meta {
			if d.meta[k] != v {
				t.Errorf("%s: meta[%s] = %q, want %q", e.Name, k, d.meta[k], v)
			}
		}
	}
}
