// Package storetest is the conformance suite for [agentmemory.Store]
// implementations. Every store in this repository runs it; a store
// elsewhere can too.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/ChristopherDavenport/agentmemory"
)

// Options configure a run.
type Options struct {
	// New returns a fresh, empty store. Its MaxEntryBytes must be at
	// least 64, so the suite can write entries it can reason about.
	New func(t *testing.T) agentmemory.Store
	// Reopen returns a second handle on the same underlying storage as
	// s, for stores that persist. It is nil for a store that does not.
	Reopen func(t *testing.T, s agentmemory.Store) agentmemory.Store
}

// Run exercises a store through the whole interface.
func Run(t *testing.T, opts Options) {
	t.Helper()
	if opts.New == nil {
		t.Fatal("storetest: Options.New is required")
	}
	t.Run("Validation", func(t *testing.T) { testValidation(t, opts) })
	t.Run("PutGetList", func(t *testing.T) { testPutGetList(t, opts) })
	t.Run("Bounds", func(t *testing.T) { testBounds(t, opts) })
	t.Run("IfHash", func(t *testing.T) { testIfHash(t, opts) })
	t.Run("Forget", func(t *testing.T) { testForget(t, opts) })
	t.Run("Journal", func(t *testing.T) { testJournal(t, opts) })
	t.Run("Search", func(t *testing.T) { testSearch(t, opts) })
	t.Run("Concurrent", func(t *testing.T) { testConcurrent(t, opts) })
	if opts.Reopen != nil {
		t.Run("Persistence", func(t *testing.T) { testPersistence(t, opts) })
	}
}

func testValidation(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	tests := []struct {
		name string
		e    agentmemory.Entry
	}{
		{"empty scope", agentmemory.Entry{Scope: "", Name: "a", Content: "x"}},
		{"upper scope", agentmemory.Entry{Scope: "User", Name: "a", Content: "x"}},
		{"path scope", agentmemory.Entry{Scope: "../etc", Name: "a", Content: "x"}},
		{"empty name", agentmemory.Entry{Scope: "user", Name: "", Content: "x"}},
		{"spaced name", agentmemory.Entry{Scope: "user", Name: "a b", Content: "x"}},
		{"double hyphen", agentmemory.Entry{Scope: "user", Name: "a--b", Content: "x"}},
		{"leading hyphen", agentmemory.Entry{Scope: "user", Name: "-a", Content: "x"}},
		{"dotted name", agentmemory.Entry{Scope: "user", Name: "a.md", Content: "x"}},
		{"long name", agentmemory.Entry{Scope: "user", Name: strings.Repeat("a", agentmemory.MaxNameBytes+1), Content: "x"}},
		{"empty content", agentmemory.Entry{Scope: "user", Name: "a", Content: ""}},
		{"bad utf8", agentmemory.Entry{Scope: "user", Name: "a", Content: "x\xff"}},
		{"meta key", agentmemory.Entry{Scope: "user", Name: "a", Content: "x", Meta: map[string]string{"Type": "y"}}},
		{"reserved meta", agentmemory.Entry{Scope: "user", Name: "a", Content: "x", Meta: map[string]string{"name": "y"}}},
		{"multiline meta", agentmemory.Entry{Scope: "user", Name: "a", Content: "x", Meta: map[string]string{"description": "a\nb"}}},
		{"padded meta", agentmemory.Entry{Scope: "user", Name: "a", Content: "x", Meta: map[string]string{"description": " a "}}},
		{"meta too big", agentmemory.Entry{Scope: "user", Name: "a", Content: "x", Meta: map[string]string{"description": strings.Repeat("d", agentmemory.MaxMetaBytes)}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := st.Put(ctx, tt.e); !errors.Is(err, agentmemory.ErrInvalid) {
				t.Errorf("Put = %v, want ErrInvalid", err)
			}
		})
	}
	// Nothing invalid reached the store or its journal.
	if es, err := st.List(ctx, "user"); err != nil || len(es) != 0 {
		t.Errorf("List after invalid puts = %v, %v", es, err)
	}
	for c, err := range st.Journal(ctx, 0) {
		t.Errorf("journal after invalid puts has %+v, %v", c, err)
	}
}

func testPutGetList(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	if _, err := st.Get(ctx, "user", "missing"); !errors.Is(err, agentmemory.ErrNotFound) {
		t.Errorf("Get(missing) = %v, want ErrNotFound", err)
	}
	if es, err := st.List(ctx, "nowhere"); err != nil || len(es) != 0 {
		t.Errorf("List(unknown scope) = %v, %v; want none, nil", es, err)
	}
	style := agentmemory.Entry{Scope: "user", Name: "style", Content: "Short answers.\n", Meta: map[string]string{"description": "How to answer", "type": "feedback"}}
	if err := st.Put(ctx, style); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Content round-trips byte for byte: no trailing newline, unicode,
	// leading whitespace, a line that looks like frontmatter.
	odd := agentmemory.Entry{Scope: "user", Name: "odd", Content: "  ---\nname: not-meta\n---\nsüß ✓ 日本"}
	if err := st.Put(ctx, odd); err != nil {
		t.Fatalf("Put: %v", err)
	}
	proj := agentmemory.Entry{Scope: "project", Name: "build", Content: "make check"}
	if err := st.Put(ctx, proj); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := st.Get(ctx, "user", "style")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Hash != agentmemory.Hash(style.Content) || got.Updated.IsZero() || got.Deleted {
		t.Errorf("Get did not fill the entry: %+v", got)
	}
	if !sameEntry(*got, style) {
		t.Errorf("Get = %+v, want %+v", got, style)
	}
	got, err = st.Get(ctx, "user", "odd")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Content != odd.Content || got.Meta != nil {
		t.Errorf("Get(odd) = %q meta %v, want %q meta nil", got.Content, got.Meta, odd.Content)
	}
	// List is by name, one scope at a time.
	es, err := st.List(ctx, "user")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if names(es) != "odd,style" {
		t.Errorf("List(user) = %s, want odd,style", names(es))
	}
	for _, e := range es {
		if e.Hash != agentmemory.Hash(e.Content) || e.Updated.IsZero() || e.Scope != "user" {
			t.Errorf("List entry not filled: %+v", e)
		}
	}
	if es, _ := st.List(ctx, "project"); names(es) != "build" {
		t.Errorf("List(project) = %s, want build", names(es))
	}
	// Put replaces content and meta together.
	style.Content = "Long answers.\n"
	style.Meta = map[string]string{"type": "feedback"}
	if err := st.Put(ctx, style); err != nil {
		t.Fatalf("Put(replace): %v", err)
	}
	got, err = st.Get(ctx, "user", "style")
	if err != nil {
		t.Fatal(err)
	}
	if !sameEntry(*got, style) || got.Description() != "" {
		t.Errorf("Get after replace = %+v, want %+v", got, style)
	}
	// The store's copy is its own.
	got.Meta["type"] = "changed"
	if again, _ := st.Get(ctx, "user", "style"); again.Meta["type"] != "feedback" {
		t.Error("a caller's edit to a returned meta map reached the store")
	}
	// An empty meta map stores as none.
	if err := st.Put(ctx, agentmemory.Entry{Scope: "user", Name: "bare", Content: "x", Meta: map[string]string{}}); err != nil {
		t.Fatal(err)
	}
	if bare, _ := st.Get(ctx, "user", "bare"); bare.Meta != nil {
		t.Errorf("empty meta stored as %v, want nil", bare.Meta)
	}
}

func testBounds(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	limit := st.MaxEntryBytes()
	if limit < 256 {
		t.Fatalf("MaxEntryBytes = %d; the suite needs at least 256", limit)
	}
	at := agentmemory.Entry{Scope: "user", Name: "at", Content: strings.Repeat("a", limit)}
	if err := st.Put(ctx, at); err != nil {
		t.Fatalf("Put at the bound: %v", err)
	}
	// A create over the bound reports no stored size.
	over := agentmemory.Entry{Scope: "user", Name: "over", Content: strings.Repeat("b", limit+1)}
	err := st.Put(ctx, over)
	var se *agentmemory.SizeError
	if !errors.Is(err, agentmemory.ErrTooLarge) || !errors.As(err, &se) {
		t.Fatalf("Put over the bound = %v, want SizeError", err)
	}
	if se.Size != limit+1 || se.Limit != limit || se.Stored != -1 || se.Scope != "user" || se.Name != "over" {
		t.Errorf("SizeError = %+v", se)
	}
	if _, err := st.Get(ctx, "user", "over"); !errors.Is(err, agentmemory.ErrNotFound) {
		t.Errorf("over-bound content was stored: %v", err)
	}
	// A replace over the bound reports what the entry holds now.
	small := agentmemory.Entry{Scope: "user", Name: "small", Content: "ten bytes!"}
	if err := st.Put(ctx, small); err != nil {
		t.Fatal(err)
	}
	small.Content = strings.Repeat("c", limit+7)
	err = st.Put(ctx, small)
	if !errors.As(err, &se) {
		t.Fatalf("Put over the bound = %v, want SizeError", err)
	}
	if se.Stored != 10 || se.Size != limit+7 {
		t.Errorf("SizeError = %+v, want Stored 10 Size %d", se, limit+7)
	}
	if got, _ := st.Get(ctx, "user", "small"); got == nil || got.Content != "ten bytes!" {
		t.Errorf("a refused replace changed the entry: %+v", got)
	}
	if !strings.Contains(err.Error(), "holds 10 bytes now") {
		t.Errorf("error does not name the stored size: %v", err)
	}
	// The journal has the two good puts and nothing else.
	if n := count(t, st, 0); n != 2 {
		t.Errorf("journal has %d records, want 2", n)
	}
}

func testIfHash(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	e := agentmemory.Entry{Scope: "user", Name: "profile", Content: "Chris."}
	// A create that must not exist.
	if err := st.Put(ctx, e, agentmemory.IfHash("")); err != nil {
		t.Fatalf("Put(IfHash \"\") on a new entry: %v", err)
	}
	h1 := agentmemory.Hash(e.Content)
	var ce *agentmemory.ConflictError
	err := st.Put(ctx, e, agentmemory.IfHash(""))
	if !errors.Is(err, agentmemory.ErrConflict) || !errors.As(err, &ce) {
		t.Fatalf("Put(IfHash \"\") on an existing entry = %v, want ConflictError", err)
	}
	if ce.Want != "" || ce.Have != h1 {
		t.Errorf("ConflictError = %+v, want Have %s", ce, h1)
	}
	// A wrong hash.
	e.Content = "Chris. Prefers Go."
	err = st.Put(ctx, e, agentmemory.IfHash(agentmemory.Hash("stale")))
	if !errors.As(err, &ce) || ce.Have != h1 || ce.Want != agentmemory.Hash("stale") {
		t.Fatalf("Put(IfHash stale) = %v", err)
	}
	if got, _ := st.Get(ctx, "user", "profile"); got.Content != "Chris." {
		t.Errorf("a refused conditional Put changed the entry: %q", got.Content)
	}
	// The right hash.
	if err := st.Put(ctx, e, agentmemory.IfHash(h1)); err != nil {
		t.Fatalf("Put(IfHash right) = %v", err)
	}
	h2 := agentmemory.Hash(e.Content)
	// A missing entry with a hash expected.
	if err := st.Forget(ctx, "user", "profile"); err != nil {
		t.Fatal(err)
	}
	err = st.Put(ctx, e, agentmemory.IfHash(h2))
	if !errors.As(err, &ce) || ce.Have != "" || ce.Want != h2 {
		t.Fatalf("Put(IfHash) on a forgotten entry = %v", err)
	}
	// And after a tombstone the entry may be created again.
	if err := st.Put(ctx, e, agentmemory.IfHash("")); err != nil {
		t.Fatalf("Put(IfHash \"\") after Forget: %v", err)
	}
	// The precondition does not bypass validation.
	bad := agentmemory.Entry{Scope: "user", Name: "Bad", Content: "x"}
	if err := st.Put(ctx, bad, agentmemory.IfHash("")); !errors.Is(err, agentmemory.ErrInvalid) {
		t.Errorf("Put(invalid, IfHash) = %v, want ErrInvalid", err)
	}
	if n := count(t, st, 0); n != 4 {
		t.Errorf("journal has %d records, want 4", n)
	}
}

func testForget(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	if err := st.Forget(ctx, "user", "missing"); !errors.Is(err, agentmemory.ErrNotFound) {
		t.Errorf("Forget(missing) = %v, want ErrNotFound", err)
	}
	e := agentmemory.Entry{Scope: "user", Name: "tz", Content: "Europe/London", Meta: map[string]string{"description": "Timezone"}}
	if err := st.Put(ctx, e); err != nil {
		t.Fatal(err)
	}
	if err := st.Put(ctx, agentmemory.Entry{Scope: "user", Name: "keep", Content: "kept"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Forget(agentmemory.WithSession(ctx, "sess-f"), "user", "tz"); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if _, err := st.Get(ctx, "user", "tz"); !errors.Is(err, agentmemory.ErrNotFound) {
		t.Errorf("Get after Forget = %v, want ErrNotFound", err)
	}
	if es, _ := st.List(ctx, "user"); names(es) != "keep" {
		t.Errorf("List after Forget = %s, want keep", names(es))
	}
	if err := st.Forget(ctx, "user", "tz"); !errors.Is(err, agentmemory.ErrNotFound) {
		t.Errorf("second Forget = %v, want ErrNotFound", err)
	}
	if found, _ := st.Search(ctx, []agentmemory.Scope{"user"}, "London", 0); len(found) != 0 {
		t.Errorf("Search finds a forgotten entry: %+v", found)
	}
	// The tombstone keeps the last content and names what it replaced.
	changes := collect(t, st, 0)
	if len(changes) != 3 {
		t.Fatalf("journal = %d records, want 3", len(changes))
	}
	tomb := changes[2]
	if !tomb.Entry.Deleted || tomb.Entry.Content != "Europe/London" || tomb.Entry.Hash != agentmemory.Hash("Europe/London") ||
		tomb.Prev != tomb.Entry.Hash || tomb.Session != "sess-f" || tomb.Entry.Description() != "Timezone" || tomb.Entry.Updated.IsZero() {
		t.Errorf("tombstone = %+v", tomb)
	}
	// Recreating after a tombstone is a create: Prev is empty.
	if err := st.Put(ctx, e); err != nil {
		t.Fatal(err)
	}
	if changes := collect(t, st, 3); len(changes) != 1 || changes[0].Prev != "" || changes[0].Entry.Deleted {
		t.Errorf("record after recreate = %+v", changes)
	}
}

func testJournal(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	for c, err := range st.Journal(ctx, 0) {
		t.Errorf("empty journal yields %+v, %v", c, err)
	}
	a := agentmemory.WithSession(ctx, "sess-a")
	b := agentmemory.WithSession(ctx, "sess-b")
	steps := []struct {
		ctx  context.Context
		e    agentmemory.Entry
		prev string
	}{
		{a, agentmemory.Entry{Scope: "user", Name: "one", Content: "1"}, ""},
		{b, agentmemory.Entry{Scope: "user", Name: "two", Content: "2"}, ""},
		{b, agentmemory.Entry{Scope: "user", Name: "one", Content: "1b"}, agentmemory.Hash("1")},
		{ctx, agentmemory.Entry{Scope: "project", Name: "one", Content: "p1"}, ""},
		{a, agentmemory.Entry{Scope: "user", Name: "one", Content: "1c", Meta: map[string]string{"k": "v"}}, agentmemory.Hash("1b")},
	}
	for i, s := range steps {
		if err := st.Put(s.ctx, s.e); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	changes := collect(t, st, 0)
	if len(changes) != len(steps) {
		t.Fatalf("journal = %d records, want %d", len(changes), len(steps))
	}
	for i, c := range changes {
		s := steps[i]
		if c.Seq != uint64(i+1) {
			t.Errorf("record %d: Seq = %d, want %d", i, c.Seq, i+1)
		}
		if c.Entry.Scope != s.e.Scope || c.Entry.Name != s.e.Name || c.Entry.Content != s.e.Content || !reflect.DeepEqual(c.Entry.Meta, s.e.Meta) {
			t.Errorf("record %d: entry = %+v, want %+v", i, c.Entry, s.e)
		}
		if c.Entry.Hash != agentmemory.Hash(s.e.Content) || c.Entry.Deleted || c.Entry.Updated.IsZero() {
			t.Errorf("record %d: entry not filled: %+v", i, c.Entry)
		}
		if c.Prev != s.prev {
			t.Errorf("record %d: Prev = %q, want %q", i, c.Prev, s.prev)
		}
		if c.Session != agentmemory.SessionFrom(s.ctx) {
			t.Errorf("record %d: Session = %q, want %q", i, c.Session, agentmemory.SessionFrom(s.ctx))
		}
		if c.At.IsZero() {
			t.Errorf("record %d: At is zero", i)
		}
		if i > 0 && c.At.Before(changes[i-1].At) {
			t.Errorf("record %d: At %v before record %d: %v", i, c.At, i-1, changes[i-1].At)
		}
	}
	// The record's Updated is the entry's.
	if got, _ := st.Get(ctx, "user", "one"); !got.Updated.Equal(changes[4].Entry.Updated) {
		t.Errorf("Get Updated %v, journal Updated %v", got.Updated, changes[4].Entry.Updated)
	}
	// after is exclusive and resumes from any point.
	for _, after := range []uint64{0, 1, 3, 4, 5, 6, 100} {
		got := collect(t, st, after)
		want := 0
		if after < uint64(len(steps)) {
			want = len(steps) - int(after)
		}
		if len(got) != want {
			t.Errorf("Journal(%d) = %d records, want %d", after, len(got), want)
		}
		if len(got) > 0 && got[0].Seq != after+1 {
			t.Errorf("Journal(%d) starts at %d", after, got[0].Seq)
		}
	}
	// Stopping early is allowed.
	n := 0
	for range st.Journal(ctx, 0) {
		n++
		if n == 2 {
			break
		}
	}
	// A record is the caller's to change.
	first := collect(t, st, 0)[0]
	first.Entry.Content = "changed"
	if again := collect(t, st, 0)[0]; again.Entry.Content != "1" {
		t.Error("a caller's edit to a yielded record reached the store")
	}
}

func testSearch(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	puts := []agentmemory.Entry{
		{Scope: "user", Name: "editor", Content: "Uses Neovim with a dark theme.", Meta: map[string]string{"description": "Editor preference"}},
		{Scope: "user", Name: "language", Content: "Prefers Go over Python for tools.", Meta: map[string]string{"description": "Language preference"}},
		{Scope: "user", Name: "timezone", Content: "Europe/London"},
		{Scope: "project", Name: "build", Content: "Run make check before a commit; the Go toolchain is 1.25."},
		{Scope: "hidden", Name: "secret", Content: "Go is also mentioned here."},
	}
	for _, e := range puts {
		if err := st.Put(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	user := []agentmemory.Scope{"user"}
	both := []agentmemory.Scope{"user", "project"}
	tests := []struct {
		name   string
		scopes []agentmemory.Scope
		query  string
		limit  int
		want   []string // scope/name, order-insensitive
	}{
		{"one word, one scope", user, "neovim", 0, []string{"user/editor"}},
		{"case", user, "NEOVIM", 0, []string{"user/editor"}},
		{"across scopes", both, "Go", 0, []string{"user/language", "project/build"}},
		{"scope restricts", user, "Go", 0, []string{"user/language"}},
		{"all words must match", both, "Go Python", 0, []string{"user/language"}},
		{"name matches", user, "timezone", 0, []string{"user/timezone"}},
		{"description matches", user, "preference", 0, []string{"user/editor", "user/language"}},
		{"no match", both, "rust", 0, nil},
		{"unknown scope", []agentmemory.Scope{"nowhere"}, "go", 0, nil},
		{"limit", both, "Go", 1, nil}, // any one of the two
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := st.Search(ctx, tt.scopes, tt.query, tt.limit)
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if tt.limit > 0 {
				if len(got) != tt.limit {
					t.Errorf("Search with limit %d returned %d", tt.limit, len(got))
				}
				return
			}
			if !sameSet(keys(got), tt.want) {
				t.Errorf("Search = %v, want %v", keys(got), tt.want)
			}
			for _, e := range got {
				if e.Hash == "" || e.Updated.IsZero() || e.Deleted {
					t.Errorf("Search entry not filled: %+v", e)
				}
			}
		})
	}
}

// testConcurrent runs several writers against one store: each writes
// its own entries and patches one shared entry through IfHash with a
// retry, as the memory_patch tool does. The journal must come out
// contiguous, every writer's own entries must be present, and the
// shared entry must hold every writer's fact.
func testConcurrent(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	const writers, each = 4, 8
	if err := st.Put(ctx, agentmemory.Entry{Scope: "user", Name: "shared", Content: "facts:"}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, writers*each*2)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			wctx := agentmemory.WithSession(ctx, fmt.Sprintf("sess-%d", w))
			for i := 0; i < each; i++ {
				e := agentmemory.Entry{Scope: "user", Name: fmt.Sprintf("w%d-n%d", w, i), Content: fmt.Sprintf("writer %d entry %d", w, i)}
				if err := st.Put(wctx, e); err != nil {
					errs <- err
					return
				}
				if err := appendFact(wctx, st, fmt.Sprintf(" w%d-%d", w, i)); err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("writer: %v", err)
	}
	shared, err := st.Get(ctx, "user", "shared")
	if err != nil {
		t.Fatal(err)
	}
	for w := 0; w < writers; w++ {
		for i := 0; i < each; i++ {
			if fact := fmt.Sprintf(" w%d-%d", w, i); !strings.Contains(shared.Content, fact) {
				t.Errorf("shared entry lost %q", fact)
			}
			if _, err := st.Get(ctx, "user", fmt.Sprintf("w%d-n%d", w, i)); err != nil {
				t.Errorf("own entry: %v", err)
			}
		}
	}
	changes := collect(t, st, 0)
	if want := 1 + writers*each*2; len(changes) != want {
		t.Errorf("journal = %d records, want %d", len(changes), want)
	}
	last := map[string]string{}
	for i, c := range changes {
		if c.Seq != uint64(i+1) {
			t.Errorf("record %d: Seq = %d", i, c.Seq)
		}
		key := string(c.Entry.Scope) + "/" + c.Entry.Name
		if c.Prev != last[key] {
			t.Errorf("record %d (%s): Prev = %q, want %q; the journal is not a chain", i, key, c.Prev, last[key])
		}
		last[key] = c.Entry.Hash
	}
	if last["user/shared"] != shared.Hash {
		t.Errorf("journal's last hash for shared %s, live %s", last["user/shared"], shared.Hash)
	}
}

// appendFact adds fact to the shared entry with the read-patch-put
// loop the tools use: each attempt is anchored in what it read, and a
// conflict re-reads.
func appendFact(ctx context.Context, st agentmemory.Store, fact string) error {
	for attempt := 0; ; attempt++ {
		cur, err := st.Get(ctx, "user", "shared")
		if err != nil {
			return err
		}
		next := *cur
		next.Content = cur.Content + fact
		err = st.Put(ctx, next, agentmemory.IfHash(cur.Hash))
		if err == nil {
			return nil
		}
		if !errors.Is(err, agentmemory.ErrConflict) || attempt == 200 {
			return err
		}
	}
}

func testPersistence(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	a := agentmemory.WithSession(ctx, "sess-p")
	puts := []agentmemory.Entry{
		{Scope: "user", Name: "style", Content: "Short answers.\n", Meta: map[string]string{"description": "How to answer", "type": "feedback"}},
		{Scope: "user", Name: "gone", Content: "to be forgotten"},
		{Scope: "user", Name: "odd", Content: "  ---\nname: not-meta\n---\nsüß ✓ 日本"},
		{Scope: "project", Name: "build", Content: "make check"},
	}
	for _, e := range puts {
		if err := st.Put(a, e); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Forget(a, "user", "gone"); err != nil {
		t.Fatal(err)
	}
	before := collect(t, st, 0)
	style, _ := st.Get(ctx, "user", "style")

	st2 := opts.Reopen(t, st)
	if st2.MaxEntryBytes() != st.MaxEntryBytes() {
		t.Errorf("MaxEntryBytes after reopen = %d, want %d", st2.MaxEntryBytes(), st.MaxEntryBytes())
	}
	again, err := st2.Get(ctx, "user", "style")
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if !sameEntry(*again, *style) || again.Hash != style.Hash || !again.Updated.Equal(style.Updated) {
		t.Errorf("Get after reopen = %+v, want %+v", again, style)
	}
	if odd, _ := st2.Get(ctx, "user", "odd"); odd == nil || odd.Content != puts[2].Content {
		t.Errorf("odd content after reopen = %+v", odd)
	}
	if _, err := st2.Get(ctx, "user", "gone"); !errors.Is(err, agentmemory.ErrNotFound) {
		t.Errorf("forgotten entry after reopen = %v", err)
	}
	if es, _ := st2.List(ctx, "user"); names(es) != "odd,style" {
		t.Errorf("List after reopen = %s", names(es))
	}
	after := collect(t, st2, 0)
	if len(after) != len(before) {
		t.Fatalf("journal after reopen = %d records, want %d", len(after), len(before))
	}
	for i := range before {
		b, a := before[i], after[i]
		if b.Seq != a.Seq || b.Prev != a.Prev || b.Session != a.Session || !b.At.Equal(a.At) ||
			!sameEntry(b.Entry, a.Entry) || b.Entry.Hash != a.Entry.Hash || b.Entry.Deleted != a.Entry.Deleted || !b.Entry.Updated.Equal(a.Entry.Updated) {
			t.Errorf("record %d after reopen = %+v, want %+v", i, a, b)
		}
	}
	// Writing through the second handle continues the sequence, and
	// the first handle sees it.
	if err := st2.Put(ctx, agentmemory.Entry{Scope: "user", Name: "more", Content: "more"}); err != nil {
		t.Fatal(err)
	}
	if more := collect(t, st, uint64(len(before))); len(more) != 1 || more[0].Seq != uint64(len(before))+1 {
		t.Errorf("first handle after a write through the second: %+v", more)
	}
	if _, err := st.Get(ctx, "user", "more"); err != nil {
		t.Errorf("first handle does not see the second's write: %v", err)
	}
	if found, _ := opts.Reopen(t, st2).Search(ctx, []agentmemory.Scope{"user"}, "more", 0); len(found) != 1 {
		t.Errorf("Search after reopen = %+v", found)
	}
}

func collect(t *testing.T, st agentmemory.Store, after uint64) []agentmemory.Change {
	t.Helper()
	var out []agentmemory.Change
	for c, err := range st.Journal(context.Background(), after) {
		if err != nil {
			t.Fatalf("Journal: %v", err)
		}
		out = append(out, c)
	}
	return out
}

func count(t *testing.T, st agentmemory.Store, after uint64) int {
	t.Helper()
	return len(collect(t, st, after))
}

func names(es []agentmemory.Entry) string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.Name)
	}
	return strings.Join(out, ",")
}

func keys(es []agentmemory.Entry) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, string(e.Scope)+"/"+e.Name)
	}
	return out
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

// sameEntry compares the fields a caller sets: scope, name, content
// and meta, with an empty meta equal to none.
func sameEntry(a, b agentmemory.Entry) bool {
	if a.Scope != b.Scope || a.Name != b.Name || a.Content != b.Content {
		return false
	}
	if len(a.Meta) == 0 && len(b.Meta) == 0 {
		return true
	}
	return reflect.DeepEqual(a.Meta, b.Meta)
}
