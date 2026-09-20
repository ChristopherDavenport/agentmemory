package agentmemory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files under testdata/render")

// renderCase is one golden fixture: a store, the scopes rendered, and
// the total bound.
type renderCase struct {
	name     string
	scopes   []Scope
	maxTotal int
	max      int // the store's entry bound
	entries  []Entry
}

// filler returns content of exactly n bytes of one letter, ending in
// a newline, so each entry's content is its own.
func filler(n int, c byte) string {
	return strings.Repeat(string(c), n-1) + "\n"
}

func renderCases() []renderCase {
	return []renderCase{
		{name: "empty", scopes: []Scope{"user", "project"}},
		{name: "no-scopes", scopes: nil, entries: []Entry{{Scope: "user", Name: "unseen", Content: "not rendered"}}},
		{name: "one-scope", scopes: []Scope{"user"}, entries: []Entry{
			{Scope: "user", Name: "style", Content: "Short answers.\n\nCode in Go.\n", Meta: map[string]string{"description": "How the user likes answers", "type": "feedback"}},
			{Scope: "user", Name: "timezone", Content: "Europe/London"},
			{Scope: "project", Name: "unlisted", Content: "not in the scopes"},
		}},
		{name: "two-scopes", scopes: []Scope{"user", "project"}, entries: []Entry{
			{Scope: "user", Name: "editor", Content: "Neovim, dark theme — süß ✓ 日本\n", Meta: map[string]string{"description": "Editor"}},
			{Scope: "project", Name: "build", Content: "#### Build\n\nRun `make check` before a commit.\n"},
		}},
		{name: "at-bound", scopes: []Scope{"user"}, maxTotal: 1024, max: 256, entries: []Entry{
			{Scope: "user", Name: "a", Content: filler(256, 'a')},
			{Scope: "user", Name: "b", Content: filler(256, 'b')},
			{Scope: "user", Name: "c", Content: filler(256, 'c')},
			{Scope: "user", Name: "d", Content: filler(256, 'd')},
		}},
		{name: "omitted", scopes: []Scope{"user", "project", "empty"}, maxTotal: 1000, max: 256, entries: []Entry{
			{Scope: "user", Name: "a", Content: filler(256, 'a')},
			{Scope: "user", Name: "b", Content: filler(256, 'b')},
			{Scope: "user", Name: "c", Content: filler(256, 'c'), Meta: map[string]string{"description": "The last one shown"}},
			{Scope: "user", Name: "d", Content: filler(256, 'd'), Meta: map[string]string{"description": "Does not fit"}},
			{Scope: "user", Name: "e", Content: filler(100, 'e')}, // would fit, but the block stops at the first that does not
			{Scope: "project", Name: "build", Content: "make check\n"},
		}},
	}
}

func TestRenderGolden(t *testing.T) {
	ctx := context.Background()
	for _, tc := range renderCases() {
		t.Run(tc.name, func(t *testing.T) {
			var opts []MemOption
			if tc.max > 0 {
				opts = append(opts, WithMaxEntryBytes(tc.max))
			}
			store := NewMemStore(opts...)
			for _, e := range tc.entries {
				if err := store.Put(ctx, e); err != nil {
					t.Fatal(err)
				}
			}
			var ropts []RenderOption
			if tc.maxTotal > 0 {
				ropts = append(ropts, WithMaxTotalBytes(tc.maxTotal))
			}
			block, man, err := Render(ctx, store, tc.scopes, ropts...)
			if err != nil {
				t.Fatal(err)
			}
			checkGolden(t, filepath.Join("testdata", "render", tc.name+".md"), []byte(block))
			manJSON, err := json.MarshalIndent(man, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			checkGolden(t, filepath.Join("testdata", "render", tc.name+".manifest.json"), append(manJSON, '\n'))
			// The manifest's hashes are the hashes of what the block
			// shows, and the block shows exactly the manifest's entries.
			for _, me := range man.Entries {
				e, err := store.Get(ctx, me.Scope, me.Name)
				if err != nil {
					t.Fatal(err)
				}
				if me.Hash != e.Hash || me.Bytes != e.Size() || !strings.Contains(block, e.Content) {
					t.Errorf("manifest entry %s/%s does not match the block", me.Scope, me.Name)
				}
			}
			for _, me := range man.Omitted {
				e, _ := store.Get(ctx, me.Scope, me.Name)
				if me.Hash != e.Hash || strings.Contains(block, e.Content) {
					t.Errorf("omitted entry %s/%s is in the block", me.Scope, me.Name)
				}
			}
			// Rendering again gives the same bytes.
			again, _, err := Render(ctx, store, tc.scopes, ropts...)
			if err != nil || again != block {
				t.Error("Render is not stable")
			}
		})
	}
}

// TestRenderBudget fills the block to the default bound: eight entries
// of the default entry bound render whole, a ninth is omitted.
func TestRenderBudget(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	for i := 0; i < 9; i++ {
		if err := store.Put(ctx, Entry{Scope: "user", Name: "note-" + string(rune('a'+i)), Content: filler(DefaultMaxEntryBytes, byte('a'+i))}); err != nil {
			t.Fatal(err)
		}
	}
	block, man, err := Render(ctx, store, []Scope{"user"})
	if err != nil {
		t.Fatal(err)
	}
	if len(man.Entries) != 8 || len(man.Omitted) != 1 || man.Omitted[0].Name != "note-i" {
		t.Errorf("manifest: %d shown, %+v omitted", len(man.Entries), man.Omitted)
	}
	if !strings.Contains(block, "Entries: 8 shown, 1 omitted. Used: 32768 of 32768 bytes (0 free). Entry limit: 4096 bytes.") {
		t.Errorf("header: %s", strings.SplitN(block, "\n", 4)[2])
	}
	if !strings.Contains(block, "Not shown, over the total budget; fetch with memory_search: note-i (4096 bytes)") {
		t.Error("omitted line missing")
	}
	if len(block) < DefaultMaxTotalBytes {
		t.Errorf("block is %d bytes; the budget is on content, headings are extra", len(block))
	}
}

func TestRenderErrors(t *testing.T) {
	ctx := context.Background()
	if _, _, err := Render(ctx, NewMemStore(), []Scope{"user", "Bad"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("Render with a bad scope = %v", err)
	}
	if _, _, err := Render(ctx, failing{}, []Scope{"user"}); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("Render over a failing store = %v", err)
	}
}

type failing struct{ Store }

func (failing) List(context.Context, Scope) ([]Entry, error) { return nil, errors.New("boom") }
func (failing) MaxEntryBytes() int                           { return DefaultMaxEntryBytes }

func TestUsage(t *testing.T) {
	u := Usage()
	for _, name := range []string{SaveTool, PatchTool, ForgetTool, SearchTool} {
		if !strings.Contains(u, name) {
			t.Errorf("Usage does not name %s", name)
		}
	}
}

// checkGolden compares got with the golden file, or rewrites it under
// -update.
func checkGolden(t *testing.T, path string, got []byte) {
	t.Helper()
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test . -update)", err)
	}
	if !bytes.Equal(want, got) {
		t.Errorf("%s differs from golden:\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}
