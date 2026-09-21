package agentmemory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
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
			{Scope: "user", Name: "e", Content: filler(100, 'e')}, // fits in what d left, and is not hidden by it
			{Scope: "project", Name: "build", Content: "make check\n"},
		}},
		// One large entry early in the alphabet must not hide the small
		// ones after it: the budget is packed in list order.
		{name: "skipped", scopes: []Scope{"user"}, maxTotal: 900, max: 512, entries: []Entry{
			{Scope: "user", Name: "architecture-notes", Content: filler(512, 'n'), Meta: map[string]string{"description": "Long"}},
			{Scope: "user", Name: "bristol", Content: "Lives in Bristol.\n"},
			{Scope: "user", Name: "more-notes", Content: filler(500, 'm')},
			{Scope: "user", Name: "zz-passport", Content: "In the top drawer.\n"},
		}},
		// Many short entries with descriptions: the headings and the
		// descriptions are the block, and the bound counts them.
		{name: "many-small", scopes: []Scope{"user"}, maxTotal: 700, max: 256, entries: manySmall(12)},
	}
}

// manySmall returns n short entries, each with a description of its
// own, as a store of one fact per file accumulates.
func manySmall(n int) []Entry {
	out := make([]Entry, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, Entry{
			Scope:   "user",
			Name:    fmt.Sprintf("note-%02d", i),
			Content: fmt.Sprintf("Fact %d.\n", i),
			Meta:    map[string]string{"description": fmt.Sprintf("What fact %d is for", i)},
		})
	}
	return out
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
			// The bound is on the block, and the header says so.
			max := tc.maxTotal
			if max <= 0 {
				max = DefaultMaxTotalBytes
			}
			if len(block) > max {
				t.Errorf("block is %d bytes, over the %d byte bound", len(block), max)
			}
			if !strings.Contains(block, fmt.Sprintf("Block: %d of %d bytes (%d free).", len(block), max, max-len(block))) {
				t.Errorf("the header does not state the block's own size (%d of %d):\n%s", len(block), max, firstLines(block, 3))
			}
			for _, me := range man.Omitted {
				if me.Reason != OmitBudget {
					t.Errorf("omitted entry %s/%s has reason %q", me.Scope, me.Name, me.Reason)
				}
			}
			for _, me := range man.Entries {
				if me.Reason != "" {
					t.Errorf("included entry %s/%s has reason %q", me.Scope, me.Name, me.Reason)
				}
			}
		})
	}
}

// TestRenderBudget fills the block to the default bound: eight entries
// of the default entry bound are 32,768 bytes of content, which no
// longer fits once the block's own text is counted, so the eighth is
// left out and the block stays inside the bound.
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
	if len(man.Entries) != 7 || len(man.Omitted) != 2 || man.Omitted[0].Name != "note-h" || man.Omitted[1].Name != "note-i" {
		t.Errorf("manifest: %d shown, %+v omitted", len(man.Entries), man.Omitted)
	}
	if len(block) > DefaultMaxTotalBytes {
		t.Errorf("block is %d bytes, over the %d byte bound", len(block), DefaultMaxTotalBytes)
	}
	if !strings.Contains(block, fmt.Sprintf("Entries: 7 shown, 2 omitted. Block: %d of 32768 bytes (%d free). Entry limit: 4096 bytes.", len(block), DefaultMaxTotalBytes-len(block))) {
		t.Errorf("header: %s", firstLines(block, 3))
	}
	if !strings.Contains(block, "Not shown, over the block budget; fetch with memory_search: note-h (4096 bytes), note-i (4096 bytes)") {
		t.Errorf("omitted line missing:\n%s", firstLines(block, 3))
	}
}

// TestRenderPacksTheBudget is the case the block used to hide: a large
// entry early by name and a small one after it, with room for the small
// one. The large one is skipped, the small one is shown, and the order
// of the block is still list order.
func TestRenderPacksTheBudget(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	for i := 0; i < 12; i++ {
		if err := store.Put(ctx, Entry{Scope: "user", Name: fmt.Sprintf("note-%02d", i), Content: filler(3000, 'n')}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Put(ctx, Entry{Scope: "user", Name: "zz-passport", Content: "In the top drawer.\n"}); err != nil {
		t.Fatal(err)
	}
	block, man, err := Render(ctx, store, []Scope{"user"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(block, "### zz-passport") {
		t.Errorf("the 19 byte entry is not in a block with %d bytes free:\n%s", DefaultMaxTotalBytes-len(block), firstLines(block, 3))
	}
	for _, me := range man.Omitted {
		if me.Name == "zz-passport" {
			t.Error("zz-passport is omitted")
		}
	}
	// List order, whichever were skipped.
	var order []string
	for _, me := range man.Entries {
		order = append(order, me.Name)
	}
	if !sortedNames(order) {
		t.Errorf("the block is not in list order: %v", order)
	}
}

// TestRenderCountsTheBlock is the case the bound used to miss: many
// short entries whose headings and descriptions are most of the block.
func TestRenderCountsTheBlock(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	for i := 0; i < 600; i++ {
		if err := store.Put(ctx, Entry{
			Scope:   "user",
			Name:    fmt.Sprintf("note-%03d", i),
			Content: filler(20, 'x'),
			Meta:    map[string]string{"description": strings.Repeat("d", 200)},
		}); err != nil {
			t.Fatal(err)
		}
	}
	block, man, err := Render(ctx, store, []Scope{"user"})
	if err != nil {
		t.Fatal(err)
	}
	if len(block) > DefaultMaxTotalBytes {
		t.Errorf("block is %d bytes, over the %d byte bound", len(block), DefaultMaxTotalBytes)
	}
	if len(man.Entries)+len(man.Omitted) != 600 || len(man.Omitted) == 0 {
		t.Errorf("manifest: %d shown, %d omitted of 600", len(man.Entries), len(man.Omitted))
	}
	// Every omitted entry is either named in the block or counted there.
	if !strings.Contains(block, "Not shown, over the block budget; fetch with memory_search:") {
		t.Error("the block does not say what it left out")
	}
}

// TestRenderTinyBound is the pathological end: a bound too small for
// the entries and nearly too small for their names.
func TestRenderTinyBound(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	for i := 0; i < 8; i++ {
		if err := store.Put(ctx, Entry{Scope: "user", Name: fmt.Sprintf("note-%d", i), Content: filler(200, 'x')}); err != nil {
			t.Fatal(err)
		}
	}
	block, man, err := Render(ctx, store, []Scope{"user"}, WithMaxTotalBytes(180))
	if err != nil {
		t.Fatal(err)
	}
	if len(man.Entries) != 0 || len(man.Omitted) != 8 {
		t.Errorf("manifest: %d shown, %d omitted", len(man.Entries), len(man.Omitted))
	}
	if len(block) > 180 {
		t.Errorf("block is %d bytes, over the 180 byte bound:\n%s", len(block), block)
	}
	if !strings.Contains(block, "Not shown, over the block budget: 8 entries.") {
		t.Errorf("the block does not count what it could not name:\n%s", block)
	}
}

func sortedNames(names []string) bool {
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			return false
		}
	}
	return true
}

func firstLines(s string, n int) string {
	parts := strings.SplitN(s, "\n", n+1)
	if len(parts) > n {
		parts = parts[:n]
	}
	return strings.Join(parts, "\n")
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
