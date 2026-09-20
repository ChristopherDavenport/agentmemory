package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ChristopherDavenport/agentmemory"
	"github.com/ChristopherDavenport/agentmemory/storetest"
)

func openStore(t *testing.T, path string, opts ...Option) *Store {
	t.Helper()
	s, err := Open(path, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestStore(t *testing.T) {
	storetest.Run(t, storetest.Options{
		New: func(t *testing.T) agentmemory.Store {
			return openStore(t, filepath.Join(t.TempDir(), "memory.db"))
		},
		Reopen: func(t *testing.T, s agentmemory.Store) agentmemory.Store {
			return openStore(t, s.(*Store).Path())
		},
	})
}

func TestStoreSmallBound(t *testing.T) {
	storetest.Run(t, storetest.Options{
		New: func(t *testing.T) agentmemory.Store {
			return openStore(t, filepath.Join(t.TempDir(), "memory.db"), WithMaxEntryBytes(256))
		},
	})
}

// TestSearchRanking checks what the FTS index adds over a substring
// scan: relevance order, token matching across punctuation, and the
// index following writes and tombstones.
func TestSearchRanking(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, filepath.Join(t.TempDir(), "memory.db"))
	puts := []agentmemory.Entry{
		{Scope: "user", Name: "mention", Content: "Go is mentioned once in a long note about many other things, none of which matter here."},
		{Scope: "user", Name: "focus", Content: "Go. Go tooling. Go modules.", Meta: map[string]string{"description": "Go notes"}},
		{Scope: "user", Name: "path", Content: "Timezone Europe/London; editor: nvim-qt"},
	}
	for _, e := range puts {
		if err := s.Put(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Search(ctx, []agentmemory.Scope{"user"}, "go", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "focus" || got[1].Name != "mention" {
		t.Errorf("Search(go) = %v, want focus then mention", names(got))
	}
	for _, q := range []string{"london", "Europe", "nvim", "qt", "europe/london"} {
		got, err := s.Search(ctx, []agentmemory.Scope{"user"}, q, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Name != "path" {
			t.Errorf("Search(%q) = %v, want path", q, names(got))
		}
	}
	// Prefixes match; a query with FTS syntax in it is still words.
	for _, q := range []string{"tool", `"go"`, "go*", "(go)", "-go"} {
		if got, err := s.Search(ctx, []agentmemory.Scope{"user"}, q, 0); err != nil || len(got) == 0 {
			t.Errorf("Search(%q) = %v, %v", q, names(got), err)
		}
	}
	// The index follows a replace and a tombstone.
	if err := s.Put(ctx, agentmemory.Entry{Scope: "user", Name: "focus", Content: "Rust now."}); err != nil {
		t.Fatal(err)
	}
	if err := s.Forget(ctx, "user", "mention"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Search(ctx, []agentmemory.Scope{"user"}, "go", 0); len(got) != 0 {
		t.Errorf("Search(go) after replace and forget = %v", names(got))
	}
	if got, _ := s.Search(ctx, []agentmemory.Scope{"user"}, "rust", 0); len(got) != 1 {
		t.Errorf("Search(rust) = %v", names(got))
	}
	if got, _ := s.Search(ctx, []agentmemory.Scope{"user"}, "", 0); len(got) != 2 {
		t.Errorf("Search(\"\") = %v, want every entry", names(got))
	}
	if got, _ := s.Search(ctx, nil, "go", 0); len(got) != 0 {
		t.Errorf("Search over no scopes = %v", names(got))
	}
}

func names(es []agentmemory.Entry) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.Name)
	}
	return out
}

// TestSearchOddQueries checks that queries with no tokens or with
// characters FTS5 would read as syntax neither error nor match
// everything, and that a word is a prefix ("NOT" finds "notes").
func TestSearchOddQueries(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, filepath.Join(t.TempDir(), "memory.db"))
	if err := s.Put(ctx, agentmemory.Entry{Scope: "user", Name: "a", Content: "C++ and 日本語 notes; see a-b."}); err != nil {
		t.Fatal(err)
	}
	for q, want := range map[string]int{"...": 0, "\"\"": 0, "*": 0, "c++": 1, "日本語": 1, "a-b": 1, "notes;": 1, "AND": 1, "NOT": 1, "(": 0} {
		got, err := s.Search(ctx, []agentmemory.Scope{"user"}, q, 0)
		if err != nil {
			t.Errorf("Search(%q): %v", q, err)
			continue
		}
		if len(got) != want {
			t.Errorf("Search(%q) = %d entries, want %d", q, len(got), want)
		}
	}
}
