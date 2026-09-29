package agentmemory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
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
	rendered    func() Manifest
	readScopes  []Scope
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

// WithRendered tells memory_save what the model was shown: fn returns
// the [Manifest] of the render the model is composing from, which is the
// last one the product put in the instructions. A save of an entry that
// manifest holds names the hash the block showed as the write's base,
// through [BasedOn], so a write another session made after the render
// and this save replaced is a record [LostUpdates] reports, rather than
// a base the save claimed by reading it. An entry the manifest does not
// hold, a create or one the block omitted, is based on the save's own
// read, as it is without the option. The result line says which base
// the write took, and says so to the model when the entry changed after
// the render.
//
// fn is called on every save, from the goroutine running the call, so
// it must be safe to call while the product renders the next turn. A
// nil fn is the same as no option. The manifest must be the one the
// model is reading now: a product that re-renders before every model
// call, in BeforeModelCall, keeps it so; one that renders once per turn
// has a second save of an entry in that turn based on the block rather
// than on the model's own first save, and reported as a lost update.
// memory_patch is unchanged: its edit is anchored in the stored text,
// so it merges with a concurrent write or fails when the anchor is gone.
func WithRendered(fn func() Manifest) ToolOption {
	return func(o *toolOptions) { o.rendered = fn }
}

// WithReadScopes gives memory_search scopes the model may read and not
// write, such as project rules a product renders into the block for the
// model to follow and never edit. They join the scopes memory_search
// names in its schema and searches when a call names none, so an entry
// the block omitted from them is reachable as the block says it is;
// memory_save, memory_patch and memory_forget refuse them with "scope
// <name> is read-only", which their descriptions state. [Render] is
// unchanged: a product renders the read scopes beside the others.
//
// A read scope must be kebab-case, given once, and not also one of the
// scopes passed to [Tools]; [Tools] panics otherwise.
func WithReadScopes(scopes ...Scope) ToolOption {
	return func(o *toolOptions) { o.readScopes = append(o.readScopes, scopes...) }
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
	Scopes []string `json:"scopes,omitempty" desc:"Which memories to search; all of them when omitted"`
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
//
// memory_search is annotated read-only, memory_save and memory_forget
// destructive, since a save replaces the entry whole, and memory_patch
// neither; none is open-world. See [agenttool.Annotations].
//
// [WithReadScopes] adds scopes memory_search reaches and the writers
// refuse, and [WithRendered] anchors memory_save to the block the model
// read.
//
// The schema check is the tools' own, added with [agenttool.Wrap]; a
// host that unwraps a tool and executes what is inside skips it.
func Tools(store Store, scopes []Scope, opts ...ToolOption) []agenttool.Tool {
	if len(scopes) == 0 {
		panic("agentmemory.Tools: no scopes")
	}
	for i, s := range scopes {
		if !ValidScope(s) {
			panic(fmt.Sprintf("agentmemory.Tools: scope %q is not kebab-case", s))
		}
		if slices.Contains(scopes[:i], s) {
			panic(fmt.Sprintf("agentmemory.Tools: scope %q is given twice", s))
		}
	}
	o := toolOptions{searchLimit: DefaultSearchLimit, searchBytes: DefaultSearchBytes}
	for _, opt := range opts {
		opt(&o)
	}
	for i, s := range o.readScopes {
		if !ValidScope(s) {
			panic(fmt.Sprintf("agentmemory.Tools: read scope %q is not kebab-case", s))
		}
		if slices.Contains(o.readScopes[:i], s) {
			panic(fmt.Sprintf("agentmemory.Tools: read scope %q is given twice", s))
		}
		if slices.Contains(scopes, s) {
			panic(fmt.Sprintf("agentmemory.Tools: scope %q is both writable and read-only", s))
		}
	}
	if o.searchLimit <= 0 {
		o.searchLimit = DefaultSearchLimit
	}
	if o.searchBytes <= 0 {
		o.searchBytes = DefaultSearchBytes
	}
	t := &toolset{store: store, scopes: scopes, opts: o}
	searchable := slices.Concat(scopes, o.readScopes)
	// Every tool carries a title, so none of them has the zero
	// annotations, which a host such as mcpserver reads as none at all
	// and MCP then defaults to destructive and open-world.
	return []agenttool.Tool{
		tool(SaveTool, t.saveDescription(), t.save, scopes, o.readScopes, agenttool.Annotations{Title: "Save memory entry", Destructive: true}),
		tool(PatchTool, t.patchDescription(), t.patch, scopes, o.readScopes, agenttool.Annotations{Title: "Edit memory entry"}),
		tool(ForgetTool, t.forgetDescription(), t.forget, scopes, o.readScopes, agenttool.Annotations{Title: "Forget memory entry", Destructive: true}),
		tool(SearchTool, t.searchDescription(), t.search, searchable, nil, agenttool.Annotations{Title: "Search memory", ReadOnly: true}),
	}
}

// tool builds one memory tool over a schema that names the scopes, and
// checks a call against that schema before it runs.
//
// The check is the tool's own. agenttool validates against the schema
// it reflected, and skips it for a schema given through WithParameters,
// since it cannot know what that schema promises; these tools build
// their own schema to name the scopes, so they check it themselves.
// Without it every required property and every enum is advice: a
// memory_patch that omits new_text would decode to the empty string and
// delete old_text rather than come back as an error the model can read.
// The check is added with [agenttool.Wrap], which forwards every
// property the tool declares, its annotations among them.
//
// A writer's schema leaves the read scopes out of its enum, so a call
// naming one would fail the schema with a list of the other scopes;
// readOnly is checked first, so the model is told the scope is
// read-only, as the description says.
func tool[Args, Out any](name, description string, fn func(context.Context, Args) (Out, error), scopes, readOnly []Scope, a agenttool.Annotations) agenttool.Tool {
	tree, schema := scopeSchema[Args](scopes)
	inner := agenttool.New(name, description, fn, agenttool.WithParameters(schema), agenttool.WithAnnotations(a))
	return agenttool.Wrap(inner, func(ctx context.Context, call agenttool.Call) (agenttool.Result, error) {
		if err := refuseReadOnly(call.Args, readOnly); err != nil {
			return agenttool.Result{}, err
		}
		if err := tree.ValidateJSON(call.Args); err != nil {
			return agenttool.Result{}, err
		}
		return inner.Execute(ctx, call)
	})
}

// refuseReadOnly returns the read-only refusal when args name one of
// readOnly as their scope, and nil otherwise, including for arguments
// the schema check will refuse on their own.
func refuseReadOnly(args json.RawMessage, readOnly []Scope) error {
	if len(readOnly) == 0 {
		return nil
	}
	var a struct {
		Scope string `json:"scope"`
	}
	if json.Unmarshal(args, &a) != nil {
		return nil
	}
	if slices.Contains(readOnly, Scope(a.Scope)) {
		return readOnlyError(Scope(a.Scope))
	}
	return nil
}

func readOnlyError(s Scope) error { return fmt.Errorf("scope %s is read-only", s) }

// scopeSchema reflects an argument type and writes the scopes a
// product allows into the schema: an enum on scope, and on the items of
// scopes, so the model is steered by what it is sent rather than by
// prose it may not follow, and scope required when there is more than
// one, so a call that omits it is an error it can read rather than a
// write into whichever scope happens to be first. The schema is built
// here because the allowed scopes are chosen at this call and a struct
// tag cannot carry them.
//
// It returns the tree as well as the JSON, because a schema given
// through WithParameters is not one agenttool validates against: the
// tools check it themselves, see [checked]. The call-time check in
// scope stays too, for a caller that reaches a tool function another
// way.
func scopeSchema[Args any](scopes []Scope) (*agenttool.Schema, json.RawMessage) {
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
	return tree, data
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

func (t *toolset) scopeNames() []string { return names(t.scopes) }

func names(scopes []Scope) []string {
	out := make([]string, 0, len(scopes))
	for _, s := range scopes {
		out = append(out, string(s))
	}
	return out
}

// writerScopes is the scope sentence of a writer's description: the
// scopes it writes, and the ones it refuses with the message it gives.
func (t *toolset) writerScopes() string {
	list := t.scopeList()
	for _, s := range t.opts.readScopes {
		list += fmt.Sprintf(" Scope %s is read-only: memory_search reads it, and a write to it is refused with %q.", s, readOnlyError(s).Error())
	}
	return list
}

// searchScopes is the scope sentence of memory_search's description:
// every scope it reaches, which are all searched when a call names
// none.
func (t *toolset) searchScopes() string {
	all := slices.Concat(t.scopes, t.opts.readScopes)
	if len(all) == 1 {
		return fmt.Sprintf("Scope: %s, the only one; scopes may be left out.", all[0])
	}
	list := fmt.Sprintf("Scopes: %s; all of them when scopes is left out.", strings.Join(names(all), ", "))
	if len(t.opts.readScopes) > 0 {
		list += fmt.Sprintf(" Read-only, so for reading: %s.", strings.Join(names(t.opts.readScopes), ", "))
	}
	return list
}

func (t *toolset) saveDescription() string {
	return fmt.Sprintf("Create a memory entry, or replace one whole. To edit an existing entry prefer memory_patch, which sends only the change. Content is Markdown of at most %d bytes; a larger write is refused with the sizes. Give a description in meta so the entry can be found. meta replaces the entry's metadata when you give it and keeps what is there when you leave it out, so send an empty object to clear it. The result says what the write replaced and what became of the metadata. %s",
		t.store.MaxEntryBytes(), t.writerScopes())
}

func (t *toolset) patchDescription() string {
	return "Edit a memory entry by replacing old_text with new_text. old_text must appear exactly once in the entry, copied exactly; the call fails and says so when it is absent or repeated. Prefer this to memory_save for an edit: the call is the size of the change, and the edit is anchored in the stored text, so a change another session made in the meantime is kept. " + t.writerScopes()
}

func (t *toolset) forgetDescription() string {
	return "Remove a memory entry. The store's journal keeps its last content. " + t.writerScopes()
}

func (t *toolset) searchDescription() string {
	return fmt.Sprintf("Find memory entries: the ones the memory block omits, or one to read whole before editing it. Every word of the query must appear in an entry's name, description or content, in any case. Returns up to %d entries unless limit says otherwise, and at most %d bytes of content; the rest are listed by name. Results stay in the conversation, so search for what you need and no more. %s",
		t.opts.searchLimit, t.opts.searchBytes, t.searchScopes())
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
	if slices.Contains(t.opts.readScopes, Scope(s)) {
		return "", readOnlyError(Scope(s))
	}
	return "", fmt.Errorf("scope %q is not available; %s", s, t.scopeList())
}

// readScope resolves one of a search's scopes, which may be a read
// scope as well as a writable one.
func (t *toolset) readScope(s string) (Scope, error) {
	all := slices.Concat(t.scopes, t.opts.readScopes)
	if slices.Contains(all, Scope(s)) {
		return Scope(s), nil
	}
	return "", fmt.Errorf("scope %q is not available; %s", s, t.searchScopes())
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
//
// The state it was built on is the one the model composed from when
// the product says what that was, see [WithRendered]: the model wrote
// content from the block, not from this read, and a write that landed
// between the two is the one this save discards.
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
	if stored != nil && a.Meta == nil {
		e.Meta = stored.Meta
	}
	shown, fromBlock := t.shown(scope, a.Name)
	var opts []PutOption
	switch {
	case fromBlock:
		opts = append(opts, BasedOn(shown))
	case stored != nil:
		opts = append(opts, BasedOn(stored.Hash))
	}
	c, err := t.store.Put(ctx, e, opts...)
	if err != nil {
		return agenttool.Result{}, err
	}
	return wrote(SaveTool, c, "Saved %s/%s (%d of %d bytes) %s — %s; %s%s",
		scope, a.Name, len(a.Content), t.store.MaxEntryBytes(), Hash(a.Content), savedWhat(stored), metaWhat(stored, a.Meta, e.Meta), t.basedWhat(stored, shown, fromBlock))
}

// shown returns the hash the rendered block showed for the entry, and
// whether it showed the entry at all; see [WithRendered].
func (t *toolset) shown(scope Scope, name string) (string, bool) {
	if t.opts.rendered == nil {
		return "", false
	}
	for _, me := range t.opts.rendered().Entries {
		if me.Scope == scope && me.Name == name {
			return me.Hash, true
		}
	}
	return "", false
}

// basedWhat says which state the write was built on, when the tools
// know what the block showed: the block's, or, for an entry it did not
// show, the stored one. When the entry changed after the block was
// rendered, the model is told that this write replaced the change, so
// it can read the entry and put back what it needs.
func (t *toolset) basedWhat(stored *Entry, shown string, fromBlock bool) string {
	switch {
	case t.opts.rendered == nil || (!fromBlock && stored == nil):
		return ""
	case !fromBlock:
		return "; built on the stored entry, which the block did not show"
	case stored == nil:
		return "; built on the block, but the entry was forgotten after the block was rendered and this write created it again"
	case stored.Hash != shown:
		return fmt.Sprintf("; built on the block's %s, but the entry changed to %s after the block was rendered and this write replaced that change; read it with memory_search if the change mattered", shown, stored.Hash)
	}
	return "; built on the block"
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
	scopes := slices.Concat(t.scopes, t.opts.readScopes)
	if len(a.Scopes) > 0 {
		scopes = make([]Scope, 0, len(a.Scopes))
		for _, s := range a.Scopes {
			scope, err := t.readScope(s)
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
	searched := strings.Join(names(scopes), ", ")
	if len(found) == 0 {
		// A search writes nothing, so its result carries no record.
		return agenttool.Text(fmt.Sprintf("No entries in %s match %q.", searched, a.Query)), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d %s in %s for %q.\n", len(found), plural(len(found), "match", "matches"), searched, a.Query)
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
