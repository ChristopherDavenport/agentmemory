package agentmemory

import (
	"context"
	"errors"
	"fmt"
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
	Scope   string            `json:"scope,omitempty" desc:"Which memory the entry belongs to; see the tool description for the choices and the default"`
	Name    string            `json:"name" desc:"Kebab-case name, unique within the scope: lowercase letters, digits and hyphens"`
	Content string            `json:"content" desc:"The whole content of the entry, Markdown"`
	Meta    map[string]string `json:"meta,omitempty" desc:"Metadata beside the content: a description is shown in the block and the index; keys are kebab-case, values one line"`
}

type patchArgs struct {
	Scope   string `json:"scope,omitempty" desc:"Which memory the entry belongs to; see the tool description for the choices and the default"`
	Name    string `json:"name" desc:"The entry to edit"`
	OldText string `json:"old_text" desc:"Text that appears exactly once in the entry, copied exactly"`
	NewText string `json:"new_text" desc:"What replaces it; empty removes it"`
}

type forgetArgs struct {
	Scope string `json:"scope,omitempty" desc:"Which memory the entry belongs to; see the tool description for the choices and the default"`
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
// scope outside the list is an error the model sees; a call that
// omits the scope uses the first. Every write is a function call in
// the transcript, and the store's journal records it under the
// session on the context, see [WithSession]. Tools panics with no
// scopes, since a tool set that can reach nothing is a programming
// error.
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
		agenttool.New(SaveTool, t.saveDescription(), t.save),
		agenttool.New(PatchTool, t.patchDescription(), t.patch),
		agenttool.New(ForgetTool, t.forgetDescription(), t.forget),
		agenttool.New(SearchTool, t.searchDescription(), t.search),
	}
}

type toolset struct {
	store  Store
	scopes []Scope
	opts   toolOptions
}

// scopeList names the choices for a tool description.
func (t *toolset) scopeList() string {
	names := make([]string, 0, len(t.scopes))
	for _, s := range t.scopes {
		names = append(names, string(s))
	}
	return fmt.Sprintf("Scopes: %s; the default is %s.", strings.Join(names, ", "), t.scopes[0])
}

func (t *toolset) saveDescription() string {
	return fmt.Sprintf("Create a memory entry, or replace one whole. To edit an existing entry prefer memory_patch, which sends only the change. Content is Markdown of at most %d bytes; a larger write is refused with the sizes. Give a description in meta so the entry can be found. %s",
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

// scope resolves a call's scope argument to an allowed scope.
func (t *toolset) scope(s string) (Scope, error) {
	if s == "" {
		return t.scopes[0], nil
	}
	for _, allowed := range t.scopes {
		if string(allowed) == s {
			return allowed, nil
		}
	}
	return "", fmt.Errorf("scope %q is not available; %s", s, t.scopeList())
}

func (t *toolset) save(ctx context.Context, a saveArgs) (string, error) {
	scope, err := t.scope(a.Scope)
	if err != nil {
		return "", err
	}
	e := Entry{Scope: scope, Name: a.Name, Content: a.Content, Meta: a.Meta}
	if err := t.store.Put(ctx, e); err != nil {
		return "", err
	}
	return fmt.Sprintf("Saved %s/%s (%d of %d bytes) %s", scope, a.Name, len(a.Content), t.store.MaxEntryBytes(), Hash(a.Content)), nil
}

func (t *toolset) patch(ctx context.Context, a patchArgs) (string, error) {
	scope, err := t.scope(a.Scope)
	if err != nil {
		return "", err
	}
	if a.OldText == "" {
		return "", errors.New("old_text is empty; give the exact text to replace, or use memory_save to write the entry whole")
	}
	var lastErr error
	for attempt := 0; attempt < patchAttempts; attempt++ {
		cur, err := t.store.Get(ctx, scope, a.Name)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return "", fmt.Errorf("%w; memory_save creates an entry", err)
			}
			return "", err
		}
		switch n := strings.Count(cur.Content, a.OldText); {
		case n == 0:
			return "", fmt.Errorf("old_text does not appear in %s/%s; read the entry with memory_search and copy the text exactly", scope, a.Name)
		case n > 1:
			return "", fmt.Errorf("old_text appears %d times in %s/%s; include more of the surrounding text so it appears once", n, scope, a.Name)
		}
		next := *cur
		next.Content = strings.Replace(cur.Content, a.OldText, a.NewText, 1)
		err = t.store.Put(ctx, next, IfHash(cur.Hash))
		if err == nil {
			return fmt.Sprintf("Patched %s/%s (%d of %d bytes) %s", scope, a.Name, len(next.Content), t.store.MaxEntryBytes(), Hash(next.Content)), nil
		}
		if !errors.Is(err, ErrConflict) {
			return "", err
		}
		// Another writer changed the entry between the read and the
		// write. The edit is anchored in old_text, so apply it to the
		// new content.
		lastErr = err
	}
	return "", fmt.Errorf("%w after %d attempts; another session keeps changing it, try again", lastErr, patchAttempts)
}

func (t *toolset) forget(ctx context.Context, a forgetArgs) (string, error) {
	scope, err := t.scope(a.Scope)
	if err != nil {
		return "", err
	}
	if err := t.store.Forget(ctx, scope, a.Name); err != nil {
		return "", err
	}
	return fmt.Sprintf("Forgot %s/%s", scope, a.Name), nil
}

func (t *toolset) search(ctx context.Context, a searchArgs) (string, error) {
	if strings.TrimSpace(a.Query) == "" {
		return "", errors.New("query is empty; give one or more words")
	}
	scopes := t.scopes
	if len(a.Scopes) > 0 {
		scopes = make([]Scope, 0, len(a.Scopes))
		for _, s := range a.Scopes {
			scope, err := t.scope(s)
			if err != nil {
				return "", err
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
		return "", err
	}
	names := make([]string, 0, len(scopes))
	for _, s := range scopes {
		names = append(names, string(s))
	}
	if len(found) == 0 {
		return fmt.Sprintf("No entries in %s match %q.", strings.Join(names, ", "), a.Query), nil
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
	return b.String(), nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
