package agentmemory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/ChristopherDavenport/agenttool"
)

// The tools' names.
const (
	SaveTool   = "memory_save"
	PatchTool  = "memory_patch"
	ForgetTool = "memory_forget"
	SearchTool = "memory_search"
)

// Defaults for memory_search. A search result is appended to the
// transcript once and stays for the rest of the session, unlike the
// rendered block, so both are low against the render budget.
const (
	// DefaultSearchLimit is how many entries a search returns when the
	// call names no limit.
	DefaultSearchLimit = 5
	// DefaultSearchBytes bounds the content one search result shows;
	// matches past it are listed by name.
	DefaultSearchBytes = 16 << 10
)

// patchAttempts is how many times memory_patch re-reads and re-applies
// an edit whose conditional write lost to another writer.
const patchAttempts = 4

// WriteNS is the namespace a memory write is recorded under, so a
// reader of a session recognises one without knowing the product that
// wrote it. It is [WriteRecord]'s [agenttool.Recordable] namespace.
const WriteNS = "agentmemory:write"

// WriteRecord is what a memory tool knows and the line it returns to
// the model does not: the journal record the write produced, with the
// sequence number it took, the hashes it was built on and replaced, and
// the session it was attributed to. The tools set it as the Details of
// their [agenttool.Result], where it is never sent to the model, and a
// recorder that does not know the type writes it beside the call as a
// namespaced entry, which is what joins a session to the store's
// journal without re-reading a journal that may since have been
// compacted or moved.
type WriteRecord struct {
	// Tool is the tool that made the write.
	Tool string `json:"tool"`
	// Change is the record the store appended.
	Change Change `json:"change"`
}

// RecordNS implements [agenttool.Recordable].
func (WriteRecord) RecordNS() string { return WriteNS }

var _ agenttool.Recordable = WriteRecord{}

// ToolOption configures [Tools].
type ToolOption func(*toolOptions)

type toolOptions struct {
	searchLimit int
	searchBytes int
}

// WithSearchLimit sets the number of entries memory_search returns
// when the call names no limit; the default is [DefaultSearchLimit],
// and a value under one means the default.
func WithSearchLimit(n int) ToolOption {
	return func(o *toolOptions) { o.searchLimit = n }
}

// WithSearchBytes sets the most content one memory_search result
// shows; the default is [DefaultSearchBytes], and a value under one
// means the default.
func WithSearchBytes(n int) ToolOption {
	return func(o *toolOptions) { o.searchBytes = n }
}

type saveArgs struct {
	Scope   string            `json:"scope,omitempty" desc:"Which memory the entry belongs to"`
	Name    string            `json:"name" desc:"Kebab-case name, unique within the scope: lowercase letters, digits and hyphens"`
	Content string            `json:"content" desc:"The whole content of the entry, Markdown"`
	Meta    map[string]string `json:"meta,omitempty" desc:"Metadata beside the content: a description is shown in the block and the index; keys are kebab-case, values one line. Omit it to keep the metadata the entry has; give {} to clear it"`
}

type patchArgs struct {
	Scope   string `json:"scope,omitempty" desc:"Which memory the entry belongs to"`
	Name    string `json:"name" desc:"The entry to edit"`
	OldText string `json:"old_text" desc:"Text that appears exactly once in the entry, copied exactly"`
	NewText string `json:"new_text" desc:"What replaces it; empty removes it"`
}

type forgetArgs struct {
	Scope string `json:"scope,omitempty" desc:"Which memory the entry belongs to"`
	Name  string `json:"name" desc:"The entry to remove"`
}

type searchArgs struct {
	Query  string   `json:"query" desc:"Words that must all appear in an entry's name, description or content, in any case"`
	Scopes []string `json:"scopes,omitempty" desc:"Which memories to search; all the ones this tool can reach when omitted"`
	Limit  int      `json:"limit,omitempty" desc:"At most this many entries; keep it low, results stay in the conversation"`
}

// Tools returns the four tools through which the model reaches store,
// restricted to the scopes the product allows: memory_save,
// memory_patch, memory_forget and memory_search. A call that names a
// scope outside the list is an error the model sees, and the list is
// in the schema as an enum, not only in the description; a call that
// omits the scope is an error too, unless the product allows one scope
// and there is nothing to choose. Every write is a function call in
// the transcript, and the store's journal records it under the
// session on the context, see [WithSession]. A write's result carries
// that journal record as [WriteRecord] in its Details, which a
// recorder writes beside the call under [WriteNS] and the model never
// sees. Tools panics with no scopes, since a tool set that can reach
// nothing is a programming error.
func Tools(store Store, scopes []Scope, opts ...ToolOption) []agenttool.Tool {
	if len(scopes) == 0 {
		panic("agentmemory.Tools: no scopes")
	}
	for _, s := range scopes {
		if !ValidScope(s) {
			panic(fmt.Sprintf("agentmemory.Tools: scope %q is not kebab-case", s))
		}
	}
	o := toolOptions{searchLimit: DefaultSearchLimit, searchBytes: DefaultSearchBytes}
	for _, opt := range opts {
		opt(&o)
	}
	if o.searchLimit <= 0 {
		o.searchLimit = DefaultSearchLimit
	}
	if o.searchBytes <= 0 {
		o.searchBytes = DefaultSearchBytes
	}
	t := &toolset{store: store, scopes: scopes, opts: o}
	return []agenttool.Tool{
		agenttool.New(SaveTool, t.saveDescription(), t.save, agenttool.WithParameters(scopeSchema[saveArgs](scopes))),
		agenttool.New(PatchTool, t.patchDescription(), t.patch, agenttool.WithParameters(scopeSchema[patchArgs](scopes))),
		agenttool.New(ForgetTool, t.forgetDescription(), t.forget, agenttool.WithParameters(scopeSchema[forgetArgs](scopes))),
		agenttool.New(SearchTool, t.searchDescription(), t.search, agenttool.WithParameters(scopeSchema[searchArgs](scopes))),
	}
}

// scopeSchema reflects an argument type and writes the scopes a
// product allows into the schema: an enum on scope, and on the items of
// scopes, so the model is steered by what it is sent rather than by
// prose it may not follow, and scope required when there is more than
// one, so a call that omits it is an error it can read rather than a
// write into whichever scope happens to be first. The schema is built
// here because the allowed scopes are chosen at this call and a struct
// tag cannot carry them.
//
// A schema given this way is not what agenttool validates a call
// against, so the call-time check in scope stays: it is the one a model
// that ignores the schema meets.
func scopeSchema[Args any](scopes []Scope) json.RawMessage {
	var zero Args
	tree, err := agenttool.Reflect(reflect.TypeOf(&zero).Elem())
	if err != nil {
		panic(fmt.Sprintf("agentmemory.Tools: %v", err))
	}
	values := make([]any, 0, len(scopes))
	for _, s := range scopes {
		values = append(values, string(s))
	}
	for _, p := range tree.Properties {
		switch p.Name {
		case "scope":
			p.Schema.Enum = values
			if len(scopes) > 1 {
				p.Schema.Description = "Which memory the entry belongs to; name one of the values"
				tree.Required = append([]string{"scope"}, tree.Required...)
			}
		case "scopes":
			if p.Schema.Items != nil {
				p.Schema.Items.Enum = values
			}
		}
	}
	data, err := json.Marshal(tree)
	if err != nil {
		panic(fmt.Sprintf("agentmemory.Tools: %v", err))
	}
	return data
}

type toolset struct {
	store  Store
	scopes []Scope
	opts   toolOptions
}

// scopeList names the choices for a tool description. With one scope
// the argument may be left out, since there is nothing to choose; with
// more than one it is required, so the failure mode of forgetting it
// is an error rather than a fact written into whichever scope the
// product listed first.
func (t *toolset) scopeList() string {
	if len(t.scopes) == 1 {
		return fmt.Sprintf("Scope: %s, the only one; the argument may be left out.", t.scopes[0])
	}
	return fmt.Sprintf("Scopes: %s; name one on every call.", strings.Join(t.scopeNames(), ", "))
}

func (t *toolset) scopeNames() []string {
	names := make([]string, 0, len(t.scopes))
	for _, s := range t.scopes {
		names = append(names, string(s))
	}
	return names
}

func (t *toolset) saveDescription() string {
	return fmt.Sprintf("Create a memory entry, or replace one whole. To edit an existing entry prefer memory_patch, which sends only the change. Content is Markdown of at most %d bytes; a larger write is refused with the sizes. Give a description in meta so the entry can be found. meta replaces the entry's metadata when you give it and keeps what is there when you leave it out, so send an empty object to clear it. The result says what the write replaced and what became of the metadata. %s",
		t.store.MaxEntryBytes(), t.scopeList())
}

func (t *toolset) patchDescription() string {
	return "Edit a memory entry by replacing old_text with new_text. old_text must appear exactly once in the entry, copied exactly; the call fails and says so when it is absent or repeated. Prefer this to memory_save for an edit: the call is the size of the change, and the edit is anchored in the stored text, so a change another session made in the meantime is kept. " + t.scopeList()
}

func (t *toolset) forgetDescription() string {
	return "Remove a memory entry. The store's journal keeps its last content. " + t.scopeList()
}

func (t *toolset) searchDescription() string {
	return fmt.Sprintf("Find memory entries: the ones the memory block omits, or one to read whole before editing it. Every word of the query must appear in an entry's name, description or content, in any case. Returns up to %d entries unless limit says otherwise, and at most %d bytes of content; the rest are listed by name. Results stay in the conversation, so search for what you need and no more. %s",
		t.opts.searchLimit, t.opts.searchBytes, t.scopeList())
}

// scope resolves a call's scope argument to an allowed scope. An
// omitted scope is the only scope when there is one, and an error
// naming the choices when there are several.
func (t *toolset) scope(s string) (Scope, error) {
	if s == "" {
		if len(t.scopes) == 1 {
			return t.scopes[0], nil
		}
		return "", fmt.Errorf("scope is required; name one of: %s", strings.Join(t.scopeNames(), ", "))
	}
	for _, allowed := range t.scopes {
		if string(allowed) == s {
			return allowed, nil
		}
	}
	return "", fmt.Errorf("scope %q is not available; %s", s, t.scopeList())
}

// wrote builds a write's result: the line the model reads, and the
// journal record beside it as details only a recorder sees.
func wrote(tool string, c *Change, format string, args ...any) (agenttool.Result, error) {
	res := agenttool.Text(fmt.Sprintf(format, args...))
	if c != nil {
		res.Details = WriteRecord{Tool: tool, Change: *c}
	}
	return res, nil
}

// save writes the whole entry. It reads the stored one first for two
// reasons: a call that leaves meta out means the content, not the
// description, so the stored metadata is carried forward rather than
// deleted in silence; and the write then names the state it was built
// on, so the journal can show a write that lost another's. Put stays a
// replace of the whole entry, because a store whose write is sometimes
// a merge cannot keep the journal a list of full states.
func (t *toolset) save(ctx context.Context, a saveArgs) (agenttool.Result, error) {
	scope, err := t.scope(a.Scope)
	if err != nil {
		return agenttool.Result{}, err
	}
	stored, err := t.store.Get(ctx, scope, a.Name)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return agenttool.Result{}, err
	}
	if errors.Is(err, ErrNotFound) {
		stored = nil
	}
	e := Entry{Scope: scope, Name: a.Name, Content: a.Content, Meta: a.Meta}
	var opts []PutOption
	if stored != nil {
		opts = append(opts, BasedOn(stored.Hash))
		if a.Meta == nil {
			e.Meta = stored.Meta
		}
	}
	c, err := t.store.Put(ctx, e, opts...)
	if err != nil {
		return agenttool.Result{}, err
	}
	return wrote(SaveTool, c, "Saved %s/%s (%d of %d bytes) %s — %s; %s",
		scope, a.Name, len(a.Content), t.store.MaxEntryBytes(), Hash(a.Content), savedWhat(stored), metaWhat(stored, a.Meta, e.Meta))
}

// savedWhat says what the write did to the entry.
func savedWhat(stored *Entry) string {
	if stored == nil {
		return "created"
	}
	return fmt.Sprintf("replaced %d bytes", stored.Size())
}

// metaWhat says what became of the metadata, since a call that leaves
// it out is the one that used to delete it.
func metaWhat(stored *Entry, given, final map[string]string) string {
	switch {
	case len(final) == 0 && (stored == nil || len(stored.Meta) == 0):
		return "no meta"
	case len(final) == 0:
		return "meta cleared"
	case given == nil:
		return "meta kept (" + metaKeys(final) + ")"
	case stored == nil:
		return "meta set (" + metaKeys(final) + ")"
	}
	return "meta replaced (" + metaKeys(final) + ")"
}

func metaKeys(meta map[string]string) string {
	keys := make([]string, 0, len(meta))
	for k := range meta {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

func (t *toolset) patch(ctx context.Context, a patchArgs) (agenttool.Result, error) {
	scope, err := t.scope(a.Scope)
	if err != nil {
		return agenttool.Result{}, err
	}
	if a.OldText == "" {
		return agenttool.Result{}, errors.New("old_text is empty; give the exact text to replace, or use memory_save to write the entry whole")
	}
	var lastErr error
	for attempt := 0; attempt < patchAttempts; attempt++ {
		cur, err := t.store.Get(ctx, scope, a.Name)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return agenttool.Result{}, fmt.Errorf("%w; memory_save creates an entry", err)
			}
			return agenttool.Result{}, err
		}
		switch n := strings.Count(cur.Content, a.OldText); {
		case n == 0:
			return agenttool.Result{}, fmt.Errorf("old_text does not appear in %s/%s; read the entry with memory_search and copy the text exactly", scope, a.Name)
		case n > 1:
			return agenttool.Result{}, fmt.Errorf("old_text appears %d times in %s/%s; include more of the surrounding text so it appears once", n, scope, a.Name)
		}
		next := *cur
		next.Content = strings.Replace(cur.Content, a.OldText, a.NewText, 1)
		c, err := t.store.Put(ctx, next, IfHash(cur.Hash))
		if err == nil {
			return wrote(PatchTool, c, "Patched %s/%s (%d of %d bytes) %s", scope, a.Name, len(next.Content), t.store.MaxEntryBytes(), Hash(next.Content))
		}
		if !errors.Is(err, ErrConflict) {
			return agenttool.Result{}, err
		}
		// Another writer changed the entry between the read and the
		// write. The edit is anchored in old_text, so apply it to the
		// new content.
		lastErr = err
	}
	return agenttool.Result{}, fmt.Errorf("%w after %d attempts; another session keeps changing it, try again", lastErr, patchAttempts)
}

func (t *toolset) forget(ctx context.Context, a forgetArgs) (agenttool.Result, error) {
	scope, err := t.scope(a.Scope)
	if err != nil {
		return agenttool.Result{}, err
	}
	c, err := t.store.Forget(ctx, scope, a.Name)
	if err != nil {
		return agenttool.Result{}, err
	}
	return wrote(ForgetTool, c, "Forgot %s/%s", scope, a.Name)
}

func (t *toolset) search(ctx context.Context, a searchArgs) (agenttool.Result, error) {
	if strings.TrimSpace(a.Query) == "" {
		return agenttool.Result{}, errors.New("query is empty; give one or more words")
	}
	scopes := t.scopes
	if len(a.Scopes) > 0 {
		scopes = make([]Scope, 0, len(a.Scopes))
		for _, s := range a.Scopes {
			scope, err := t.scope(s)
			if err != nil {
				return agenttool.Result{}, err
			}
			scopes = append(scopes, scope)
		}
	}
	limit := a.Limit
	if limit <= 0 {
		limit = t.opts.searchLimit
	}
	found, err := t.store.Search(ctx, scopes, a.Query, limit)
	if err != nil {
		return agenttool.Result{}, err
	}
	names := make([]string, 0, len(scopes))
	for _, s := range scopes {
		names = append(names, string(s))
	}
	if len(found) == 0 {
		// A search writes nothing, so its result carries no record.
		return agenttool.Text(fmt.Sprintf("No entries in %s match %q.", strings.Join(names, ", "), a.Query)), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d %s in %s for %q.\n", len(found), plural(len(found), "match", "matches"), strings.Join(names, ", "), a.Query)
	shown := 0
	var unshown []string
	for _, e := range found {
		if shown+e.Size() > t.opts.searchBytes {
			unshown = append(unshown, fmt.Sprintf("%s/%s (%d bytes)", e.Scope, e.Name, e.Size()))
			continue
		}
		shown += e.Size()
		fmt.Fprintf(&b, "\n%s/%s (%d bytes)", e.Scope, e.Name, e.Size())
		if d := e.Description(); d != "" {
			b.WriteString(": ")
			b.WriteString(d)
		}
		b.WriteString("\n")
		b.WriteString(e.Content)
		if !strings.HasSuffix(e.Content, "\n") {
			b.WriteString("\n")
		}
	}
	if len(unshown) > 0 {
		fmt.Fprintf(&b, "\nNot shown, over the %d byte result limit; search for them by name: %s\n", t.opts.searchBytes, strings.Join(unshown, ", "))
	}
	return agenttool.Text(b.String()), nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
