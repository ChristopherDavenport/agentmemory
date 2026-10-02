package filestore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentmemory"
)

// The format document, docs/filestore-format.md, is held to the code by
// the tests in this file: they read the fenced blocks it marks with
// file=<tree>/<path> as the files of example trees, and the block it
// marks lock as the lock file, and check each against what the package
// reads and writes. A change to the format starts in the document's
// examples and fails here until the code agrees.

const formatDoc = "../docs/filestore-format.md"

// exampleTrees are the document's example files by tree, then by path
// within the tree, and its tagged blocks by tag.
type exampleTrees struct {
	trees  map[string]map[string][]byte
	tagged map[string][]byte
	text   string
}

func readFormatDoc(t *testing.T) exampleTrees {
	t.Helper()
	data, err := os.ReadFile(formatDoc)
	if err != nil {
		t.Fatal(err)
	}
	ex := exampleTrees{trees: map[string]map[string][]byte{}, tagged: map[string][]byte{}, text: string(data)}
	lines := strings.Split(string(data), "\n")
	for i := 0; i < len(lines); i++ {
		info, ok := strings.CutPrefix(lines[i], "```")
		if !ok || info == "" {
			continue
		}
		var body strings.Builder
		for i++; i < len(lines) && lines[i] != "```"; i++ {
			body.WriteString(lines[i])
			body.WriteByte('\n')
		}
		words := strings.Fields(info)
		content := []byte(body.String())
		var file string
		for _, w := range words[1:] {
			switch {
			case strings.HasPrefix(w, "file="):
				file = strings.TrimPrefix(w, "file=")
			case w == "noeol":
				content = bytes.TrimSuffix(content, []byte("\n"))
			default:
				ex.tagged[w] = content
			}
		}
		if file == "" {
			continue
		}
		tree, path, ok := strings.Cut(file, "/")
		if !ok {
			t.Fatalf("block file=%s names no tree", file)
		}
		if ex.trees[tree] == nil {
			ex.trees[tree] = map[string][]byte{}
		}
		if _, dup := ex.trees[tree][path]; dup {
			t.Fatalf("block file=%s appears twice", file)
		}
		ex.trees[tree][path] = content
	}
	for _, tree := range []string{"memory", "torn", "torn-after"} {
		if len(ex.trees[tree]) == 0 {
			t.Fatalf("the document has no %s tree", tree)
		}
	}
	return ex
}

// materialise writes an example tree to a new directory.
func materialise(t *testing.T, tree map[string][]byte) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "memory")
	for path, data := range tree {
		full := filepath.Join(dir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// readTree returns every file under dir by slash path.
func readTree(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		out[filepath.ToSlash(rel)] = data
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func compareTree(t *testing.T, want, got map[string][]byte) {
	t.Helper()
	paths := map[string]bool{}
	for p := range want {
		paths[p] = true
	}
	for p := range got {
		paths[p] = true
	}
	sorted := make([]string, 0, len(paths))
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)
	for _, p := range sorted {
		w, inWant := want[p]
		g, inGot := got[p]
		switch {
		case !inWant:
			t.Errorf("%s: written but not in the document", p)
		case !inGot:
			t.Errorf("%s: in the document but not written", p)
		case !bytes.Equal(w, g):
			t.Errorf("%s differs\n--- document (%d bytes)\n%s\n--- written (%d bytes)\n%s", p, len(w), w, len(g), g)
		}
	}
}

// The worked example's clock: one second per change from t0.
var formatT0 = time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)

func formatClock(from time.Time) func() time.Time {
	n := 0
	return func() time.Time {
		n++
		return from.Add(time.Duration(n-1) * time.Second)
	}
}

// TestFormatReplay runs the worked example against a fresh store and
// compares every file with the document's memory tree, so the document
// shows what the package writes, byte for byte.
func TestFormatReplay(t *testing.T) {
	ex := readFormatDoc(t)
	dir := filepath.Join(t.TempDir(), "memory")
	s, err := Open(dir, WithClock(formatClock(formatT0)))
	if err != nil {
		t.Fatal(err)
	}
	slack := agentmemory.WithSession(context.Background(), "slack-7f3a")
	telegram := agentmemory.WithSession(context.Background(), "telegram-c91e")
	meta := func() map[string]string { return map[string]string{"description": "How to answer", "type": "feedback"} }
	put := func(ctx context.Context, name, content string, meta map[string]string, opts ...agentmemory.PutOption) *agentmemory.Change {
		t.Helper()
		c, err := s.Put(ctx, agentmemory.Entry{Scope: "user", Name: name, Content: content, Meta: meta}, opts...)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	put(slack, "editor", "Uses neovim.\n", nil)
	read := put(slack, "style", "Prefers short answers.\n", meta())
	put(telegram, "style", "Prefers short answers & tabs.\n", meta(), agentmemory.BasedOn(read.Entry.Hash))
	put(slack, "style", "Prefers short answers. Lives in Bristol.\n", meta(), agentmemory.BasedOn(read.Entry.Hash))
	// The person's merge is the entry file the document shows.
	hand := filepath.Join(dir, "user", "style.md")
	if err := os.WriteFile(hand, ex.trees["memory"]["user/style.md"], 0o644); err != nil {
		t.Fatal(err)
	}
	// The person's time is the file's modification time, to the half
	// second: a file system with one-second times would fail here.
	at := formatT0.Add(3500 * time.Millisecond)
	if err := os.Chtimes(hand, at, at); err != nil {
		t.Fatal(err)
	}
	changes, err := s.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Source != agentmemory.SourceReconciled {
		t.Fatalf("Reconcile = %+v, want one reconciled change", changes)
	}
	if _, err := s.Forget(slack, "user", "editor"); err != nil {
		t.Fatal(err)
	}
	compareTree(t, ex.trees["memory"], readTree(t, dir))
}

// TestFormatRead opens the document's memory tree as a store and checks
// that the package reads it as the document says: the entries, the
// journal's records and their chain, the index, the cursor, and the
// encodings, which must come back byte for byte.
func TestFormatRead(t *testing.T) {
	ex := readFormatDoc(t)
	tree := ex.trees["memory"]
	dir := materialise(t, tree)
	s, err := Open(dir, WithClock(formatClock(formatT0.Add(time.Minute))))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// The journal, line by line.
	var records []agentmemory.Change
	last := map[string]string{}
	for i, line := range bytes.Split(bytes.TrimSuffix(tree["journal.jsonl"], []byte("\n")), []byte("\n")) {
		c, err := decodeRecord(line)
		if err != nil {
			t.Fatalf("journal line %d: %v", i+1, err)
		}
		if c.Seq != uint64(i+1) {
			t.Errorf("line %d has seq %d", i+1, c.Seq)
		}
		if again, _ := json.Marshal(c); !bytes.Equal(again, line) {
			t.Errorf("line %d does not encode back to itself:\n%s\n%s", i+1, line, again)
		}
		if h := agentmemory.Hash(c.Entry.Content); h != c.Entry.Hash {
			t.Errorf("line %d: hash %s, content hashes %s", i+1, c.Entry.Hash, h)
		}
		k := key(c.Entry.Scope, c.Entry.Name)
		if c.Replaced != last[k] {
			t.Errorf("line %d: replaced %q, the chain holds %q", i+1, c.Replaced, last[k])
		}
		if c.Entry.Deleted {
			last[k] = ""
		} else {
			last[k] = c.Entry.Hash
		}
		records = append(records, c)
	}
	if len(records) != 6 {
		t.Fatalf("%d records, the document describes 6", len(records))
	}
	// What the document says of each record: prevOf and replacedOf are
	// the seq of the record whose hash prev and replaced name, 0 for
	// none.
	hashOfSeq := func(seq int) string {
		if seq == 0 {
			return ""
		}
		return records[seq-1].Entry.Hash
	}
	for i, want := range []struct {
		prevOf, replacedOf int
		reconciled         bool
		tombstone          bool
		updated            time.Time
	}{
		{0, 0, false, false, formatT0},
		{0, 0, false, false, formatT0.Add(time.Second)},
		{2, 2, false, false, formatT0.Add(2 * time.Second)},
		{2, 3, false, false, formatT0.Add(3 * time.Second)}, // the fork
		{4, 4, true, false, formatT0.Add(3500 * time.Millisecond)},
		{6, 6, false, true, formatT0.Add(5 * time.Second)},
	} {
		c := records[i]
		if c.Prev != hashOfSeq(want.prevOf) || c.Replaced != hashOfSeq(want.replacedOf) {
			t.Errorf("record %d: prev %s replaced %s, want record %d's and %d's", c.Seq, c.Prev, c.Replaced, want.prevOf, want.replacedOf)
		}
		if got := c.Source == agentmemory.SourceReconciled && c.Session == ""; got != want.reconciled {
			t.Errorf("record %d: reconciled = %v", c.Seq, got)
		}
		if c.Entry.Deleted != want.tombstone {
			t.Errorf("record %d: tombstone = %v", c.Seq, c.Entry.Deleted)
		}
		if !c.Entry.Updated.Equal(want.updated) {
			t.Errorf("record %d: updated = %s, want %s", c.Seq, c.Entry.Updated, want.updated)
		}
	}

	// The store reads the same records, and the one lost update.
	var seqs []uint64
	for c, err := range s.Journal(ctx, 0) {
		if err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, c.Seq)
	}
	if fmt.Sprint(seqs) != "[1 2 3 4 5 6]" {
		t.Errorf("Journal = %v", seqs)
	}
	lost, err := agentmemory.LostUpdates(ctx, s, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(lost) != 1 || lost[0].Change.Seq != 4 || lost[0].Over != records[2].Entry.Hash {
		t.Errorf("LostUpdates = %+v, want record 4 over record 3's hash", lost)
	}

	// The live entries.
	style, err := s.Get(ctx, "user", "style")
	if err != nil {
		t.Fatal(err)
	}
	if style.Hash != records[4].Entry.Hash || style.Content != records[4].Entry.Content {
		t.Errorf("Get(style) = %q %s, want record 5's state", style.Content, style.Hash)
	}
	if fmt.Sprint(style.Meta) != fmt.Sprint(records[4].Entry.Meta) {
		t.Errorf("Get(style).Meta = %v", style.Meta)
	}
	if _, err := s.Get(ctx, "user", "editor"); !errors.Is(err, agentmemory.ErrNotFound) {
		t.Errorf("Get(editor) = %v, want ErrNotFound", err)
	}
	if es, err := s.List(ctx, "user"); err != nil || len(es) != 1 || es[0].Name != "style" {
		t.Errorf("List(user) = %v, %v", es, err)
	}

	// The entry file: decoded as the document says and encoded back.
	file := tree["user/style.md"]
	d := decode(file)
	if agentmemory.Hash(d.content) != records[4].Entry.Hash {
		t.Errorf("the entry file's content hashes %s", agentmemory.Hash(d.content))
	}
	if d.updated.IsZero() || fmt.Sprint(d.meta) != fmt.Sprint(records[4].Entry.Meta) {
		t.Errorf("decode = %+v", d)
	}
	if again := encode(agentmemory.Entry{Scope: "user", Name: "style", Content: d.content, Meta: d.meta, Updated: d.updated}); !bytes.Equal(again, file) {
		t.Errorf("the entry file does not encode back to itself:\n%s\n%s", file, again)
	}

	// The index is what the package writes.
	if err := os.Remove(filepath.Join(dir, "user", indexName)); err != nil {
		t.Fatal(err)
	}
	if err := s.reindex("user"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dir, "user", indexName)); got != string(tree["user/INDEX.md"]) {
		t.Errorf("INDEX.md = %q, the document shows %q", got, tree["user/INDEX.md"])
	}

	// The cursor: read as it is, and rebuilt from the journal.
	for _, rebuild := range []bool{false, true} {
		if rebuild {
			if err := os.Remove(s.statePath()); err != nil {
				t.Fatal(err)
			}
		}
		st, err := s.loadState()
		if err != nil {
			t.Fatal(err)
		}
		if st.read == rebuild {
			t.Errorf("rebuild=%v: cursor read from the file = %v", rebuild, st.read)
		}
		data, _ := json.Marshal(st)
		if got := string(data) + "\n"; got != string(tree[".state.json"]) {
			t.Errorf("rebuild=%v: cursor = %s, the document shows %s", rebuild, got, tree[".state.json"])
		}
	}
	if st, _ := s.loadState(); st.Off != int64(len(tree["journal.jsonl"])) {
		t.Errorf("cursor offset %d, journal is %d bytes", st.Off, len(tree["journal.jsonl"]))
	}
}

// TestFormatTornTail makes the write the document describes on its torn
// journal and compares the result with the torn-after tree.
func TestFormatTornTail(t *testing.T) {
	ex := readFormatDoc(t)
	dir := materialise(t, ex.trees["torn"])
	if bytes.HasSuffix(ex.trees["torn"]["journal.jsonl"], []byte("\n")) {
		t.Fatal("the torn journal ends in a newline; it should end in a partial line")
	}
	s, err := Open(dir, WithClock(formatClock(formatT0.Add(11*time.Second))))
	if err != nil {
		t.Fatal(err)
	}
	ctx := agentmemory.WithSession(context.Background(), "slack-7f3a")
	c, err := s.Put(ctx, agentmemory.Entry{Scope: "user", Name: "notes", Content: "Keep answers short.\n"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Seq != 2 {
		t.Errorf("Seq = %d, want 2: the partial record's number was never allocated", c.Seq)
	}
	got := readTree(t, dir)
	delete(got, ".state.json") // derived, and not part of the torn example
	if _, ok := got["user/notes.md"]; !ok {
		t.Error("the entry file was not written")
	}
	delete(got, "user/notes.md")
	delete(got, "user/INDEX.md")
	compareTree(t, ex.trees["torn-after"], got)
	var seqs []uint64
	var errs []string
	for c, err := range s.Journal(context.Background(), 0) {
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		seqs = append(seqs, c.Seq)
	}
	if fmt.Sprint(seqs) != "[1 2]" || len(errs) != 1 || !strings.Contains(errs[0], "line 2") {
		t.Errorf("Journal = %v, %v; want records 1 and 2 and an error for line 2", seqs, errs)
	}
}

// TestFormatLock decodes the document's lock, encodes it back, and
// checks the claim name the document derives from it.
func TestFormatLock(t *testing.T) {
	ex := readFormatDoc(t)
	data, ok := ex.tagged["lock"]
	if !ok {
		t.Fatal("the document has no block marked lock")
	}
	var info LockInfo
	if err := json.Unmarshal(data, &info); err != nil {
		t.Fatal(err)
	}
	var again bytes.Buffer
	if err := json.NewEncoder(&again).Encode(info); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again.Bytes(), data) {
		t.Errorf("the lock does not encode back to itself:\n%s%s", data, again.Bytes())
	}
	if info.PID <= 0 || info.Host == "" || info.Since.IsZero() {
		t.Errorf("lock = %+v", info)
	}
	claim := lockName + claimSuffix + info.fingerprint()
	if !strings.Contains(ex.text, "`"+claim+"`") {
		t.Errorf("the document does not name the claim %s", claim)
	}
	if !info.same(info) || info.stale() {
		t.Error("a lock from another host reads as stale")
	}
}

// TestFormatNames checks that the file names and bounds the document
// states are the package's.
func TestFormatNames(t *testing.T) {
	ex := readFormatDoc(t)
	for _, want := range []string{
		"`" + journalName + "`",
		"`" + stateName + "`",
		"`" + lockName + "`",
		"`" + lockName + claimSuffix,
		"`" + indexName + "`",
		"`.tmp-*`",
		fmt.Sprintf("| %d bytes |", agentmemory.MaxNameBytes),
		fmt.Sprintf("| %d bytes |", agentmemory.MaxMetaBytes),
		fmt.Sprintf("%d bytes by default", agentmemory.DefaultMaxEntryBytes),
		fmt.Sprintf("first %d bytes", headLimit),
		fmt.Sprintf("`DefaultLockTimeout`, %d s", int(DefaultLockTimeout/time.Second)),
		fmt.Sprintf("from %d ms and doubling to %d ms", int(minLockWait/time.Millisecond), int(maxLockWait/time.Millisecond)),
		"`" + agentmemory.SourceReconciled + "`",
		"`\\u003c`, `\\u003e` and `\\u0026`",
		"`\\u2028` and `\\u2029`",
		"`" + agentmemory.MetaDescription + "`",
		"`reconciled`",
	} {
		if !strings.Contains(ex.text, want) {
			t.Errorf("the document does not state %s", want)
		}
	}
}
