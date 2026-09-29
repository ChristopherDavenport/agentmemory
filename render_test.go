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
				if _, err := store.Put(ctx, e); err != nil {
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
