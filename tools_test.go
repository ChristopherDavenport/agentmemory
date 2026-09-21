package agentmemory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/ChristopherDavenport/agenttool"
)

// call runs the named tool with raw JSON arguments and returns the text
// the model would see, or the error.
func call(t *testing.T, tools []agenttool.Tool, name, args string) (string, error) {
	t.Helper()
	for _, tool := range tools {
		if tool.Name() != name {
			continue
		}
		res, err := tool.Execute(context.Background(), agenttool.Call{ID: "call_1", Args: json.RawMessage(args)})
		if err != nil {
			return "", err
		}
		return res.Output.String(), nil
	}
	t.Fatalf("no tool %q", name)
	return "", nil
}

func TestToolsShape(t *testing.T) {
	store := NewMemStore()
	tools := Tools(store, []Scope{"user", "project"})
	if err := agenttool.Set(tools).Validate(); err != nil {
		t.Fatal(err)
	}
	want := []string{SaveTool, PatchTool, ForgetTool, SearchTool}
	for i, tool := range tools {
		if tool.Name() != want[i] {
			t.Errorf("tool %d = %s, want %s", i, tool.Name(), want[i])
		}
		if !strings.Contains(tool.Description(), "Scopes: user, project; name one on every call.") {
			t.Errorf("%s description does not name the scopes: %s", tool.Name(), tool.Description())
		}
		if agenttool.IsStrict(tool) {
			t.Errorf("%s is strict; meta is a map", tool.Name())
		}
	}
	// The schema names the arguments the plan gives each tool, and the
	// scopes the product allows, so the model is steered by what it is
	// sent and not by prose alone.
	var schema struct {
		Required   []string `json:"required"`
		Properties map[string]struct {
			Enum  []string `json:"enum"`
			Items struct {
				Enum []string `json:"enum"`
			} `json:"items"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(tools[0].Parameters(), &schema); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(schema.Required) != "[scope name content]" || len(schema.Properties) != 4 {
		t.Errorf("memory_save schema: required %v, %d properties", schema.Required, len(schema.Properties))
	}
	if fmt.Sprint(schema.Properties["scope"].Enum) != "[user project]" {
		t.Errorf("memory_save schema: scope enum %v", schema.Properties["scope"].Enum)
	}
	for _, tool := range tools {
		if err := json.Unmarshal(tool.Parameters(), &schema); err != nil {
			t.Fatal(err)
		}
		if tool.Name() == SearchTool {
			if fmt.Sprint(schema.Properties["scopes"].Items.Enum) != "[user project]" {
				t.Errorf("%s schema: scopes items enum %v", tool.Name(), schema.Properties["scopes"].Items.Enum)
			}
			continue
		}
		if fmt.Sprint(schema.Properties["scope"].Enum) != "[user project]" || schema.Required[0] != "scope" {
			t.Errorf("%s schema: scope enum %v, required %v", tool.Name(), schema.Properties["scope"].Enum, schema.Required)
		}
	}
	// With one scope there is nothing to choose, so the argument stays
	// optional and the enum still says what it may be.
	one := Tools(store, []Scope{"user"})
	if err := agenttool.Set(one).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(one[0].Parameters(), &schema); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(schema.Required) != "[name content]" || fmt.Sprint(schema.Properties["scope"].Enum) != "[user]" {
		t.Errorf("one-scope schema: required %v, scope enum %v", schema.Required, schema.Properties["scope"].Enum)
	}
	if !strings.Contains(one[0].Description(), "Scope: user, the only one; the argument may be left out.") {
		t.Errorf("one-scope description: %s", one[0].Description())
	}
	if !strings.Contains(tools[0].Description(), "at most 4096 bytes") {
		t.Errorf("memory_save description does not name the bound: %s", tools[0].Description())
	}
	if !strings.Contains(tools[3].Description(), "up to 5 entries") || !strings.Contains(tools[3].Description(), "16384 bytes") {
		t.Errorf("memory_search description does not name its defaults: %s", tools[3].Description())
	}
}

func TestToolsPanics(t *testing.T) {
	for name, scopes := range map[string][]Scope{"none": nil, "bad": {"User"}} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("Tools did not panic")
				}
			}()
			Tools(NewMemStore(), scopes)
		})
	}
}

func TestToolsFailures(t *testing.T) {
	store := NewMemStore(WithMaxEntryBytes(256))
	tools := Tools(store, []Scope{"user", "project"}, WithSearchLimit(2), WithSearchBytes(300))
	ctx := context.Background()
	if _, err := store.Put(ctx, Entry{Scope: "user", Name: "style", Content: "Short answers. Short code.", Meta: map[string]string{"description": "How to answer"}}); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		tool string
		args string
		want string // a substring of the error the model sees
	}{
		{"save: other scope", SaveTool, `{"scope":"secret","name":"a","content":"x"}`, `scope "secret" is not available; Scopes: user, project; name one on every call.`},
		{"save: no scope", SaveTool, `{"name":"a","content":"x"}`, `scope is required; name one of: user, project`},
		{"save: bad name", SaveTool, `{"scope":"user","name":"Not Kebab","content":"x"}`, `name "Not Kebab" is not kebab-case`},
		{"save: empty content", SaveTool, `{"scope":"user","name":"a","content":""}`, `has no content`},
		{"save: over the bound, new", SaveTool, `{"scope":"user","name":"big","content":"` + strings.Repeat("x", 257) + `"}`, `entry user/big is 257 bytes, over the 256 byte limit; split it or trim it`},
		{"save: over the bound, existing", SaveTool, `{"scope":"user","name":"style","content":"` + strings.Repeat("x", 300) + `"}`, `entry user/style is 300 bytes, over the 256 byte limit; it holds 26 bytes now`},
		{"save: bad meta", SaveTool, `{"scope":"user","name":"a","content":"x","meta":{"Type":"y"}}`, `meta key "Type" is not kebab-case`},
		{"save: missing content", SaveTool, `{"scope":"user","name":"a"}`, `content`},
		{"patch: missing entry", PatchTool, `{"scope":"user","name":"nope","old_text":"a","new_text":"b"}`, `no such entry: user/nope; memory_save creates an entry`},
		{"patch: absent text", PatchTool, `{"scope":"user","name":"style","old_text":"Long","new_text":"b"}`, `old_text does not appear in user/style`},
		{"patch: repeated text", PatchTool, `{"scope":"user","name":"style","old_text":"Short","new_text":"Long"}`, `old_text appears 2 times in user/style; include more of the surrounding text`},
		{"patch: empty old", PatchTool, `{"scope":"user","name":"style","old_text":"","new_text":"b"}`, `old_text is empty`},
		{"patch: to empty", PatchTool, `{"scope":"user","name":"style","old_text":"Short answers. Short code.","new_text":""}`, `has no content; use forget`},
		{"patch: other scope", PatchTool, `{"scope":"secret","name":"style","old_text":"a","new_text":"b"}`, `scope "secret" is not available`},
		{"patch: no scope", PatchTool, `{"name":"style","old_text":"a","new_text":"b"}`, `scope is required; name one of: user, project`},
		{"forget: missing", ForgetTool, `{"scope":"user","name":"nope"}`, `no such entry: user/nope`},
		{"forget: other scope", ForgetTool, `{"scope":"secret","name":"style"}`, `scope "secret" is not available`},
		{"forget: no scope", ForgetTool, `{"name":"style"}`, `scope is required; name one of: user, project`},
		{"search: empty query", SearchTool, `{"query":"  "}`, `query is empty`},
		{"search: other scope", SearchTool, `{"query":"x","scopes":["user","secret"]}`, `scope "secret" is not available`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := call(t, tools, tt.tool, tt.args)
			if err == nil {
				t.Fatalf("succeeded with %q, want an error containing %q", out, tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to contain %q", err, tt.want)
			}
		})
	}
	// Nothing above changed the store.
	if es, _ := store.List(ctx, "user"); len(es) != 1 || es[0].Content != "Short answers. Short code." {
		t.Errorf("store after failures = %+v", es)
	}
	if n := journalLen(store); n != 1 {
		t.Errorf("journal has %d records after failures, want 1", n)
	}
}

func TestToolsHappyPath(t *testing.T) {
	store := NewMemStore()
	tools := Tools(store, []Scope{"user", "project"}, WithSearchLimit(2), WithSearchBytes(64))
	ctx := WithSession(context.Background(), "sess-1")
	_ = ctx

	out, err := call(t, tools, SaveTool, `{"scope":"user","name":"style","content":"Short answers.\n","meta":{"description":"How to answer","type":"feedback"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if want := "Saved user/style (15 of 4096 bytes) " + Hash("Short answers.\n") + " — created; meta set (description, type)"; out != want {
		t.Errorf("save = %q, want %q", out, want)
	}
	if _, err := call(t, tools, SaveTool, `{"scope":"project","name":"build","content":"Run make check.\n"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, tools, SaveTool, `{"scope":"user","name":"long","content":"`+strings.Repeat("Prefers Go. ", 8)+`"}`); err != nil {
		t.Fatal(err)
	}

	out, err = call(t, tools, PatchTool, `{"scope":"user","name":"style","old_text":"Short","new_text":"Long"}`)
	if err != nil {
		t.Fatal(err)
	}
	if want := "Patched user/style (14 of 4096 bytes) " + Hash("Long answers.\n"); out != want {
		t.Errorf("patch = %q, want %q", out, want)
	}
	got, _ := store.Get(context.Background(), "user", "style")
	if got.Content != "Long answers.\n" || got.Meta["type"] != "feedback" {
		t.Errorf("entry after patch = %+v; meta must survive a patch", got)
	}
	// A patch that removes text.
	if _, err := call(t, tools, PatchTool, `{"scope":"project","name":"build","old_text":" check","new_text":""}`); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.Get(context.Background(), "project", "build"); got.Content != "Run make.\n" {
		t.Errorf("entry after removing patch = %q", got.Content)
	}

	out, err = call(t, tools, SearchTool, `{"query":"answers"}`)
	if err != nil {
		t.Fatal(err)
	}
	if want := "1 match in user, project for \"answers\".\n\nuser/style (14 bytes): How to answer\nLong answers.\n"; out != want {
		t.Errorf("search = %q, want %q", out, want)
	}
	// The limit caps the count and the byte bound caps the content.
	if _, err := call(t, tools, SaveTool, `{"scope":"user","name":"another","content":"Go go go."}`); err != nil {
		t.Fatal(err)
	}
	out, err = call(t, tools, SearchTool, `{"query":"GO","scopes":["user"]}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "2 matches in user for \"GO\".\n") || !strings.Contains(out, "\nuser/another (9 bytes)\nGo go go.\n") ||
		!strings.Contains(out, "Not shown, over the 64 byte result limit; search for them by name: user/long (96 bytes)") {
		t.Errorf("search with caps = %q", out)
	}
	out, err = call(t, tools, SearchTool, `{"query":"go","limit":1}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "1 match in user, project for \"go\".\n") {
		t.Errorf("search with limit = %q", out)
	}
	out, err = call(t, tools, SearchTool, `{"query":"rust"}`)
	if err != nil || out != "No entries in user, project match \"rust\"." {
		t.Errorf("search with no match = %q, %v", out, err)
	}

	out, err = call(t, tools, ForgetTool, `{"scope":"project","name":"build"}`)
	if err != nil || out != "Forgot project/build" {
		t.Errorf("forget = %q, %v", out, err)
	}
	if _, err := store.Get(context.Background(), "project", "build"); !errors.Is(err, ErrNotFound) {
		t.Errorf("entry after forget: %v", err)
	}
}

// TestSaveMeta covers what a save does to an entry's metadata: a call
// that leaves meta out keeps what is there, because the model that
// rewrites the content is not saying to drop the description; an
// explicit empty object is how it clears it; and the result says which
// happened.
func TestSaveMeta(t *testing.T) {
	store := NewMemStore()
	tools := Tools(store, []Scope{"user"})
	ctx := context.Background()
	steps := []struct {
		name string
		args string
		out  string
		meta map[string]string
	}{
		{"create with meta", `{"name":"style","content":"a","meta":{"description":"How the user likes answers"}}`,
			"created; meta set (description)", map[string]string{"description": "How the user likes answers"}},
		{"replace without meta keeps it", `{"name":"style","content":"bb"}`,
			"replaced 1 bytes; meta kept (description)", map[string]string{"description": "How the user likes answers"}},
		{"replace with meta replaces it", `{"name":"style","content":"ccc","meta":{"type":"feedback"}}`,
			"replaced 2 bytes; meta replaced (type)", map[string]string{"type": "feedback"}},
		{"an empty object clears it", `{"name":"style","content":"dddd","meta":{}}`,
			"replaced 3 bytes; meta cleared", nil},
		{"and then there is none to keep", `{"name":"style","content":"eeeee"}`,
			"replaced 4 bytes; no meta", nil},
		{"create without meta", `{"name":"bare","content":"x"}`, "created; no meta", nil},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			out, err := call(t, tools, SaveTool, step.args)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasSuffix(out, " — "+step.out) {
				t.Errorf("save = %q, want it to end with %q", out, step.out)
			}
			name := "style"
			if strings.Contains(step.args, `"bare"`) {
				name = "bare"
			}
			got, err := store.Get(ctx, "user", name)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Meta) != len(step.meta) {
				t.Fatalf("meta = %v, want %v", got.Meta, step.meta)
			}
			for k, v := range step.meta {
				if got.Meta[k] != v {
					t.Errorf("meta[%s] = %q, want %q", k, got.Meta[k], v)
				}
			}
		})
	}
	// A save is anchored in what it read, so the journal shows a write
	// that landed on something else.
	if lost, err := LostUpdates(ctx, store, 0); err != nil || len(lost) != 0 {
		t.Errorf("LostUpdates over saves that were not overtaken = %+v, %v", lost, err)
	}
}

// interposing runs a function between a tool's read and its write,
// which is where another channel's write lands in the race that used
// to leave no trace.
type interposing struct {
	*MemStore
	before func()
}

func (i *interposing) Put(ctx context.Context, e Entry, opts ...PutOption) (*Change, error) {
	if i.before != nil {
		f := i.before
		i.before = nil
		f()
	}
	return i.MemStore.Put(ctx, e, opts...)
}

// TestSaveShowsALostUpdate is the round 2 probe's scenario through the
// tools: two channels save one entry from one state, the later write
// wins whole, and the journal now says which fact went.
func TestSaveShowsALostUpdate(t *testing.T) {
	mem := NewMemStore()
	store := &interposing{MemStore: mem}
	tools := Tools(store, []Scope{"user"})
	ctx := context.Background()
	if _, err := call(t, tools, SaveTool, `{"name":"profile","content":"Chris. Timezone Europe/London."}`); err != nil {
		t.Fatal(err)
	}
	base, err := mem.Get(ctx, "user", "profile")
	if err != nil {
		t.Fatal(err)
	}
	// Telegram writes while Slack's save is between its read and its
	// write.
	store.before = func() {
		if _, err := mem.Put(WithSession(ctx, "telegram"),
			Entry{Scope: "user", Name: "profile", Content: "Chris. Timezone Europe/London. Prefers Go."},
			BasedOn(base.Hash)); err != nil {
			t.Error(err)
		}
	}
	if _, err := call(t, tools, SaveTool, `{"name":"profile","content":"Chris. Timezone Europe/London. Lives in Bristol."}`); err != nil {
		t.Fatal(err)
	}
	if got, _ := mem.Get(ctx, "user", "profile"); strings.Contains(got.Content, "Prefers Go") {
		t.Fatal("the fixture did not overwrite the other channel's write")
	}
	lost, err := LostUpdates(ctx, mem, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(lost) != 1 {
		t.Fatalf("LostUpdates = %+v, want the save that landed on Telegram's write", lost)
	}
	if lost[0].Change.Seq != 3 || lost[0].Change.Prev != base.Hash ||
		lost[0].Over != Hash("Chris. Timezone Europe/London. Prefers Go.") ||
		lost[0].Change.Replaced != lost[0].Over {
		t.Errorf("LostUpdates[0] = %+v", lost[0])
	}
}

// TestPatchRace runs two patches on one entry at once. Each is anchored
// in old_text and written with IfHash, so both land: the second to
// write re-reads and re-applies rather than clobbering the first.
func TestPatchRace(t *testing.T) {
	store := NewMemStore()
	tools := Tools(store, []Scope{"user"})
	ctx := context.Background()
	if _, err := store.Put(ctx, Entry{Scope: "user", Name: "profile", Content: "Chris. Timezone Europe/London. Editor: unknown. Language: unknown."}); err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 20; round++ {
		if _, err := store.Put(ctx, Entry{Scope: "user", Name: "profile", Content: "Chris. Timezone Europe/London. Editor: unknown. Language: unknown."}); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for _, args := range []string{
			`{"name":"profile","old_text":"Editor: unknown","new_text":"Editor: Neovim"}`,
			`{"name":"profile","old_text":"Language: unknown","new_text":"Language: Go"}`,
		} {
			wg.Add(1)
			go func(args string) {
				defer wg.Done()
				if _, err := call(t, tools, PatchTool, args); err != nil {
					errs <- err
				}
			}(args)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("round %d: %v", round, err)
		}
		got, _ := store.Get(ctx, "user", "profile")
		if got.Content != "Chris. Timezone Europe/London. Editor: Neovim. Language: Go." {
			t.Fatalf("round %d: %q", round, got.Content)
		}
	}
	// An edit whose anchor another writer removed fails out loud.
	if _, err := call(t, tools, PatchTool, `{"name":"profile","old_text":"Editor: unknown","new_text":"Editor: Emacs"}`); err == nil || !strings.Contains(err.Error(), "old_text does not appear") {
		t.Errorf("patch with a removed anchor = %v", err)
	}
}

// conflicting is a store whose Put fails with ErrConflict a set number
// of times, to exercise the patch tool's retry bound.
type conflicting struct {
	*MemStore
	fails int
}

func (c *conflicting) Put(ctx context.Context, e Entry, opts ...PutOption) (*Change, error) {
	if c.fails > 0 {
		c.fails--
		return nil, &ConflictError{Scope: e.Scope, Name: e.Name, Want: "sha256:want", Have: "sha256:have"}
	}
	return c.MemStore.Put(ctx, e, opts...)
}

func TestPatchGivesUp(t *testing.T) {
	store := &conflicting{MemStore: NewMemStore(), fails: patchAttempts}
	if _, err := store.MemStore.Put(context.Background(), Entry{Scope: "user", Name: "a", Content: "x y"}); err != nil {
		t.Fatal(err)
	}
	tools := Tools(store, []Scope{"user"})
	_, err := call(t, tools, PatchTool, `{"name":"a","old_text":"x","new_text":"z"}`)
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "after 4 attempts; another session keeps changing it") {
		t.Errorf("patch under constant conflict = %v", err)
	}
	store.fails = patchAttempts - 1
	if out, err := call(t, tools, PatchTool, `{"name":"a","old_text":"x","new_text":"z"}`); err != nil || !strings.HasPrefix(out, "Patched user/a") {
		t.Errorf("patch under a passing conflict = %q, %v", out, err)
	}
}

// TestWriteRecordDetails checks the seam a recorder reads: every write
// carries the journal record it produced as Recordable details, under
// one namespace, and a search carries none because it writes nothing.
func TestWriteRecordDetails(t *testing.T) {
	store := NewMemStore()
	tools := Tools(store, []Scope{"user"})
	ctx := WithSession(context.Background(), "sess-w")
	run := func(name, args string) agenttool.Result {
		t.Helper()
		for _, tool := range tools {
			if tool.Name() != name {
				continue
			}
			res, err := tool.Execute(ctx, agenttool.Call{ID: "call_1", Args: json.RawMessage(args)})
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			return res
		}
		t.Fatalf("no tool %q", name)
		return agenttool.Result{}
	}
	steps := []struct {
		tool string
		args string
		seq  uint64
	}{
		{SaveTool, `{"name":"style","content":"Short answers.\n"}`, 1},
		{PatchTool, `{"name":"style","old_text":"Short","new_text":"Long"}`, 2},
		{ForgetTool, `{"name":"style"}`, 3},
	}
	for _, step := range steps {
		res := run(step.tool, step.args)
		rec, ok := res.Details.(WriteRecord)
		if !ok {
			t.Fatalf("%s: Details = %#v, want a WriteRecord", step.tool, res.Details)
		}
		if rec.Tool != step.tool || rec.Change.Seq != step.seq || rec.Change.Session != "sess-w" || rec.Change.Entry.Name != "style" {
			t.Errorf("%s: record = %+v", step.tool, rec)
		}
		// What a recorder writes beside the call, without knowing the
		// type: the namespace this module exports and the record's JSON.
		r, err := agenttool.RecordOf(res.Details)
		if err != nil || r == nil {
			t.Fatalf("%s: RecordOf = %v, %v", step.tool, r, err)
		}
		if r.NS != WriteNS || WriteNS != "agentmemory:write" {
			t.Errorf("%s: namespace = %q", step.tool, r.NS)
		}
		var back WriteRecord
		if err := json.Unmarshal(r.Data, &back); err != nil {
			t.Fatalf("%s: %v", step.tool, err)
		}
		if back.Tool != step.tool || back.Change.Seq != step.seq || back.Change.Entry.Hash != rec.Change.Entry.Hash {
			t.Errorf("%s: recorded %s", step.tool, r.Data)
		}
		// The model sees the line and nothing else.
		if res.Output.String() == "" || strings.Contains(res.Output.String(), "\"seq\"") {
			t.Errorf("%s: output = %q", step.tool, res.Output.String())
		}
	}
	if _, err := call(t, tools, SaveTool, `{"name":"other","content":"x"}`); err != nil {
		t.Fatal(err)
	}
	res := run(SearchTool, `{"query":"other"}`)
	if res.Details != nil {
		t.Errorf("a search carries details: %#v", res.Details)
	}
	if r, err := agenttool.RecordOf(res.Details); err != nil || r != nil {
		t.Errorf("RecordOf(search) = %v, %v", r, err)
	}
	// A failed write records nothing.
	if _, err := call(t, tools, ForgetTool, `{"name":"missing"}`); err == nil {
		t.Error("forget of a missing entry succeeded")
	}
}

func journalLen(s Store) int {
	n := 0
	for range s.Journal(context.Background(), 0) {
		n++
	}
	return n
}
