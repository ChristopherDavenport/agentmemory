package agentmemory

import (
	"context"
	"fmt"
	"strings"
)

// DefaultMaxTotalBytes is the bound [Render] starts with: the content
// of the included entries together.
const DefaultMaxTotalBytes = 32 << 10

// RenderOption configures [Render].
type RenderOption func(*renderOptions)

type renderOptions struct {
	maxTotal int
}

// WithMaxTotalBytes sets the total content bound; the default is
// [DefaultMaxTotalBytes], and a value under one means the default.
func WithMaxTotalBytes(n int) RenderOption {
	return func(o *renderOptions) { o.maxTotal = n }
}

// Manifest lists what [Render] put in the block, for the session's
// provenance: a product records it beside the turn, in a custom entry
// under "agentmemory:render", so a later reader knows which memories
// the model had and which it did not.
type Manifest struct {
	// Entries are the included entries in block order.
	Entries []ManifestEntry `json:"entries"`
	// Omitted are the entries the total bound left out, in the order
	// they would have appeared.
	Omitted []ManifestEntry `json:"omitted,omitempty"`
}

// ManifestEntry names one entry by scope and name, with the hash and
// size of the content the block held or left out.
type ManifestEntry struct {
	Scope Scope  `json:"scope"`
	Name  string `json:"name"`
	Hash  string `json:"hash"`
	Bytes int    `json:"bytes"`
}

// Render returns the in-context block: a header with the counts and
// the budget, then one section per scope in the order given, one
// heading per entry in list order carrying its size and the store's
// limit and its description, then the content verbatim. Entries are
// included until the next would take the content total over the
// bound; it and everything after it are listed under their scopes as
// omitted, so the model knows what memory_search can fetch. The output
// is determined by the store's state and the bounds alone, so an
// unchanged store renders the same bytes and costs nothing in the
// session, and the manifest's hashes are the hashes of the content
// the block shows.
//
// Content is not transformed. A heading inside an entry's content at
// level one to three would read as structure, so the tool descriptions
// ask the model for level four or none.
func Render(ctx context.Context, s Store, scopes []Scope, opts ...RenderOption) (string, Manifest, error) {
	o := renderOptions{maxTotal: DefaultMaxTotalBytes}
	for _, opt := range opts {
		opt(&o)
	}
	if o.maxTotal <= 0 {
		o.maxTotal = DefaultMaxTotalBytes
	}
	for _, scope := range scopes {
		if !ValidScope(scope) {
			return "", Manifest{}, fmt.Errorf("%w: scope %q is not kebab-case", ErrInvalid, scope)
		}
	}
	limit := s.MaxEntryBytes()
	man := Manifest{Entries: []ManifestEntry{}}
	used := 0
	full := false
	var body strings.Builder
	for _, scope := range scopes {
		es, err := s.List(ctx, scope)
		if err != nil {
			return "", Manifest{}, err
		}
		fmt.Fprintf(&body, "\n## %s\n", scope)
		if len(es) == 0 {
			body.WriteString("\nNo entries.\n")
			continue
		}
		var omitted []string
		for _, e := range es {
			me := ManifestEntry{Scope: scope, Name: e.Name, Hash: e.Hash, Bytes: e.Size()}
			if full || used+e.Size() > o.maxTotal {
				full = true
				man.Omitted = append(man.Omitted, me)
				omitted = append(omitted, fmt.Sprintf("%s (%d bytes)", e.Name, e.Size()))
				continue
			}
			used += e.Size()
			man.Entries = append(man.Entries, me)
			fmt.Fprintf(&body, "\n### %s (%d of %d bytes)", e.Name, e.Size(), limit)
			if d := e.Description(); d != "" {
				body.WriteString(" — ")
				body.WriteString(d)
			}
			body.WriteString("\n\n")
			body.WriteString(e.Content)
			if !strings.HasSuffix(e.Content, "\n") {
				body.WriteString("\n")
			}
		}
		if len(omitted) > 0 {
			fmt.Fprintf(&body, "\nNot shown, over the total budget; fetch with memory_search: %s\n", strings.Join(omitted, ", "))
		}
	}
	var b strings.Builder
	b.WriteString("# Memory\n\n")
	fmt.Fprintf(&b, "Entries: %d shown, %d omitted. Used: %d of %d bytes (%d free). Entry limit: %d bytes.\n",
		len(man.Entries), len(man.Omitted), used, o.maxTotal, o.maxTotal-used, limit)
	b.WriteString(body.String())
	return b.String(), man, nil
}

// Usage is one paragraph telling the model how to use the memory tools
// over the block [Render] produced. A product appends it to its
// instructions when it offers [Tools].
func Usage() string {
	return "The Memory block is what you remember between sessions, rendered from the memory store at the start of each turn; it is not part of the conversation. Save a new fact with memory_save, giving a kebab-case name and a description in meta. Edit an existing entry with memory_patch, giving the exact old_text once and its replacement; prefer it to memory_save, which rewrites the entry whole. Remove an entry with memory_forget. Entries the block omits, and any entry you want to read whole, are reachable with memory_search; its results stay in the conversation, so search for what you need. Each entry's heading shows its size and the limit, and the block's header shows the budget; a write over the limit is refused with both numbers, so split or trim before you save. Use headings of level four or none inside an entry."
}
