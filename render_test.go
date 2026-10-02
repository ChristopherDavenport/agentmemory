package agentmemory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
	scopeMax map[Scope]int
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
		// A full scope rendered first, capped, leaves the scope after it
		// its room: the cap counts its entries and its omission line.
		{name: "scope-cap", scopes: []Scope{"environment", "user"}, maxTotal: 1000, max: 256, scopeMax: map[Scope]int{"environment": 400},
			entries: append(scopeEntries("environment", 8, 60), scopeEntries("user", 2, 40)...)},
	}
}

// scopeEntries returns n entries of size bytes in scope.
func scopeEntries(scope Scope, n, size int) []Entry {
	out := make([]Entry, 0, n)
	for i := range n {
		out = append(out, Entry{Scope: scope, Name: fmt.Sprintf("%s-%02d", scope, i), Content: filler(size, byte('a'+i))})
	}
	return out
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
				if _, err := store.Put(ctx, e); err != nil {
					t.Fatal(err)
				}
			}
			var ropts []RenderOption
			if tc.maxTotal > 0 {
				ropts = append(ropts, WithMaxTotalBytes(tc.maxTotal))
			}
			for scope, n := range tc.scopeMax {
				ropts = append(ropts, WithScopeMaxBytes(scope, n))
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
			// The parts are the block, and their IDs are pinned too: a
			// product records them, so a change to one is a change to
			// every session that does.
			parts, partsMan, err := RenderParts(ctx, store, tc.scopes, ropts...)
			if err != nil {
				t.Fatal(err)
			}
			if JoinParts(parts) != block || partsMan.Hash() != man.Hash() {
				t.Error("RenderParts joined is not Render")
			}
			var ids strings.Builder
			for _, p := range parts {
				fmt.Fprintf(&ids, "%s %d\n", p.ID, len(p.Text))
			}
			checkGolden(t, filepath.Join("testdata", "render", tc.name+".parts"), []byte(ids.String()))
			// The manifest's hashes are the hashes of what the block
			// shows, and the block shows exactly the manifest's entries.
			for _, me := range man.Entries {
				e, err := store.Get(ctx, me.Scope, me.Name)
				if err != nil {
					t.Fatal(err)
				}
				if me.Hash != e.Hash || me.Bytes != e.Size() || !strings.Contains(block, strings.TrimSuffix(e.Content, "\n")) {
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
			// The bound is on the block, and its last line says so.
			max := tc.maxTotal
			if max <= 0 {
				max = DefaultMaxTotalBytes
			}
			if len(block) > max {
				t.Errorf("block is %d bytes, over the %d byte bound", len(block), max)
			}
			if !strings.HasSuffix(block, blockLine(len(block), max)+" Entry limit: "+strconv.Itoa(store.MaxEntryBytes())+" bytes.") {
				t.Errorf("the last line does not state the block's own size (%d of %d):\n%s", len(block), max, lastLine(block))
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
		if _, err := store.Put(ctx, Entry{Scope: "user", Name: "note-" + string(rune('a'+i)), Content: filler(DefaultMaxEntryBytes, byte('a'+i))}); err != nil {
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
	if !strings.Contains(block, "Entries: 7 shown, 2 omitted. "+blockLine(len(block), DefaultMaxTotalBytes)+" Entry limit: 4096 bytes.") {
		t.Errorf("summary: %s", lastLine(block))
	}
	if !strings.Contains(block, "Not shown, over the block budget; fetch with memory_search: note-h (4096 bytes), note-i (4096 bytes)") {
		t.Errorf("omitted line missing:\n%s", lastLine(block))
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
		if _, err := store.Put(ctx, Entry{Scope: "user", Name: fmt.Sprintf("note-%02d", i), Content: filler(3000, 'n')}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Put(ctx, Entry{Scope: "user", Name: "zz-passport", Content: "In the top drawer.\n"}); err != nil {
		t.Fatal(err)
	}
	block, man, err := Render(ctx, store, []Scope{"user"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(block, "### zz-passport") {
		t.Errorf("the 19 byte entry is not in a block with %d bytes free:\n%s", DefaultMaxTotalBytes-len(block), lastLine(block))
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
		if _, err := store.Put(ctx, Entry{
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
		if _, err := store.Put(ctx, Entry{Scope: "user", Name: fmt.Sprintf("note-%d", i), Content: filler(200, 'x')}); err != nil {
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

// blockLine is the summary's sentence about the block's own size. The
// free count is written at the width of the bound, so that the
// summary's width depends on the size alone and the size it states
// settles; see summary.
func blockLine(size, max int) string {
	return fmt.Sprintf("Block: %d of %d bytes (%*d free).", size, max, len(strconv.Itoa(max)), max-size)
}

func sortedNames(names []string) bool {
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			return false
		}
	}
	return true
}

func lastLine(s string) string {
	return s[strings.LastIndex(s, "\n")+1:]
}

// TestManifestIdentity checks what a product compares to decide
// whether to record: the hash is over what the model was shown and
// what it was not, so an unchanged render hashes the same and any
// difference moves it.
func TestManifestIdentity(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore(WithMaxEntryBytes(256))
	for _, e := range []Entry{
		{Scope: "user", Name: "a", Content: filler(200, 'a')},
		{Scope: "user", Name: "b", Content: filler(200, 'b')},
		{Scope: "user", Name: "c", Content: filler(200, 'c')},
	} {
		if _, err := store.Put(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	scopes := []Scope{"user"}
	opt := WithMaxTotalBytes(600)
	_, first, err := Render(ctx, store, scopes, opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Entries) == 0 || len(first.Omitted) == 0 {
		t.Fatalf("the fixture shows and omits nothing: %+v", first)
	}
	_, again, err := Render(ctx, store, scopes, opt)
	if err != nil {
		t.Fatal(err)
	}
	if first.Hash() != again.Hash() {
		t.Error("two renders of one store hash differently")
	}
	if !strings.HasPrefix(first.Hash(), "sha256:") || len(first.Hash()) != len(Hash("")) {
		t.Errorf("Hash = %q", first.Hash())
	}
	// A write the block shows moves the hash.
	if _, err := store.Put(ctx, Entry{Scope: "user", Name: "a", Content: filler(199, 'z')}); err != nil {
		t.Fatal(err)
	}
	_, edited, err := Render(ctx, store, scopes, opt)
	if err != nil {
		t.Fatal(err)
	}
	if edited.Hash() == first.Hash() {
		t.Error("a changed entry did not move the manifest's hash")
	}
	// So does a change to what was left out, and to the order.
	swapped := Manifest{Entries: []ManifestEntry{first.Entries[0]}, Omitted: first.Omitted}
	reordered := Manifest{Entries: []ManifestEntry{first.Entries[0]}, Omitted: []ManifestEntry{}}
	if len(first.Entries) > 1 {
		reordered.Entries = []ManifestEntry{first.Entries[1], first.Entries[0]}
		reordered.Omitted = first.Omitted
		if swapped.Hash() == reordered.Hash() {
			t.Error("order does not move the manifest's hash")
		}
	}
	dropped := Manifest{Entries: first.Entries}
	if dropped.Hash() == first.Hash() {
		t.Error("what was omitted does not move the manifest's hash")
	}
	reason := Manifest{Entries: first.Entries, Omitted: append([]ManifestEntry(nil), first.Omitted...)}
	reason.Omitted[0].Reason = "something else"
	if reason.Hash() == first.Hash() {
		t.Error("the reason does not move the manifest's hash")
	}
	// Record is the namespace and the bytes a product hands its
	// recorder.
	ns, data := first.Record()
	if ns != ManifestNS || ManifestNS != "agentmemory:render" {
		t.Errorf("Record namespace = %q", ns)
	}
	var back Manifest
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("Record data: %v", err)
	}
	if back.Hash() != first.Hash() {
		t.Errorf("the recorded manifest hashes differently:\n%s", data)
	}
	if len(back.Omitted) == 0 || back.Omitted[0].Reason != OmitBudget || back.Omitted[0].Bytes == 0 || back.Omitted[0].Name == "" {
		t.Errorf("the recorded omissions do not carry scope, name, size and reason: %s", data)
	}
}

// TestRenderSummarySettles is the case the summary's width could not
// settle on: at 243 bytes the block's size and its free count cross a
// power of ten in opposite directions, so a summary whose free count
// shrinks with the size is wider for the smaller size than for the
// larger and states a length the block does not have. The sweep is the
// same question asked at random.
func TestRenderSummarySettles(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore(WithMaxEntryBytes(4096))
	if _, err := store.Put(ctx, Entry{Scope: "user", Name: "a", Content: strings.Repeat("x", 9) + "\n"}); err != nil {
		t.Fatal(err)
	}
	block, _, err := Render(ctx, store, []Scope{"user"}, WithMaxTotalBytes(243))
	if err != nil {
		t.Fatal(err)
	}
	if want := blockLine(len(block), 243); !strings.Contains(block, want) {
		t.Errorf("the block is %d bytes and does not say %q:\n%s", len(block), want, lastLine(block))
	}

	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 20000; i++ {
		max := 120 + rng.Intn(4000)
		st := NewMemStore(WithMaxEntryBytes(4096))
		for j := 0; j < 1+rng.Intn(4); j++ {
			e := Entry{Scope: "user", Name: fmt.Sprintf("e%d", j), Content: strings.Repeat("x", 1+rng.Intn(300))}
			if rng.Intn(2) == 0 {
				e.Meta = map[string]string{"description": strings.Repeat("d", rng.Intn(40))}
			}
			if _, err := st.Put(ctx, e); err != nil {
				t.Fatal(err)
			}
		}
		block, _, err := Render(ctx, st, []Scope{"user"}, WithMaxTotalBytes(max))
		if err != nil {
			t.Fatal(err)
		}
		if len(block) > max {
			t.Fatalf("bound %d: block is %d bytes", max, len(block))
		}
		if want := blockLine(len(block), max); !strings.Contains(block, want) {
			t.Fatalf("bound %d: the block is %d bytes and does not say %q:\n%s", max, len(block), want, lastLine(block))
		}
	}
}

// TestRenderSummaryIsExact sweeps the block's size across the digit
// boundaries where the size it reports and the free count it reports
// move in opposite directions, and holds the summary to the block's
// true length at every one of them. The bound is on the block, so it
// is checked here too.
func TestRenderSummaryIsExact(t *testing.T) {
	ctx := context.Background()
	for _, max := range []int{400, 1000, 2000, 11000, DefaultMaxTotalBytes} {
		for n := 1; n <= max; n += 7 {
			store := NewMemStore(WithMaxEntryBytes(max))
			if _, err := store.Put(ctx, Entry{Scope: "user", Name: "a", Content: filler(n, 'a')}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Put(ctx, Entry{Scope: "user", Name: "b", Content: "b\n"}); err != nil {
				t.Fatal(err)
			}
			block, _, err := Render(ctx, store, []Scope{"user"}, WithMaxTotalBytes(max))
			if err != nil {
				t.Fatal(err)
			}
			if len(block) > max {
				t.Fatalf("max %d, entry %d: block is %d bytes", max, n, len(block))
			}
			want := blockLine(len(block), max)
			if !strings.Contains(block, want) {
				t.Fatalf("max %d, entry %d: summary does not say %q:\n%s", max, n, want, lastLine(block))
			}
		}
	}
}

func TestRenderErrors(t *testing.T) {
	ctx := context.Background()
	if _, _, err := Render(ctx, NewMemStore(), []Scope{"user", "Bad"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("Render with a bad scope = %v", err)
	}
	if _, _, err := RenderParts(ctx, NewMemStore(), []Scope{"user", "user"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("RenderParts with a scope given twice = %v", err)
	}
	if _, _, err := Render(ctx, failing{}, []Scope{"user"}); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("Render over a failing store = %v", err)
	}
}

// TestRenderBudgetRefused is #17: a bound the block cannot meet is
// refused with ErrBudget, never answered with a block over it, and a
// bound of zero or less is not read as the default.
func TestRenderBudgetRefused(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	for i := range 20 {
		if _, err := store.Put(ctx, Entry{Scope: "user", Name: fmt.Sprintf("n-%02d", i), Content: filler(300, 'x')}); err != nil {
			t.Fatal(err)
		}
	}
	scopes := []Scope{"user", "project"}
	for _, max := range []int{0, -100, 1, 80} {
		t.Run(strconv.Itoa(max), func(t *testing.T) {
			block, man, err := Render(ctx, store, scopes, WithMaxTotalBytes(max))
			if !errors.Is(err, ErrBudget) {
				t.Errorf("Render under a %d byte bound = %d bytes, %v; want ErrBudget", max, len(block), err)
			}
			if block != "" || len(man.Entries) != 0 || len(man.Omitted) != 0 {
				t.Errorf("a refused render returned a block or a manifest")
			}
			if _, _, err := RenderParts(ctx, store, scopes, WithMaxTotalBytes(max)); !errors.Is(err, ErrBudget) {
				t.Errorf("RenderParts under a %d byte bound = %v; want ErrBudget", max, err)
			}
		})
	}
	// The smallest bound that renders gives a block inside it, and the
	// byte under it is refused: there is no bound the block exceeds.
	floor := 0
	for max := 1; max <= 1024; max++ {
		block, _, err := Render(ctx, store, scopes, WithMaxTotalBytes(max))
		if errors.Is(err, ErrBudget) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(block) > max {
			t.Fatalf("block is %d bytes, over the %d byte bound", len(block), max)
		}
		floor = max
		break
	}
	if floor == 0 {
		t.Fatal("no bound up to 1024 bytes renders")
	}
	for max := floor; max < floor+64; max++ {
		block, _, err := Render(ctx, store, scopes, WithMaxTotalBytes(max))
		if err != nil || len(block) > max {
			t.Errorf("a %d byte bound, over the %d byte floor: %d bytes, %v", max, floor, len(block), err)
		}
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

// TestRenderPartsKeepThePrefix is the round 3 study's cache question: a
// write to one entry changes that entry's part and the summary, and no
// other part, so the block before the entry is the prefix the last
// request cached. A new entry adds its part and moves the summary.
func TestRenderPartsKeepThePrefix(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	for i := range 8 {
		if _, err := store.Put(ctx, Entry{Scope: "user", Name: fmt.Sprintf("note-%d", i), Content: filler(3000, byte('a'+i))}); err != nil {
			t.Fatal(err)
		}
	}
	scopes := []Scope{"user", "project"}
	before, _, err := RenderParts(ctx, store, scopes)
	if err != nil {
		t.Fatal(err)
	}
	if before[0].ID != TitlePartID || before[len(before)-1].ID != SummaryPartID || before[1].ID != "memory/user" || before[2].ID != PartID("user", "note-0") {
		t.Errorf("parts begin %s, %s, %s and end %s", before[0].ID, before[1].ID, before[2].ID, before[len(before)-1].ID)
	}
	ids := map[string]bool{}
	for _, p := range before {
		if ids[p.ID] {
			t.Errorf("two parts are %s", p.ID)
		}
		ids[p.ID] = true
		if p.ID != SummaryPartID && (strings.HasPrefix(p.Text, "\n") || strings.HasSuffix(p.Text, "\n")) {
			t.Errorf("part %s begins or ends with a newline, which the separator supplies: %q", p.ID, p.Text)
		}
	}

	if _, err := store.Put(ctx, Entry{Scope: "user", Name: "note-6", Content: "Likes Bristol"}); err != nil {
		t.Fatal(err)
	}
	after, _, err := RenderParts(ctx, store, scopes)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("%d parts before the write, %d after", len(before), len(after))
	}
	var changed []string
	for i := range after {
		if after[i].ID != before[i].ID {
			t.Fatalf("part %d is %s after the write, %s before", i, after[i].ID, before[i].ID)
		}
		if after[i].Text != before[i].Text {
			changed = append(changed, after[i].ID)
		}
	}
	if fmt.Sprint(changed) != "["+PartID("user", "note-6")+" "+SummaryPartID+"]" {
		t.Errorf("the write changed %v", changed)
	}
	b1, b2 := JoinParts(before), JoinParts(after)
	shared := 0
	for shared < len(b1) && shared < len(b2) && b1[shared] == b2[shared] {
		shared++
	}
	if want := strings.Index(b1, "### note-6"); shared < want {
		t.Errorf("the two blocks share %d bytes; the entry the write touched starts at %d", shared, want)
	}
}

// manifestFixture is n entries of scope user, the first shown ones
// shown and the rest omitted for the budget, as a render over a memory
// past its bound lists them.
func manifestFixture(n, shown int) Manifest {
	var m Manifest
	for i := range n {
		e := ManifestEntry{Scope: "user", Name: fmt.Sprintf("fact-%03d", i), Hash: Hash(strconv.Itoa(i)), Bytes: 40 + i}
		if i < shown {
			m.Entries = append(m.Entries, e)
			continue
		}
		e.Reason = OmitBudget
		m.Omitted = append(m.Omitted, e)
	}
	return m
}

// TestManifestRecordSince is the record of a write under a large
// memory: the delta names what moved, and folding it onto the manifest
// before gives back the manifest after, for every kind of move.
func TestManifestRecordSince(t *testing.T) {
	base := manifestFixture(600, 126)
	clone := func(m Manifest) Manifest {
		return Manifest{Entries: append([]ManifestEntry(nil), m.Entries...), Omitted: append([]ManifestEntry(nil), m.Omitted...)}
	}
	cases := []struct {
		name  string
		edit  func(Manifest) Manifest
		delta bool
	}{
		{name: "unchanged", edit: clone, delta: true},
		{name: "patch-shown", delta: true, edit: func(m Manifest) Manifest {
			m = clone(m)
			m.Entries[60].Hash, m.Entries[60].Bytes = Hash("patched"), 43
			return m
		}},
		{name: "patch-omitted", delta: true, edit: func(m Manifest) Manifest {
			m = clone(m)
			m.Omitted[300].Hash = Hash("patched")
			return m
		}},
		{name: "create-at-start", delta: true, edit: func(m Manifest) Manifest {
			m = clone(m)
			m.Entries = append([]ManifestEntry{{Scope: "user", Name: "aaa", Hash: Hash("new"), Bytes: 3}}, m.Entries...)
			return m
		}},
		{name: "forget-last-shown", delta: true, edit: func(m Manifest) Manifest {
			m = clone(m)
			m.Entries = m.Entries[:len(m.Entries)-1]
			return m
		}},
		{name: "shown-to-omitted", delta: true, edit: func(m Manifest) Manifest {
			m = clone(m)
			last := m.Entries[len(m.Entries)-1]
			last.Reason = OmitBudget
			m.Entries = m.Entries[:len(m.Entries)-1]
			m.Omitted = append([]ManifestEntry{last}, m.Omitted...)
			return m
		}},
		{name: "omitted-to-shown", delta: true, edit: func(m Manifest) Manifest {
			m = clone(m)
			first := m.Omitted[0]
			first.Reason = ""
			m.Entries = append(m.Entries, first)
			m.Omitted = m.Omitted[1:]
			return m
		}},
		{name: "swap", delta: true, edit: func(m Manifest) Manifest {
			m = clone(m)
			m.Entries[10], m.Entries[11] = m.Entries[11], m.Entries[10]
			return m
		}},
		{name: "truncated", delta: true, edit: func(Manifest) Manifest { return manifestFixture(3, 3) }},
		// Nothing to keep, so the whole record is the smaller.
		{name: "emptied", delta: false, edit: func(Manifest) Manifest { return Manifest{} }},
		{name: "all-new", delta: false, edit: func(Manifest) Manifest {
			return Manifest{Entries: []ManifestEntry{{Scope: "project", Name: "rule", Hash: Hash("r"), Bytes: 1}}}
		}},
		{name: "name-twice", delta: false, edit: func(m Manifest) Manifest {
			m = clone(m)
			m.Entries = append(m.Entries, m.Entries[0])
			return m
		}},
	}
	whole := len(must(base.Record()))
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			next := tc.edit(base)
			ns, data := next.RecordSince(base)
			if ns != ManifestNS {
				t.Errorf("namespace = %q", ns)
			}
			if got := bytes.Contains(data, []byte(`"base"`)); got != tc.delta {
				t.Fatalf("delta = %v, want %v: %s", got, tc.delta, data)
			}
			if tc.delta && len(data) > whole/100 {
				t.Errorf("the delta is %d bytes, the whole record %d: %s", len(data), whole, data)
			}
			got, err := ApplyManifestRecord(base, data)
			if err != nil {
				t.Fatalf("ApplyManifestRecord: %v\n%s", err, data)
			}
			if got.Hash() != next.Hash() {
				t.Errorf("the folded manifest hashes differently:\n%s", data)
			}
		})
	}
	// The case the delta is for: a patch under 126 shown and 474
	// omitted entries is one entry, where the whole record is every one.
	patched := cases[1].edit(base)
	_, data := patched.RecordSince(base)
	t.Logf("one patch: whole record %d bytes, delta %d: %s", whole, len(data), data)
}

// TestManifestRecordSinceRandom folds a chain of random edits, each
// recorded as a delta on the last, and ends at the last manifest.
func TestManifestRecordSinceRandom(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	edit := func(list []ManifestEntry) []ManifestEntry {
		out := append([]ManifestEntry(nil), list...)
		switch op := r.Intn(4); {
		case op == 0 && len(out) > 0:
			i := r.Intn(len(out))
			out = append(out[:i], out[i+1:]...)
		case op == 1 && len(out) > 0:
			out[r.Intn(len(out))].Hash = Hash(strconv.Itoa(r.Int()))
		case op == 2 && len(out) > 1:
			i := r.Intn(len(out) - 1)
			out[i], out[i+1] = out[i+1], out[i]
		default:
			i := r.Intn(len(out) + 1)
			e := ManifestEntry{Scope: "user", Name: fmt.Sprintf("new-%d", r.Int()), Hash: Hash("x"), Bytes: 1}
			out = append(out[:i], append([]ManifestEntry{e}, out[i:]...)...)
		}
		return out
	}
	have := manifestFixture(40, 15)
	recorded := have
	for i := range 500 {
		next := Manifest{Entries: have.Entries, Omitted: have.Omitted}
		if r.Intn(2) == 0 {
			next.Entries = edit(have.Entries)
		} else {
			next.Omitted = edit(have.Omitted)
		}
		_, data := next.RecordSince(have)
		folded, err := ApplyManifestRecord(recorded, data)
		if err != nil {
			t.Fatalf("step %d: %v\n%s", i, err, data)
		}
		if folded.Hash() != next.Hash() {
			t.Fatalf("step %d: folded to another manifest:\n%s", i, data)
		}
		have, recorded = next, folded
	}
}

// TestApplyManifestRecordRefuses is a delta a reader cannot fold: on
// another manifest, or malformed.
func TestApplyManifestRecordRefuses(t *testing.T) {
	base := manifestFixture(20, 10)
	next := Manifest{Entries: base.Entries[1:], Omitted: base.Omitted}
	_, delta := next.RecordSince(base)
	if !bytes.Contains(delta, []byte(`"base"`)) {
		t.Fatalf("not a delta: %s", delta)
	}
	if _, err := ApplyManifestRecord(Manifest{}, delta); !errors.Is(err, ErrManifestBase) {
		t.Errorf("a delta on another manifest = %v, want ErrManifestBase", err)
	}
	h := base.Hash()
	cases := []struct {
		name, data, want string
	}{
		{"not-json", `{`, "manifest record"},
		{"keep-past-end", `{"base":"` + h + `","hash":"x","entries":[{"keep":11}]}`, "keeps 11 entries and 10 are left"},
		{"keep-zero", `{"base":"` + h + `","hash":"x","entries":[{"keep":0}]}`, "neither a positive keep nor an entry"},
		{"keep-and-entry", `{"base":"` + h + `","hash":"x","entries":[{"keep":1,"scope":"user","name":"a"}]}`, "both a keep and an entry"},
		{"no-name", `{"base":"` + h + `","hash":"x","omitted":[{"scope":"user","hash":"h"}]}`, "names no entry"},
		{"wrong-hash", `{"base":"` + h + `","hash":"sha256:00","entries":[{"keep":10}],"omitted":[{"keep":10}]}`, "the result hashes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ApplyManifestRecord(base, []byte(tc.data))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to say %q", err, tc.want)
			}
		})
	}
	// A whole record is the manifest it holds, whatever is in force.
	_, whole := next.Record()
	got, err := ApplyManifestRecord(manifestFixture(3, 1), whole)
	if err != nil || got.Hash() != next.Hash() {
		t.Errorf("a whole record folds to %v, %v", got.Hash(), err)
	}
}

func must(_ string, data []byte) []byte { return data }

// TestRenderScopeMaxBytes is a full scope rendered before a small one
// under one bound: without a cap the later scope gets nothing, and
// with one on the full scope it gets its room, the full scope keeps
// inside its cap, and neither moves the block past its bound.
func TestRenderScopeMaxBytes(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	for _, e := range append(scopeEntries("environment", 20, 150), scopeEntries("user", 2, 80)...) {
		if _, err := store.Put(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	scopes := []Scope{"environment", "user"}
	const bound = 3575
	shown := func(man Manifest, scope Scope) int {
		n := 0
		for _, me := range man.Entries {
			if me.Scope == scope {
				n++
			}
		}
		return n
	}
	// scopeBytes is what a scope's parts after its heading add.
	scopeBytes := func(parts []Part, scope Scope) int {
		n := 0
		for _, p := range parts {
			if strings.HasPrefix(p.ID, scopePartID(scope)+"/") || p.ID == omittedPartID(scope) {
				n += cost(p.Text)
			}
		}
		return n
	}
	cases := []struct {
		name      string
		opts      []RenderOption
		env, user int
		envCap    int
	}{
		{name: "uncapped", env: 17, user: 0},
		{name: "env-capped", opts: []RenderOption{WithScopeMaxBytes("environment", 2400)}, env: 12, user: 2, envCap: 2400},
		{name: "both-capped", opts: []RenderOption{WithScopeMaxBytes("environment", 2400), WithScopeMaxBytes("user", 1000)}, env: 12, user: 2, envCap: 2400},
		{name: "env-zero", opts: []RenderOption{WithScopeMaxBytes("environment", 0)}, env: 0, user: 2},
		{name: "last-wins", opts: []RenderOption{WithScopeMaxBytes("environment", 0), WithScopeMaxBytes("environment", 2400)}, env: 12, user: 2, envCap: 2400},
		{name: "cap-over-bound", opts: []RenderOption{WithScopeMaxBytes("environment", 1<<20)}, env: 17, user: 0},
		{name: "unlisted-scope", opts: []RenderOption{WithScopeMaxBytes("project", 0)}, env: 17, user: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := append([]RenderOption{WithMaxTotalBytes(bound)}, tc.opts...)
			parts, man, err := RenderParts(ctx, store, scopes, opts...)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(JoinParts(parts)); got > bound {
				t.Errorf("block is %d bytes, over the %d byte bound", got, bound)
			}
			if e, u := shown(man, "environment"), shown(man, "user"); e != tc.env || u != tc.user {
				t.Errorf("shown: %d environment, %d user; want %d, %d", e, u, tc.env, tc.user)
			}
			if tc.envCap > 0 {
				if n := scopeBytes(parts, "environment"); n > tc.envCap {
					t.Errorf("environment takes %d bytes, over its %d byte cap", n, tc.envCap)
				}
			}
			if len(man.Entries)+len(man.Omitted) != 22 {
				t.Errorf("the manifest names %d entries, want 22", len(man.Entries)+len(man.Omitted))
			}
			for _, me := range man.Omitted {
				if me.Reason != OmitBudget {
					t.Errorf("omitted %s/%s has reason %q", me.Scope, me.Name, me.Reason)
				}
			}
		})
	}
	if _, _, err := Render(ctx, store, scopes, WithScopeMaxBytes("user", -1)); !errors.Is(err, ErrBudget) {
		t.Errorf("a cap under zero = %v, want ErrBudget", err)
	}
}

// TestOmitBlock is a product that had no room for the block on one
// turn: it records a manifest with every entry omitted under OmitBlock,
// which hashes apart from the same entries omitted for budget and from
// the empty manifest, records and folds as any manifest does, and is
// told apart by its reason rather than by an empty one.
func TestOmitBlock(t *testing.T) {
	shown := manifestFixture(5, 3)
	dropped := Manifest{}
	for _, e := range append(slices.Clone(shown.Entries), shown.Omitted...) {
		e.Reason = OmitBlock
		dropped.Omitted = append(dropped.Omitted, e)
	}
	budget := Manifest{Omitted: slices.Clone(dropped.Omitted)}
	for i := range budget.Omitted {
		budget.Omitted[i].Reason = OmitBudget
	}
	tests := []struct {
		name   string
		m      Manifest
		differ Manifest
	}{
		{"dropped block against the empty manifest", dropped, Manifest{}},
		{"dropped block against the same entries over budget", dropped, budget},
		{"dropped block against the block shown", dropped, shown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.m.Hash() == tt.differ.Hash() {
				t.Error("the manifests hash the same")
			}
			_, data := tt.m.RecordSince(tt.differ)
			got, err := ApplyManifestRecord(tt.differ, data)
			if err != nil {
				t.Fatal(err)
			}
			if got.Hash() != tt.m.Hash() || len(got.Entries) != 0 {
				t.Errorf("folded %+v", got)
			}
			for _, e := range got.Omitted {
				if e.Reason != OmitBlock {
					t.Errorf("%s/%s omitted for %q, want %q", e.Scope, e.Name, e.Reason, OmitBlock)
				}
			}
		})
	}
	if OmitBlock == OmitBudget || OmitBlock == "" {
		t.Error("OmitBlock is not its own reason")
	}
}

// TestManifestFoldRecord is a kit restarted into a handoff: it has
// folded the session's path, so the fold holds its own last manifest
// behind the other agent's, and it has kept nothing itself. The record
// it writes is the delta on its own last manifest, not the whole.
func TestManifestFoldRecord(t *testing.T) {
	a := manifestFixture(600, 126)
	b := Manifest{Entries: []ManifestEntry{{Scope: "billing", Name: "plan", Hash: Hash("p"), Bytes: 10}}}
	nextA := Manifest{Entries: slices.Clone(a.Entries), Omitted: a.Omitted}
	nextA.Entries[7].Hash = Hash("patched")
	tests := []struct {
		name     string
		path     [][]byte // the records on the session's path, in order
		m        Manifest
		wantBase string // "" for the whole record
	}{
		{"empty fold, first manifest", nil, a, ""},
		{"own manifest in force", [][]byte{must(a.Record())}, nextA, a.Hash()},
		{"own manifest behind the other agent's", [][]byte{must(a.Record()), must(b.RecordSince(a))}, nextA, a.Hash()},
		{"own manifest behind seven others", append([][]byte{must(a.Record())}, fixtures(7)...), nextA, a.Hash()},
		{"own manifest past the depth", append([][]byte{must(a.Record())}, fixtures(ManifestFoldDepth)...), nextA, ""},
		{"nothing in common with any", [][]byte{must(a.Record()), must(b.RecordSince(a))}, manifestFixture(3, 0), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var f ManifestFold
			for _, data := range tt.path {
				if err := f.Apply(data); err != nil {
					t.Fatal(err)
				}
			}
			ns, data := f.Record(tt.m)
			if ns != ManifestNS {
				t.Errorf("ns = %q", ns)
			}
			var probe struct {
				Base string `json:"base"`
			}
			if err := json.Unmarshal(data, &probe); err != nil {
				t.Fatal(err)
			}
			if probe.Base != tt.wantBase {
				t.Errorf("base = %q, want %q (%d bytes)", probe.Base, tt.wantBase, len(data))
			}
			if tt.wantBase != "" && len(data) >= len(must(tt.m.Record())) {
				t.Errorf("the delta is %d bytes, no smaller than the whole", len(data))
			}
			// Every reader folding the path resolves it, and the fold
			// that wrote it did not move.
			before := f.Manifest().Hash()
			var g ManifestFold
			for _, data := range tt.path {
				if err := g.Apply(data); err != nil {
					t.Fatal(err)
				}
			}
			if err := g.Apply(data); err != nil {
				t.Fatalf("a reader's fold refused the record: %v", err)
			}
			if g.Manifest().Hash() != tt.m.Hash() {
				t.Error("the reader's fold holds another manifest")
			}
			if f.Manifest().Hash() != before {
				t.Error("Record moved the writer's fold")
			}
		})
	}
	// Equal sizes prefer the manifest in force, which a reader with
	// ApplyManifestRecord resolves as well.
	x := Manifest{Entries: []ManifestEntry{{Scope: "s", Name: "a1", Hash: Hash("1"), Bytes: 1}, {Scope: "s", Name: "b", Hash: Hash("b"), Bytes: 1}}}
	y := Manifest{Entries: []ManifestEntry{{Scope: "s", Name: "a2", Hash: Hash("2"), Bytes: 1}, {Scope: "s", Name: "b", Hash: Hash("b"), Bytes: 1}}}
	m := Manifest{Entries: []ManifestEntry{{Scope: "s", Name: "a3", Hash: Hash("3"), Bytes: 1}, {Scope: "s", Name: "b", Hash: Hash("b"), Bytes: 1}}}
	var f ManifestFold
	for _, r := range [][]byte{must(y.Record()), must(x.Record())} {
		if err := f.Apply(r); err != nil {
			t.Fatal(err)
		}
	}
	_, data := f.Record(m)
	if _, err := ApplyManifestRecord(f.Manifest(), data); err != nil {
		t.Errorf("the tie did not go to the manifest in force: %v\n%s", err, data)
	}
}

// fixtures returns n whole records of manifests that share nothing
// with each other or with manifestFixture's defaults.
func fixtures(n int) [][]byte {
	var out [][]byte
	for i := range n {
		out = append(out, must(manifestFixture(3+i, 1).Record()))
	}
	return out
}

// TestManifestFold is two agents with their own memories taking turns
// in one session: each records a delta on its own last manifest, which
// a fold resolves and ApplyManifestRecord does not, and the records
// after the first hand-back stay small.
func TestManifestFold(t *testing.T) {
	a := manifestFixture(600, 126)
	b := Manifest{Entries: []ManifestEntry{{Scope: "billing", Name: "plan", Hash: Hash("p"), Bytes: 10}}}
	var f ManifestFold
	if f.Manifest().Hash() != (Manifest{}).Hash() {
		t.Fatal("a new fold holds a manifest")
	}
	apply := func(data []byte) {
		t.Helper()
		if err := f.Apply(data); err != nil {
			t.Fatalf("Apply: %v\n%s", err, data)
		}
	}
	_, data := a.Record()
	apply(data)
	_, data = b.RecordSince(a) // b on taking over, on the manifest in force
	apply(data)
	whole := len(must(a.Record()))
	// Hand back to a, then to b, ten times: each on its own last manifest.
	lastA, lastB := a, b
	for i := range 10 {
		nextA := Manifest{Entries: slices.Clone(lastA.Entries), Omitted: lastA.Omitted}
		if i%2 == 1 {
			nextA.Entries[i].Hash = Hash(fmt.Sprint("patched", i))
		}
		_, data = nextA.RecordSince(lastA)
		if !bytes.Contains(data, []byte(`"base"`)) || len(data) > whole/100 {
			t.Fatalf("hand-back %d to a records %d bytes, the whole is %d", i, len(data), whole)
		}
		if _, err := ApplyManifestRecord(f.Manifest(), data); !errors.Is(err, ErrManifestBase) {
			t.Errorf("ApplyManifestRecord on the manifest in force = %v, want ErrManifestBase", err)
		}
		apply(data)
		if f.Manifest().Hash() != nextA.Hash() {
			t.Fatalf("hand-back %d: the fold holds another manifest", i)
		}
		_, data = lastB.RecordSince(lastB)
		apply(data)
		if f.Manifest().Hash() != lastB.Hash() {
			t.Fatalf("hand-back %d: the fold does not hold b's manifest", i)
		}
		lastA = nextA
	}
	// A base the fold no longer holds, or never did, is refused, and
	// leaves the fold as it was.
	var g ManifestFold
	_, data = a.Record()
	if err := g.Apply(data); err != nil {
		t.Fatal(err)
	}
	for i := range ManifestFoldDepth {
		_, data = manifestFixture(3+i, 1).Record()
		if err := g.Apply(data); err != nil {
			t.Fatal(err)
		}
	}
	before := g.Manifest().Hash()
	next := Manifest{Entries: a.Entries[1:], Omitted: a.Omitted}
	_, data = next.RecordSince(a)
	if err := g.Apply(data); !errors.Is(err, ErrManifestBase) {
		t.Errorf("a delta on a manifest past the depth = %v, want ErrManifestBase", err)
	}
	if g.Manifest().Hash() != before {
		t.Error("a refused delta moved the fold")
	}
	if err := g.Apply([]byte(`{"base":"` + before + `","hash":"sha256:00"}`)); err == nil || errors.Is(err, ErrManifestBase) {
		t.Errorf("a malformed delta = %v, want a malformed refusal", err)
	}
	// Within the depth it resolves: the oldest of the last eight.
	_, data = manifestFixture(3, 1).Record()
	if err := g.Apply(data); err != nil {
		t.Fatal(err)
	}
	old := manifestFixture(4, 1)
	_, data = Manifest{Entries: old.Entries, Omitted: old.Omitted[1:]}.RecordSince(old)
	if err := g.Apply(data); err != nil {
		t.Errorf("a delta on a manifest within the depth: %v", err)
	}
	// The manifest returned is the fold's copy, not its own.
	m := g.Manifest()
	m.Entries[0].Name = "changed"
	if g.Manifest().Entries[0].Name == "changed" {
		t.Error("Manifest returned the fold's own slice")
	}
}
