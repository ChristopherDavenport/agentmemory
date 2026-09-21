package agentmemory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// DefaultMaxTotalBytes is the bound [Render] starts with: the whole
// block, header and headings included.
const DefaultMaxTotalBytes = 32 << 10

// RenderOption configures [Render].
type RenderOption func(*renderOptions)

type renderOptions struct {
	maxTotal int
}

// WithMaxTotalBytes sets the block's bound; the default is
// [DefaultMaxTotalBytes], and a value under one means the default.
func WithMaxTotalBytes(n int) RenderOption {
	return func(o *renderOptions) { o.maxTotal = n }
}

// ManifestNS is the namespace a [Manifest] is recorded under, so a
// reader of a session recognises one without knowing the product that
// wrote it. See [Manifest.Record].
const ManifestNS = "agentmemory:render"

// Manifest lists what [Render] put in the block, for the session's
// provenance: a product records it beside the turn, in a custom entry
// under [ManifestNS], so a later reader knows which memories the model
// had and which it did not.
//
// The render is a pure function of the store and the bounds, so most
// turns produce the manifest the turn before produced. [Manifest.Hash]
// is what a product compares to record it only when it has changed,
// which is what the recorder already does for the block itself, and
// [Manifest.Record] is the namespace and the bytes to hand to the
// recorder when it has.
type Manifest struct {
	// Entries are the included entries in block order.
	Entries []ManifestEntry `json:"entries"`
	// Omitted are the entries the block's bound left out, in the order
	// they would have appeared, each with the reason.
	Omitted []ManifestEntry `json:"omitted,omitempty"`
}

// ManifestEntry names one entry by scope and name, with the hash and
// size of the content the block held or left out, and, for an omitted
// one, why. The fields are what a session needs to say what the model
// was shown and what it was not.
type ManifestEntry struct {
	Scope Scope  `json:"scope"`
	Name  string `json:"name"`
	Hash  string `json:"hash"`
	Bytes int    `json:"bytes"`
	// Reason is why the entry was left out, one of the Omit constants,
	// and empty for an entry the block holds.
	Reason string `json:"reason,omitempty"`
}

// Reasons [Render] leaves an entry out of the block, carried on
// [ManifestEntry.Reason].
const (
	// OmitBudget is an entry whose rendered form did not fit in what
	// was left of the block's bound. Entries after it may still fit,
	// so the block holds what it can and the model is told the rest by
	// name.
	OmitBudget = "budget"
)

// Hash is the manifest's identity: "sha256:" and the hex digest of the
// entries it holds and the ones it left out, in order, each by scope,
// name, content hash, size and, for an omission, reason. Two renders
// that showed the model the same memories hash the same, so a product
// records the manifest when the hash differs from the last one it
// recorded and writes nothing when the render has not moved.
func (m Manifest) Hash() string {
	var b strings.Builder
	for _, e := range m.Entries {
		fmt.Fprintf(&b, "+ %s/%s %s %d\n", e.Scope, e.Name, e.Hash, e.Bytes)
	}
	for _, e := range m.Omitted {
		fmt.Fprintf(&b, "- %s/%s %s %d %s\n", e.Scope, e.Name, e.Hash, e.Bytes, e.Reason)
	}
	return Hash(b.String())
}

// Record returns the namespace and the JSON of the manifest, for a
// product to hand to its session recorder: this module never imports
// the loop or the session format, so the call that writes the entry is
// the product's, and the namespace and the bytes are this module's.
//
//	if h := man.Hash(); h != last {
//		last = h
//		ns, data := man.Record()
//		err := rec.Annotate(ctx, ns, json.RawMessage(data))
//	}
//
// A manifest is strings and numbers, so encoding it cannot fail; a
// caller that wants an error of its own marshals the value itself.
func (m Manifest) Record() (ns string, data []byte) {
	data, err := json.Marshal(m)
	if err != nil {
		// Unreachable: every field is a string, an int or a slice of
		// them. Recording something a reader can see went wrong beats
		// recording nothing.
		data = fmt.Appendf(nil, "{%q:%q}", "error", err.Error())
	}
	return ManifestNS, data
}

// Render returns the in-context block: a header with the counts and
// the budget, then one section per scope in the order given, one
// heading per entry in list order carrying its size and the store's
// limit and its description, then the content verbatim.
//
// The bound is on the block, not on the content it holds: the header,
// the scope headings, the per-entry headings with their descriptions
// and the list of what was left out are all counted, because they are
// all sent to the model, and the header reports the block's own size
// so what the model reads is what the window pays. An entry whose
// rendered form does not fit is skipped, recorded in [Manifest.Omitted]
// with [OmitBudget] and listed by name under its scope so the model
// knows what memory_search can fetch; entries after it are still
// considered, so one large entry cannot hide the small ones that sort
// after it. Order is never changed: the block holds the entries in
// list order, whichever were skipped.
//
// The output is determined by the store's state and the bounds alone,
// so an unchanged store renders the same bytes and costs nothing in
// the session, and the manifest's hashes are the hashes of the content
// the block shows. A product records the manifest when
// [Manifest.Hash] differs from the last one it recorded, under
// [ManifestNS]; see [Manifest.Record]. The header's own width is reserved before the body
// is built, at the widest the counts could be, so a block can come out
// a few bytes under the bound; it never comes out over it, except that
// the header and one heading per scope are always written, so a bound
// too small for those cannot be met.
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
	lists := make([][]Entry, len(scopes))
	total := 0
	for i, scope := range scopes {
		es, err := s.List(ctx, scope)
		if err != nil {
			return "", Manifest{}, err
		}
		lists[i] = es
		total += len(es)
	}
	// What the block holds whatever it shows: the header, at the widest
	// its numbers can be, since it is written last and reports its own
	// size, and one heading per scope. Taking it off the bound before
	// anything else means the entries of an early scope cannot spend
	// what a later scope's heading needs.
	fixed := len(header(total, total, o.maxTotal, o.maxTotal, o.maxTotal, limit))
	for i, scope := range scopes {
		fixed += len(scopeHead(scope))
		if len(lists[i]) == 0 {
			fixed += len(noEntries)
		}
	}

	man := Manifest{Entries: []ManifestEntry{}}
	var body strings.Builder
	spent := 0 // the entries and the omission lines written so far
	for i, scope := range scopes {
		body.WriteString(scopeHead(scope))
		es := lists[i]
		if len(es) == 0 {
			body.WriteString(noEntries)
			continue
		}
		room := o.maxTotal - fixed - spent
		// Packing twice is how the line naming the omissions pays for
		// itself: the first pass says whether there will be one, the
		// second holds room for it. A pass that holds room back can only
		// leave more out, so the second pass settles it.
		in, used := pack(es, limit, room)
		if !all(in) {
			in, used = pack(es, limit, room-omitLineReserve(es))
		}
		var dropped []Entry
		for j, e := range es {
			me := ManifestEntry{Scope: scope, Name: e.Name, Hash: e.Hash, Bytes: e.Size()}
			if in[j] {
				body.WriteString(entryBlock(e, limit))
				man.Entries = append(man.Entries, me)
				continue
			}
			me.Reason = OmitBudget
			man.Omitted = append(man.Omitted, me)
			dropped = append(dropped, e)
		}
		spent += used
		if len(dropped) > 0 {
			line := omitLine(dropped, o.maxTotal-fixed-spent)
			body.WriteString(line)
			spent += len(line)
		}
	}
	// The header reports the block's own size, so settling it is a fixed
	// point: the reservation above is at least as wide as the header can
	// be, and writing the size into it narrows it, which narrows the
	// size. A few passes reach the width it keeps, and every pass is
	// inside the bound because none is wider than the reservation.
	shown, left := len(man.Entries), len(man.Omitted)
	size := fixed + spent
	for range 8 {
		next := len(header(shown, left, size, o.maxTotal-size, o.maxTotal, limit)) + body.Len()
		if next == size {
			break
		}
		size = next
	}
	return header(shown, left, size, o.maxTotal-size, o.maxTotal, limit) + body.String(), man, nil
}

const (
	noEntries = "\nNo entries.\n"
	omitHead  = "\nNot shown, over the block budget; fetch with memory_search: "
	omitCount = "\nNot shown, over the block budget: %d entries.\n"
	moreTail  = ", and %d more"
)

func scopeHead(scope Scope) string { return "\n## " + string(scope) + "\n" }

// pack decides which of es the block has room for, in list order,
// skipping one that does not fit and going on to the next, so a large
// entry cannot hide the small ones after it. It returns the decision
// per entry and the bytes they take.
func pack(es []Entry, limit, room int) ([]bool, int) {
	in := make([]bool, len(es))
	used := 0
	for i, e := range es {
		n := len(entryBlock(e, limit))
		if used+n <= room {
			in[i] = true
			used += n
		}
	}
	return in, used
}

func all(in []bool) bool {
	for _, ok := range in {
		if !ok {
			return false
		}
	}
	return true
}

// omitLineReserve is the most the line naming a scope's omissions can
// take, so a pack that will need one can hold room for it.
func omitLineReserve(es []Entry) int {
	item := 0
	for _, e := range es {
		if n := len(omitItem(e)); n > item {
			item = n
		}
	}
	return len(omitHead) + item + len(fmt.Sprintf(moreTail, len(es))) + 1
}

func omitItem(e Entry) string { return fmt.Sprintf("%s (%d bytes)", e.Name, e.Size()) }

// omitLine names the entries the block left out, so the model knows
// what memory_search can fetch, within the room left for it: the names
// that fit, then how many more there are. With room for neither it
// gives the count alone, and with room for nothing it gives nothing;
// the manifest carries every omission either way.
func omitLine(dropped []Entry, room int) string {
	var b strings.Builder
	listed := 0
	for i, e := range dropped {
		piece := omitItem(e)
		if listed > 0 {
			piece = ", " + piece
		}
		tail := 0
		if rest := len(dropped) - i - 1; rest > 0 {
			tail = len(fmt.Sprintf(moreTail, rest))
		}
		if len(omitHead)+b.Len()+len(piece)+tail+1 > room {
			break
		}
		b.WriteString(piece)
		listed++
	}
	if listed == 0 {
		if short := fmt.Sprintf(omitCount, len(dropped)); len(short) <= room {
			return short
		}
		return ""
	}
	out := omitHead + b.String()
	if rest := len(dropped) - listed; rest > 0 {
		out += fmt.Sprintf(moreTail, rest)
	}
	return out + "\n"
}

// header is the block's first lines: what it holds and what it cost.
func header(shown, omitted, size, free, max, limit int) string {
	return fmt.Sprintf("# Memory\n\nEntries: %d shown, %d omitted. Block: %d of %d bytes (%d free). Entry limit: %d bytes.\n",
		shown, omitted, size, max, free, limit)
}

// entryBlock renders one entry as the block holds it.
func entryBlock(e Entry, limit int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n### %s (%d of %d bytes)", e.Name, e.Size(), limit)
	if d := e.Description(); d != "" {
		b.WriteString(" — ")
		b.WriteString(d)
	}
	b.WriteString("\n\n")
	b.WriteString(e.Content)
	if !strings.HasSuffix(e.Content, "\n") {
		b.WriteString("\n")
	}
	return b.String()
}

// Usage is one paragraph telling the model how to use the memory tools
// over the block [Render] produced. A product appends it to its
// instructions when it offers [Tools].
func Usage() string {
	return "The Memory block is what you remember between sessions, rendered from the memory store at the start of each turn; it is not part of the conversation. Save a new fact with memory_save, giving a kebab-case name and a description in meta. Edit an existing entry with memory_patch, giving the exact old_text once and its replacement; prefer it to memory_save, which rewrites the entry whole. Remove an entry with memory_forget. Entries the block omits, and any entry you want to read whole, are reachable with memory_search; its results stay in the conversation, so search for what you need. Each entry's heading shows its size and the limit, and the block's header shows the budget; a write over the limit is refused with both numbers, so split or trim before you save. Use headings of level four or none inside an entry."
}
