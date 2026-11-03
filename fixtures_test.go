package agentmemory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agenttool"
)

// The fixtures under testdata are the conformance corpus RFC 0001 names
// for an implementation in any language: JSON in, JSON or text out. Each
// is written from the cases in this package with -update and read back
// here, so the corpus is what the reference does, and a test that reads
// a fixture uses nothing but the fixture.

// renderInput is a render fixture's input: everything the block is a
// function of (RFC 0001 section 7.4).
type renderInput struct {
	Scopes     []Scope          `json:"scopes"`
	BlockBound int              `json:"block_bound"`
	EntryBound int              `json:"entry_bound"`
	Caps       map[Scope]int    `json:"caps,omitempty"`
	Entries    []renderInputRow `json:"entries"`
}

type renderInputRow struct {
	Scope   Scope             `json:"scope"`
	Name    string            `json:"name"`
	Content string            `json:"content"`
	Meta    map[string]string `json:"meta,omitempty"`
}

func inputOf(tc renderCase) renderInput {
	in := renderInput{Scopes: tc.scopes, BlockBound: tc.maxTotal, EntryBound: tc.max, Caps: tc.scopeMax, Entries: []renderInputRow{}}
	if in.Scopes == nil {
		in.Scopes = []Scope{}
	}
	if in.BlockBound <= 0 {
		in.BlockBound = DefaultMaxTotalBytes
	}
	if in.EntryBound <= 0 {
		in.EntryBound = DefaultMaxEntryBytes
	}
	for _, e := range tc.entries {
		in.Entries = append(in.Entries, renderInputRow{Scope: e.Scope, Name: e.Name, Content: e.Content, Meta: e.Meta})
	}
	return in
}

// TestRenderFixtureInputs writes and checks each render fixture's input,
// and renders every input under testdata/render from the file alone,
// comparing the block, the parts and the manifest with the fixture.
func TestRenderFixtureInputs(t *testing.T) {
	for _, tc := range renderCases() {
		data, err := json.MarshalIndent(inputOf(tc), "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		checkGolden(t, filepath.Join("testdata", "render", tc.name+".input.json"), append(data, '\n'))
	}
	if *update {
		return
	}
	inputs, err := filepath.Glob(filepath.Join("testdata", "render", "*.input.json"))
	if err != nil || len(inputs) == 0 {
		t.Fatalf("no render inputs: %v", err)
	}
	ctx := context.Background()
	for _, path := range inputs {
		name := strings.TrimSuffix(filepath.Base(path), ".input.json")
		t.Run(name, func(t *testing.T) {
			var in renderInput
			if err := json.Unmarshal(readFixture(t, path), &in); err != nil {
				t.Fatal(err)
			}
			st := NewMemStore(WithMaxEntryBytes(in.EntryBound))
			for _, e := range in.Entries {
				if _, err := st.Put(ctx, Entry{Scope: e.Scope, Name: e.Name, Content: e.Content, Meta: e.Meta}); err != nil {
					t.Fatal(err)
				}
			}
			opts := []RenderOption{WithMaxTotalBytes(in.BlockBound)}
			for s, n := range in.Caps {
				opts = append(opts, WithScopeMaxBytes(s, n))
			}
			parts, man, err := RenderParts(ctx, st, in.Scopes, opts...)
			if err != nil {
				t.Fatal(err)
			}
			base := strings.TrimSuffix(path, ".input.json")
			if got, want := JoinParts(parts), string(readFixture(t, base+".md")); got != want {
				t.Errorf("block differs from %s.md", base)
			}
			var ids strings.Builder
			for _, p := range parts {
				fmt.Fprintf(&ids, "%s %d\n", p.ID, len(p.Text))
			}
			if ids.String() != string(readFixture(t, base+".parts")) {
				t.Errorf("parts differ from %s.parts", base)
			}
			var want Manifest
			if err := json.Unmarshal(readFixture(t, base+".manifest.json"), &want); err != nil {
				t.Fatal(err)
			}
			if man.Hash() != want.Hash() {
				t.Errorf("manifest differs from %s.manifest.json", base)
			}
		})
	}
}

func readFixture(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test . -update)", err)
	}
	return data
}

// lostFixture is a lost-updates fixture: a journal, one line a record
// or damage, and what a reader from each cursor reports.
type lostFixture struct {
	Journal []string   `json:"journal"`
	Reads   []lostRead `json:"reads"`
}

type lostRead struct {
	After   uint64     `json:"after"`
	Lost    []lostItem `json:"lost"`
	Damaged int        `json:"damaged"`
}

type lostItem struct {
	Seq         uint64 `json:"seq"`
	Over        string `json:"over"`
	Fork        bool   `json:"fork,omitempty"`
	Break       bool   `json:"break,omitempty"`
	AfterDamage bool   `json:"after_damage,omitempty"`
}

// lostCases are the journals of lostupdate_test.go, with the cursors
// each is read from.
func lostCases() map[string]struct {
	steps  []journalStep
	afters []uint64
} {
	h := Hash
	return map[string]struct {
		steps  []journalStep
		afters []uint64
	}{
		"past-damage": {[]journalStep{
			rec(1, "a", "one", "", ""),
			damage(2),
			rec(3, "a", "three", h("one"), h("two")),
			rec(4, "b", "bee", "", ""),
			rec(5, "b", "bee2", h("stale"), h("bee")),
		}, []uint64{0, 1, 3}},
		"after-a-cursor": {[]journalStep{
			rec(1, "a", "one", "", ""),
			rec(2, "a", "two", h("one"), h("one")),
			rec(3, "a", "three", h("one"), h("two")),
			rec(4, "a", "four", h("gap"), h("gap")),
			rec(5, "c", "see", h("x"), h("x")),
		}, []uint64{0, 1, 2, 3, 4}},
		// A journal whose first record is not 1 has lost its start, as a
		// compacted one would: an entry's first record there is no break.
		"no-start": {[]journalStep{
			rec(5, "a", "five", h("four"), h("four")),
			rec(6, "a", "six", h("five"), h("five")),
			rec(7, "a", "seven", h("five"), h("six")),
		}, []uint64{0, 5}},
		"no-start-after-damage": {[]journalStep{
			damage(1),
			rec(5, "a", "five", h("four"), h("four")),
			rec(6, "a", "six", h("one"), h("five")),
		}, []uint64{0}},
		"tombstone-and-recreate": {[]journalStep{
			rec(1, "a", "one", "", ""),
			{c: Change{Seq: 2, Entry: Entry{Scope: "user", Name: "a", Content: "one", Hash: h("one"), Deleted: true}, Prev: h("one"), Replaced: h("one")}},
			rec(3, "a", "again", "", ""),
			rec(4, "a", "late", h("one"), ""),
		}, []uint64{0, 2}},
	}
}

// damagedLine is how a fixture writes a record the journal cannot read.
const damagedLine = `{"seq":`

func journalLines(steps []journalStep) []string {
	out := make([]string, 0, len(steps))
	for _, s := range steps {
		if s.err != nil {
			out = append(out, damagedLine)
			continue
		}
		data, _ := json.Marshal(s.c)
		out = append(out, string(data))
	}
	return out
}

// TestLostUpdateFixtures writes the lost-updates fixtures and reads
// every one back, from its journal lines alone.
func TestLostUpdateFixtures(t *testing.T) {
	ctx := context.Background()
	for name, lc := range lostCases() {
		st := journalStore{MemStore: NewMemStore(), steps: lc.steps}
		fx := lostFixture{Journal: journalLines(lc.steps)}
		for _, after := range lc.afters {
			lost, err := LostUpdates(ctx, st, after)
			read := lostRead{After: after, Lost: []lostItem{}, Damaged: len(joined(err))}
			for _, l := range lost {
				read.Lost = append(read.Lost, lostItem{Seq: l.Change.Seq, Over: l.Over, Fork: l.Change.Prev != l.Change.Replaced, Break: l.Break, AfterDamage: l.AfterDamage})
			}
			fx.Reads = append(fx.Reads, read)
		}
		data, err := json.MarshalIndent(fx, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		checkGolden(t, filepath.Join("testdata", "lostupdates", name+".json"), append(data, '\n'))
	}
	if *update {
		return
	}
	paths, _ := filepath.Glob(filepath.Join("testdata", "lostupdates", "*.json"))
	if len(paths) == 0 {
		t.Fatal("no lost-updates fixtures")
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			var fx lostFixture
			if err := json.Unmarshal(readFixture(t, path), &fx); err != nil {
				t.Fatal(err)
			}
			var steps []journalStep
			for i, line := range fx.Journal {
				var c Change
				if err := json.Unmarshal([]byte(line), &c); err != nil || c.Seq == 0 || c.Entry.Hash == "" {
					steps = append(steps, damage(i+1))
					continue
				}
				steps = append(steps, journalStep{c: c})
			}
			st := journalStore{MemStore: NewMemStore(), steps: steps}
			for _, want := range fx.Reads {
				lost, err := LostUpdates(ctx, st, want.After)
				if n := len(joined(err)); n != want.Damaged {
					t.Errorf("after %d: %d damaged, want %d", want.After, n, want.Damaged)
				}
				var got []lostItem
				for _, l := range lost {
					got = append(got, lostItem{Seq: l.Change.Seq, Over: l.Over, Fork: l.Change.Prev != l.Change.Replaced, Break: l.Break, AfterDamage: l.AfterDamage})
				}
				if !slices.Equal(got, want.Lost) && !(len(got) == 0 && len(want.Lost) == 0) {
					t.Errorf("after %d: %+v, want %+v", want.After, got, want.Lost)
				}
			}
		})
	}
}

// joined returns the errors errors.Join joined, or err alone.
func joined(err error) []error {
	if err == nil {
		return nil
	}
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		return j.Unwrap()
	}
	return []error{err}
}

// foldFixture is a manifest fold fixture: the records under
// agentmemory:render on a path, in order, and after each the hash of the
// manifest in force, or the refusal and the hash unchanged.
type foldFixture struct {
	Steps []foldStep `json:"steps"`
}

type foldStep struct {
	Note    string          `json:"note"`
	Record  json.RawMessage `json:"record"`
	Refused string          `json:"refused,omitempty"` // "unresolved" or "malformed"
	InForce string          `json:"in_force"`          // the hash of the manifest in force after it
}

// foldCases builds the records of a path two agents with their own
// memories record into, with the malformed and unresolved records a
// reader must refuse among them.
func foldCases() map[string][]foldStep {
	a := manifestFixture(8, 5)
	b := Manifest{Entries: []ManifestEntry{{Scope: "billing", Name: "plan", Hash: Hash("p"), Bytes: 10}}}
	a2 := Manifest{Entries: slices.Clone(a.Entries), Omitted: slices.Clone(a.Omitted)}
	a2.Entries[2].Hash = Hash("patched")
	a3 := Manifest{Entries: append(slices.Clone(a2.Entries), a2.Omitted[0]), Omitted: slices.Clone(a2.Omitted[1:])}
	a3.Entries[5].Reason = ""
	dropped := Manifest{}
	for _, e := range append(slices.Clone(a3.Entries), a3.Omitted...) {
		e.Reason = OmitBlock
		dropped.Omitted = append(dropped.Omitted, e)
	}
	rec := func(_ string, d []byte) json.RawMessage { return d }
	raw := func(s string) json.RawMessage { return json.RawMessage(s) }
	empty := (Manifest{}).Hash()
	return map[string][]foldStep{
		"two-agents": {
			{Note: "a's first manifest, whole", Record: rec(a.Record())},
			{Note: "b takes over: a delta on a would repeat nothing, so whole", Record: rec(b.RecordSince(a))},
			{Note: "a patches an entry: a delta on a's own last manifest", Record: rec(a2.RecordSince(a))},
			{Note: "a's budget moves one entry into the block", Record: rec(a3.RecordSince(a2))},
			{Note: "a drops the block whole", Record: rec(dropped.RecordSince(a3))},
			{Note: "back to a3 on the dropped manifest", Record: rec(a3.RecordSince(dropped))},
		},
		"refusals": {
			{Note: "a delta on the empty manifest before anything is folded", Record: raw(`{"base":"` + empty + `","hash":"` + b.Hash() + `","entries":[` + jsonOf(b.Entries[0]) + `]}`)},
			{Note: "a delta on a base no reader holds", Record: raw(`{"base":"sha256:0000000000000000000000000000000000000000000000000000000000000000","hash":"` + empty + `"}`)},
			{Note: "a keep past the end of the base list", Record: raw(`{"base":"` + b.Hash() + `","hash":"` + b.Hash() + `","entries":[{"keep":2}]}`)},
			{Note: "an element both a keep and an entry", Record: raw(`{"base":"` + b.Hash() + `","hash":"` + b.Hash() + `","entries":[{"keep":1,"scope":"billing","name":"plan","hash":"` + Hash("p") + `","bytes":10}]}`)},
			{Note: "an element neither a keep nor an entry", Record: raw(`{"base":"` + b.Hash() + `","hash":"` + b.Hash() + `","entries":[{}]}`)},
			{Note: "a delta whose result does not hash to its hash", Record: raw(`{"base":"` + b.Hash() + `","hash":"` + empty + `","entries":[{"keep":1}]}`)},
			{Note: "a base of null is no base: the record is whole", Record: raw(`{"base":null,"entries":[]}`)},
			{Note: "entries null reads as empty", Record: raw(`{"entries":null,"omitted":[{"scope":"user","name":"a","hash":"` + Hash("a") + `","bytes":1,"reason":"block"}]}`)},
			{Note: "an unknown reason is kept", Record: raw(`{"entries":[],"omitted":[{"scope":"user","name":"a","hash":"` + Hash("a") + `","bytes":1,"reason":"policy"}]}`)},
		},
	}
}

// TestManifestFoldFixtures writes the fold fixtures and folds every one
// back from its records alone.
func TestManifestFoldFixtures(t *testing.T) {
	fold := func(steps []foldStep) []foldStep {
		var f ManifestFold
		out := make([]foldStep, 0, len(steps))
		for _, s := range steps {
			err := f.Apply(s.Record)
			s.Refused = ""
			switch {
			case errors.Is(err, ErrManifestBase):
				s.Refused = "unresolved"
			case err != nil:
				s.Refused = "malformed"
			}
			s.InForce = f.Manifest().Hash()
			out = append(out, s)
		}
		return out
	}
	for name, steps := range foldCases() {
		data, err := json.MarshalIndent(foldFixture{Steps: fold(steps)}, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		checkGolden(t, filepath.Join("testdata", "manifest", name+".json"), append(data, '\n'))
	}
	if *update {
		return
	}
	paths, _ := filepath.Glob(filepath.Join("testdata", "manifest", "*.json"))
	if len(paths) == 0 {
		t.Fatal("no fold fixtures")
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			var fx foldFixture
			if err := json.Unmarshal(readFixture(t, path), &fx); err != nil {
				t.Fatal(err)
			}
			got := fold(fx.Steps)
			for i := range got {
				if got[i].Refused != fx.Steps[i].Refused || got[i].InForce != fx.Steps[i].InForce {
					t.Errorf("step %d (%s): refused %q in force %s, want %q %s", i, fx.Steps[i].Note, got[i].Refused, got[i].InForce, fx.Steps[i].Refused, fx.Steps[i].InForce)
				}
			}
		})
	}
	// The refusals fixture holds both kinds and a whole record of each
	// leniency, so a reader that accepts or refuses too much fails it.
	data := readFixture(t, filepath.Join("testdata", "manifest", "refusals.json"))
	for _, want := range []string{`"refused": "unresolved"`, `"refused": "malformed"`} {
		if !bytes.Contains(data, []byte(want)) {
			t.Errorf("refusals.json has no %s", want)
		}
	}
}

func jsonOf(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(data)
}

// trustFixture is a render input with trust labels per scope and the
// trust each part carries.
type trustFixture struct {
	Input renderInput                 `json:"input"`
	Trust map[Scope][]agenttool.Label `json:"trust"`
	Parts []struct {
		ID    string `json:"id"`
		Trust *Trust `json:"trust"`
	} `json:"parts"`
}

func renderTrust(t *testing.T, in renderInput, trust map[Scope][]agenttool.Label) []Part {
	t.Helper()
	ctx := context.Background()
	st := NewMemStore(WithMaxEntryBytes(in.EntryBound))
	for _, e := range in.Entries {
		if _, err := st.Put(ctx, Entry{Scope: e.Scope, Name: e.Name, Content: e.Content, Meta: e.Meta}); err != nil {
			t.Fatal(err)
		}
	}
	opts := []RenderOption{WithMaxTotalBytes(in.BlockBound)}
	for s, n := range in.Caps {
		opts = append(opts, WithScopeMaxBytes(s, n))
	}
	for s, labels := range trust {
		opts = append(opts, WithScopeTrust(s, labels...))
	}
	parts, _, err := RenderParts(ctx, st, in.Scopes, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return parts
}

// TestTrustFixture writes the trust fixture and checks it from the file.
func TestTrustFixture(t *testing.T) {
	in := renderInput{Scopes: []Scope{"rules", "user", "inbox"}, BlockBound: 600, EntryBound: 256, Entries: []renderInputRow{
		{Scope: "rules", Name: "style", Content: "Use Go."},
		{Scope: "user", Name: "a", Content: filler(200, 'a')},
		{Scope: "user", Name: "b", Content: filler(200, 'b')},
		{Scope: "inbox", Name: "mail", Content: "From the inbox."},
	}}
	trust := map[Scope][]agenttool.Label{"rules": {}, "inbox": {agenttool.LabelUntrusted, agenttool.LabelPrivate}}
	path := filepath.Join("testdata", "render", "trust.json")
	if *update {
		fx := trustFixture{Input: in, Trust: trust}
		for _, p := range renderTrust(t, in, trust) {
			fx.Parts = append(fx.Parts, struct {
				ID    string `json:"id"`
				Trust *Trust `json:"trust"`
			}{p.ID, p.Trust})
		}
		data, err := json.MarshalIndent(fx, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		checkGolden(t, path, append(data, '\n'))
		return
	}
	var fx trustFixture
	if err := json.Unmarshal(readFixture(t, path), &fx); err != nil {
		t.Fatal(err)
	}
	got := renderTrust(t, fx.Input, fx.Trust)
	if len(got) != len(fx.Parts) {
		t.Fatalf("%d parts, the fixture has %d", len(got), len(fx.Parts))
	}
	for i, p := range got {
		if p.ID != fx.Parts[i].ID || jsonOf(p.Trust) != jsonOf(fx.Parts[i].Trust) {
			t.Errorf("part %d: %s %s, want %s %s", i, p.ID, jsonOf(p.Trust), fx.Parts[i].ID, jsonOf(fx.Parts[i].Trust))
		}
	}
}

// searchFixture is the reference match over a set of entries.
type searchFixture struct {
	Entries []searchEntry `json:"entries"`
	Cases   []searchCase  `json:"cases"`
}

type searchEntry struct {
	renderInputRow
	Updated time.Time `json:"updated"`
}

type searchCase struct {
	Note          string            `json:"note"`
	Scopes        []Scope           `json:"scopes"`
	Query         string            `json:"query"`
	Limit         int               `json:"limit,omitempty"`
	Any           bool              `json:"any,omitempty"`
	Meta          map[string]string `json:"meta,omitempty"`
	UpdatedAfter  *time.Time        `json:"updated_after,omitempty"`
	UpdatedBefore *time.Time        `json:"updated_before,omitempty"`
	Want          []string          `json:"want"`
}

func runSearch(t *testing.T, fx searchFixture, c searchCase) []string {
	t.Helper()
	ctx := context.Background()
	i := 0
	st := NewMemStore(WithClock(func() time.Time { at := fx.Entries[i].Updated; i++; return at }))
	for _, e := range fx.Entries {
		if _, err := st.Put(ctx, Entry{Scope: e.Scope, Name: e.Name, Content: e.Content, Meta: e.Meta}); err != nil {
			t.Fatal(err)
		}
	}
	opts := SearchOptions{Any: c.Any, Meta: c.Meta}
	if c.UpdatedAfter != nil {
		opts.UpdatedAfter = *c.UpdatedAfter
	}
	if c.UpdatedBefore != nil {
		opts.UpdatedBefore = *c.UpdatedBefore
	}
	found, err := SearchWith(ctx, st, c.Scopes, c.Query, c.Limit, opts)
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, e := range found {
		out = append(out, string(e.Scope)+"/"+e.Name)
	}
	return out
}

// TestSearchFixture writes the reference search's fixture and checks it
// from the file: what matches and in what order, with each option.
func TestSearchFixture(t *testing.T) {
	t0 := time.Date(2026, 11, 2, 19, 44, 0, 0, time.UTC)
	at := func(s int) *time.Time { v := t0.Add(time.Duration(s) * time.Second); return &v }
	row := func(scope Scope, name, content string, meta map[string]string, s int) searchEntry {
		return searchEntry{renderInputRow{Scope: scope, Name: name, Content: content, Meta: meta}, *at(s)}
	}
	fx := searchFixture{
		Entries: []searchEntry{
			row("user", "editor", "Uses Neovim with a dark theme.", map[string]string{"description": "Editor preference", "type": "tool"}, 0),
			row("user", "language", "Prefers Go over Python for tools.", map[string]string{"description": "Language preference", "type": "taste"}, 1),
			row("user", "timezone", "Europe/London", nil, 2),
			row("user", "greek", "ΣΊΣΥΦΟΣ and İstanbul", nil, 3),
			row("project", "build", "Run make check before a commit; the Go toolchain is 1.25.", map[string]string{"type": "tool"}, 4),
		},
		Cases: []searchCase{
			{Note: "one word", Scopes: []Scope{"user"}, Query: "neovim"},
			{Note: "case and white space", Scopes: []Scope{"user"}, Query: " NEOVIM　dark "},
			{Note: "every word", Scopes: []Scope{"user", "project"}, Query: "go python"},
			{Note: "scope order then name", Scopes: []Scope{"project", "user"}, Query: "go"},
			{Note: "a metadata value", Scopes: []Scope{"user"}, Query: "preference"},
			{Note: "a metadata key is not searched", Scopes: []Scope{"user"}, Query: "description"},
			{Note: "the name", Scopes: []Scope{"user"}, Query: "timezone"},
			{Note: "simple lowercase mapping: Σ to σ, İ to i", Scopes: []Scope{"user"}, Query: "σίσυφοσ istanbul"},
			{Note: "the full mapping's i and combining dot is not the simple one", Scopes: []Scope{"user"}, Query: "i\u0307stanbul"},
			{Note: "the empty query", Scopes: []Scope{"user"}, Query: "", Limit: 2},
			{Note: "any, ranked by words held", Scopes: []Scope{"user", "project"}, Query: "python go", Any: true},
			{Note: "meta", Scopes: []Scope{"user", "project"}, Query: "", Meta: map[string]string{"type": "tool"}},
			{Note: "updated after, exclusive", Scopes: []Scope{"user", "project"}, Query: "", UpdatedAfter: at(2)},
			{Note: "updated before, exclusive", Scopes: []Scope{"user", "project"}, Query: "", UpdatedBefore: at(2)},
		},
	}
	path := filepath.Join("testdata", "search", "reference.json")
	if *update {
		for i, c := range fx.Cases {
			fx.Cases[i].Want = runSearch(t, fx, c)
		}
		data, err := json.MarshalIndent(fx, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		checkGolden(t, path, append(data, '\n'))
		return
	}
	var read searchFixture
	if err := json.Unmarshal(readFixture(t, path), &read); err != nil {
		t.Fatal(err)
	}
	for _, c := range read.Cases {
		if got := runSearch(t, read, c); !slices.Equal(got, c.Want) {
			t.Errorf("%s: %v, want %v", c.Note, got, c.Want)
		}
	}
}

// hashFixture pins the content and metadata hashes.
type hashFixture struct {
	Content []struct {
		Content string `json:"content"`
		Hash    string `json:"hash"`
	} `json:"content"`
	Meta []struct {
		Meta     map[string]string `json:"meta"`
		MetaHash string            `json:"meta_hash"`
	} `json:"meta"`
}

// TestHashFixture writes the hash fixture and checks it from the file.
func TestHashFixture(t *testing.T) {
	path := filepath.Join("testdata", "hashes.json")
	if *update {
		var fx hashFixture
		for _, c := range []string{"", "Short answers.\n", "süß ✓ 日本", "no final newline"} {
			fx.Content = append(fx.Content, struct {
				Content string `json:"content"`
				Hash    string `json:"hash"`
			}{c, Hash(c)})
		}
		for _, m := range []map[string]string{
			nil,
			{"description": "How to answer"},
			{"type": "feedback", "description": "How to answer"},
			{"a": "", "b-2": "x=y", "b": "süß"},
		} {
			fx.Meta = append(fx.Meta, struct {
				Meta     map[string]string `json:"meta"`
				MetaHash string            `json:"meta_hash"`
			}{m, MetaHash(m)})
		}
		data, err := json.MarshalIndent(fx, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		checkGolden(t, path, append(data, '\n'))
		return
	}
	var fx hashFixture
	if err := json.Unmarshal(readFixture(t, path), &fx); err != nil {
		t.Fatal(err)
	}
	for _, c := range fx.Content {
		if got := Hash(c.Content); got != c.Hash {
			t.Errorf("Hash(%q) = %s, want %s", c.Content, got, c.Hash)
		}
	}
	for _, m := range fx.Meta {
		if got := MetaHash(m.Meta); got != m.MetaHash {
			t.Errorf("MetaHash(%v) = %s, want %s", m.Meta, got, m.MetaHash)
		}
	}
}

// droppedFixture is testdata/dropped.json: a store, a tool set over it,
// and for each case the manifest of the block the model is reading, a
// call, and the refusal it gets, none when error is absent. It is
// written by hand, not with -update: it states RFC 0001's rule (6.8)
// rather than recording what the reference does.
type droppedFixture struct {
	Scopes     []Scope          `json:"scopes"`
	ReadScopes []Scope          `json:"read_scopes"`
	Insert     []Scope          `json:"insert"`
	Entries    []renderInputRow `json:"entries"`
	Cases      []struct {
		Name     string          `json:"name"`
		Manifest Manifest        `json:"manifest"`
		Tool     string          `json:"tool"`
		Args     json.RawMessage `json:"args"`
		Error    string          `json:"error"`
	} `json:"cases"`
}

// TestDroppedFixture checks that a write in a turn whose block was left
// out is refused where the call is decided, by CheckWrite, and again
// when it runs, before anything else is checked and with nothing
// written; and that every other call goes through.
func TestDroppedFixture(t *testing.T) {
	var fx droppedFixture
	dec := json.NewDecoder(bytes.NewReader(readFixture(t, filepath.Join("testdata", "dropped.json"))))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&fx); err != nil {
		t.Fatal(err)
	}
	for _, c := range fx.Cases {
		t.Run(c.Name, func(t *testing.T) {
			ctx := context.Background()
			store := NewMemStore()
			for _, e := range fx.Entries {
				if _, err := store.Put(ctx, Entry{Scope: e.Scope, Name: e.Name, Content: e.Content, Meta: e.Meta}); err != nil {
					t.Fatal(err)
				}
			}
			got := ""
			if err := CheckWrite(c.Manifest, c.Tool); err != nil {
				got = err.Error()
			}
			if got != c.Error {
				t.Errorf("CheckWrite = %q, want %q", got, c.Error)
			}
			man := c.Manifest
			tools := Tools(store, fx.Scopes, WithReadScopes(fx.ReadScopes...), WithInsert(fx.Insert...), WithRendered(func() Manifest { return man }))
			if !slices.ContainsFunc(tools, func(tool agenttool.Tool) bool { return tool.Name() == c.Tool }) {
				return
			}
			before := journalLen(store)
			_, err := call(t, tools, c.Tool, string(c.Args))
			got = ""
			if err != nil {
				got = err.Error()
			}
			if got != c.Error {
				t.Errorf("%s ran with error %q, want %q", c.Tool, got, c.Error)
			}
			if c.Error != "" && journalLen(store) != before {
				t.Errorf("%s wrote %d records though refused", c.Tool, journalLen(store)-before)
			}
		})
	}
}
