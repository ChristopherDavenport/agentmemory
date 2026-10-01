package agentmemory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// DefaultMaxTotalBytes is the bound [Render] starts with: the whole
// block, summary and headings included.
const DefaultMaxTotalBytes = 32 << 10

// RenderOption configures [Render].
type RenderOption func(*renderOptions)

type renderOptions struct {
	maxTotal int
	scopeMax map[Scope]int
}

// WithMaxTotalBytes sets the block's bound; without it the bound is
// [DefaultMaxTotalBytes]. A bound under one holds no block, and
// [Render] refuses it with [ErrBudget] rather than reading it as the
// default: a caller dividing a budget among layers reaches zero or
// less exactly when there is no room.
func WithMaxTotalBytes(n int) RenderOption {
	return func(o *renderOptions) { o.maxTotal = n }
}

// WithScopeMaxBytes caps what one scope's entries may take of the
// block, inside its bound: the bytes the scope's entries and the line
// naming its omissions add, separators included, and not its heading,
// which the block holds whatever it shows. The scope packs against the
// smaller of its cap and what the scopes before it left, and an entry
// the cap leaves out is [OmitBudget], as one the bound leaves out is.
//
// A scope without a cap gets what is left, so with one bound for every
// scope the scope rendered last gets what the others did not take, and
// a full scope early in the order starves it. Capping the early scope,
// or each, keeps room for the later ones; a cap of zero shows none of
// the scope's entries and names them, room permitting. A cap under
// zero is refused with [ErrBudget]. A cap on a scope the render does
// not list does nothing, and the last cap given for a scope is the one
// that holds.
func WithScopeMaxBytes(scope Scope, n int) RenderOption {
	return func(o *renderOptions) {
		if o.scopeMax == nil {
			o.scopeMax = map[Scope]int{}
		}
		o.scopeMax[scope] = n
	}
}

// ErrBudget is returned by [Render] and [RenderParts] for a bound the
// block cannot meet: one under one byte, or one under what the block
// holds whatever it shows, its title, its summary and a heading per
// scope; or for a scope cap under zero. The block is never returned over its bound; a product that
// gets ErrBudget has no room for memory on this call and leaves the
// block out.
var ErrBudget = errors.New("agentmemory: render bound is too small for the block")

// ManifestNS is the namespace a [Manifest] is recorded under, so a
// reader of a session recognises one without knowing the product that
// wrote it. See [Manifest.Record].
//
// A record under it is a whole manifest, from [Manifest.Record], or a
// delta, from [Manifest.RecordSince], whose base is the hash of a
// manifest earlier on the path: the one in force, or, since v0.0.9,
// any of the last [ManifestFoldDepth] distinct manifests in force. A
// reader before v0.0.9 folds with [ApplyManifestRecord] and refuses a
// delta of the second kind, so a writer records one only once the
// session's readers fold with [ManifestFold].
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
//
// A session that already holds a manifest records the next one with
// [Manifest.RecordSince], which writes what moved rather than every
// entry again.
func (m Manifest) Record() (ns string, data []byte) {
	return ManifestNS, marshalRecord(m)
}

// RecordSince returns the namespace and the JSON of the manifest as a
// delta on prev, the last manifest the product recorded in this
// session: a write moves one entry's hash, and under a memory at its
// bound the whole record repeats hundreds of entries that did not move
// to say so.
//
// The delta is an object with base, prev's [Manifest.Hash]; hash, this
// manifest's; and entries and omitted, each the list with every run of
// prev's entries that stays in place and unchanged written as
// {"keep":n}, and every other entry written whole. An entry prev holds
// and this manifest does not is neither kept nor written. A reader
// folds it onto the manifest in force with [ApplyManifestRecord]; the
// member base is what tells it from a whole record.
//
// It returns the whole record, as [Manifest.Record] does, when that is
// no larger than the delta, which it is for a first manifest, and when
// either manifest names one entry twice in a list, since a keep could
// not say which of the two it means.
//
// The product keeps prev per session, and records a session's first
// manifest whole. prev is the manifest in force on the session's path,
// which every reader resolves, or, where more than one agent records
// into the session, the agent's own last manifest when it is among the
// last [ManifestFoldDepth] distinct manifests in force and the delta on
// it is the smaller: a reader resolves that base with a
// [ManifestFold], and [ApplyManifestRecord] refuses it.
func (m Manifest) RecordSince(prev Manifest) (ns string, data []byte) {
	whole := marshalRecord(m)
	entries, ok1 := manifestDiff(prev.Entries, m.Entries)
	omitted, ok2 := manifestDiff(prev.Omitted, m.Omitted)
	if !ok1 || !ok2 {
		return ManifestNS, whole
	}
	delta := marshalRecord(manifestDelta{Base: prev.Hash(), Hash: m.Hash(), Entries: entries, Omitted: omitted})
	if len(delta) >= len(whole) {
		return ManifestNS, whole
	}
	return ManifestNS, delta
}

// ErrManifestBase is returned by [ApplyManifestRecord] for a delta
// whose base is not the manifest it was given, and by
// [ManifestFold.Apply] for one whose base is none the fold holds: the
// reader missed a record, or the delta belongs to another session.
var ErrManifestBase = errors.New("agentmemory: manifest delta is not based on a manifest the reader holds")

// ApplyManifestRecord returns the manifest in force after the record
// data, the bytes of an entry under [ManifestNS], given m, the one in
// force before it. A whole record, from [Manifest.Record], is the
// manifest it holds, whatever m is. A delta, from
// [Manifest.RecordSince], is folded onto m: one whose base is not
// m's hash is refused with [ErrManifestBase], and one whose keeps run
// past m, or whose result does not hash as the delta says, is refused
// as malformed.
//
// A delta may be based on an earlier manifest on the path than the one
// in force, as a writer handed back to after a handoff records one; a
// reader of a session more than one agent records into folds with a
// [ManifestFold], which resolves those.
func ApplyManifestRecord(m Manifest, data []byte) (Manifest, error) {
	whole, d, err := parseManifestRecord(data)
	if err != nil {
		return Manifest{}, err
	}
	if d == nil {
		return whole, nil
	}
	if have := m.Hash(); d.Base != have {
		return Manifest{}, fmt.Errorf("%w: the delta is based on %s, the manifest in force is %s", ErrManifestBase, d.Base, have)
	}
	return applyManifestDelta(m, d)
}

// ManifestFoldDepth is how many distinct manifests a [ManifestFold]
// resolves a delta's base against: the one in force and those in force
// before it, most recent first. A writer records a delta on a manifest
// other than the one in force only when it is among the last
// ManifestFoldDepth distinct manifests on the session's path, and
// otherwise on the one in force.
const ManifestFoldDepth = 8

// ManifestFold reads the records under [ManifestNS] on a session's
// path, in order, and keeps the manifest in force. Unlike
// [ApplyManifestRecord] it resolves a delta whose base is any of the
// last [ManifestFoldDepth] distinct manifests in force, not only the
// current one: when two agents with their own memories take turns in
// one session, each records on its own last manifest rather than the
// other agent's, which shares nothing with it, and writes what moved
// rather than every entry at each hand-back.
//
// The zero value is ready to use and holds the empty manifest. A fold
// is not safe for concurrent use.
type ManifestFold struct {
	// recent are the distinct manifests in force, most recent first,
	// the one in force at 0, and hashes their hashes.
	recent []Manifest
	hashes []string
}

// Apply folds the record data, the bytes of an entry under
// [ManifestNS]. A whole record becomes the manifest in force. A delta
// whose base is none of the manifests the fold holds is refused with
// [ErrManifestBase], and a malformed one as [ApplyManifestRecord]
// refuses it; a refused record leaves the fold as it was.
func (f *ManifestFold) Apply(data []byte) error {
	whole, d, err := parseManifestRecord(data)
	if err != nil {
		return err
	}
	if d == nil {
		f.push(whole, whole.Hash())
		return nil
	}
	i := slices.Index(f.hashes, d.Base)
	var base Manifest
	switch {
	case i >= 0:
		base = f.recent[i]
	case len(f.recent) == 0 && d.Base == (Manifest{}).Hash():
		// Nothing folded yet: the empty manifest is in force.
	default:
		return fmt.Errorf("%w: the delta is based on %s, which is not among the %d manifests last in force", ErrManifestBase, d.Base, len(f.recent))
	}
	out, err := applyManifestDelta(base, d)
	if err != nil {
		return err
	}
	f.push(out, d.Hash)
	return nil
}

// Manifest returns the manifest in force: the empty one before any
// record is folded.
func (f *ManifestFold) Manifest() Manifest {
	if len(f.recent) == 0 {
		return Manifest{}
	}
	m := f.recent[0]
	return Manifest{Entries: slices.Clone(m.Entries), Omitted: slices.Clone(m.Omitted)}
}

// push makes m, whose hash is h, the manifest in force, moving it to
// the front if the fold holds it and dropping the oldest past the
// depth.
func (f *ManifestFold) push(m Manifest, h string) {
	if i := slices.Index(f.hashes, h); i >= 0 {
		f.recent = slices.Delete(f.recent, i, i+1)
		f.hashes = slices.Delete(f.hashes, i, i+1)
	}
	f.recent = slices.Insert(f.recent, 0, m)
	f.hashes = slices.Insert(f.hashes, 0, h)
	if len(f.recent) > ManifestFoldDepth {
		f.recent = f.recent[:ManifestFoldDepth]
		f.hashes = f.hashes[:ManifestFoldDepth]
	}
}

// parseManifestRecord decodes a record under [ManifestNS]: a whole
// manifest, with a nil delta, or a delta.
func parseManifestRecord(data []byte) (Manifest, *manifestDelta, error) {
	var probe struct {
		Base *string `json:"base"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return Manifest{}, nil, fmt.Errorf("agentmemory: manifest record: %w", err)
	}
	if probe.Base == nil {
		var whole Manifest
		if err := json.Unmarshal(data, &whole); err != nil {
			return Manifest{}, nil, fmt.Errorf("agentmemory: manifest record: %w", err)
		}
		return whole, nil, nil
	}
	var d manifestDelta
	if err := json.Unmarshal(data, &d); err != nil {
		return Manifest{}, nil, fmt.Errorf("agentmemory: manifest delta: %w", err)
	}
	return Manifest{}, &d, nil
}

// applyManifestDelta folds d onto m, whose hash is d's base, and checks
// the result against d's hash.
func applyManifestDelta(m Manifest, d *manifestDelta) (Manifest, error) {
	var out Manifest
	var err error
	if out.Entries, err = manifestApply(m.Entries, d.Entries); err != nil {
		return Manifest{}, fmt.Errorf("agentmemory: manifest delta entries: %w", err)
	}
	if out.Omitted, err = manifestApply(m.Omitted, d.Omitted); err != nil {
		return Manifest{}, fmt.Errorf("agentmemory: manifest delta omitted: %w", err)
	}
	if got := out.Hash(); got != d.Hash {
		return Manifest{}, fmt.Errorf("agentmemory: manifest delta: the result hashes %s, the delta says %s", got, d.Hash)
	}
	return out, nil
}

// manifestDelta is the delta form of the record; see
// [Manifest.RecordSince].
type manifestDelta struct {
	Base    string       `json:"base"`
	Hash    string       `json:"hash"`
	Entries []manifestOp `json:"entries,omitempty"`
	Omitted []manifestOp `json:"omitted,omitempty"`
}

// manifestOp is one element of a delta's list: a keep of the next Keep
// entries in force, alone, or an entry written whole.
type manifestOp struct {
	Keep int `json:"keep,omitempty"`
	*ManifestEntry
}

type manifestKey struct {
	scope Scope
	name  string
}

// manifestIndex maps each entry of list to its position, and reports
// false when an entry is named twice.
func manifestIndex(list []ManifestEntry) (map[manifestKey]int, bool) {
	at := make(map[manifestKey]int, len(list))
	for i, e := range list {
		k := manifestKey{e.Scope, e.Name}
		if _, dup := at[k]; dup {
			return nil, false
		}
		at[k] = i
	}
	return at, true
}

// manifestDiff writes next as a delta on prev: a run of entries that
// stand, unchanged, at the cursor in prev is a keep, and anything else
// is written whole and moves the cursor past its place in prev, if it
// has one. [manifestApply] reads it with the same cursor. It reports
// false when either list names an entry twice.
func manifestDiff(prev, next []ManifestEntry) ([]manifestOp, bool) {
	at, ok := manifestIndex(prev)
	if !ok {
		return nil, false
	}
	if _, ok := manifestIndex(next); !ok {
		return nil, false
	}
	var out []manifestOp
	cursor, run := 0, 0
	flush := func() {
		if run > 0 {
			out = append(out, manifestOp{Keep: run})
			cursor += run
			run = 0
		}
	}
	for _, e := range next {
		j, ok := at[manifestKey{e.Scope, e.Name}]
		if ok && prev[j] == e && j == cursor+run {
			run++
			continue
		}
		flush()
		out = append(out, manifestOp{ManifestEntry: &e})
		if ok {
			cursor = j + 1
		}
	}
	flush()
	return out, true
}

// manifestApply folds ops onto prev, as [manifestDiff] wrote them.
func manifestApply(prev []ManifestEntry, ops []manifestOp) ([]ManifestEntry, error) {
	at, ok := manifestIndex(prev)
	if !ok {
		return nil, errors.New("the manifest in force names an entry twice")
	}
	var out []ManifestEntry
	cursor := 0
	for i, op := range ops {
		switch {
		case op.ManifestEntry != nil && op.Keep != 0:
			return nil, fmt.Errorf("element %d is both a keep and an entry", i)
		case op.ManifestEntry != nil:
			e := *op.ManifestEntry
			if e.Scope == "" || e.Name == "" {
				return nil, fmt.Errorf("element %d names no entry", i)
			}
			out = append(out, e)
			if j, ok := at[manifestKey{e.Scope, e.Name}]; ok {
				cursor = j + 1
			}
		case op.Keep > 0 && op.Keep <= len(prev)-cursor:
			out = append(out, prev[cursor:cursor+op.Keep]...)
			cursor += op.Keep
		case op.Keep > 0:
			return nil, fmt.Errorf("element %d keeps %d entries and %d are left", i, op.Keep, len(prev)-cursor)
		default:
			return nil, fmt.Errorf("element %d is neither a positive keep nor an entry", i)
		}
	}
	return out, nil
}

// marshalRecord encodes a record, which is strings and numbers and so
// cannot fail to encode.
func marshalRecord(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		// Unreachable: every field is a string, an int or a slice of
		// them. Recording something a reader can see went wrong beats
		// recording nothing.
		data = fmt.Appendf(nil, "{%q:%q}", "error", err.Error())
	}
	return data
}

// PartSeparator joins the parts [RenderParts] returns into the block
// [Render] returns: one blank line. It is agentsession's PartSeparator,
// the rule a session's instructions parts are joined by, stated here
// because this module does not import the session format; a product
// that records the block's parts as instructions parts, beside parts of
// its own, gets from the format's join exactly the string Render gives.
const PartSeparator = "\n\n"

// Part is one piece of the rendered block, with an ID that is the same
// across renders for as long as the piece exists, so a product that
// records its instructions as parts records a write as a change to the
// entry it touched and names the others by hash. [RenderParts] returns
// the block as parts, in order, and [JoinParts] gives back the block.
type Part struct {
	// ID names the piece: [TitlePartID] and [SummaryPartID] for the
	// block's first and last lines, "memory/<scope>" for a scope's
	// heading, [PartID] for an entry, and "memory/<scope>:omitted" for
	// the line naming what a scope left out. Scopes and names are
	// kebab-case, so no two pieces of one block can share an ID.
	ID string `json:"id"`
	// Text is the piece, without the blank line that separates it from
	// the next.
	Text string `json:"text"`
}

// The IDs of the block's fixed parts.
const (
	// TitlePartID is the block's first part, its title, which no write
	// changes. Every ID starts "memory", so a product that records its
	// own parts beside these gives its own some other prefix.
	TitlePartID = "memory"
	// SummaryPartID is the block's last part: the counts, the block's
	// size and the entry limit, which every write changes.
	SummaryPartID = "memory:summary"
)

// PartID is the ID of the part that holds an entry, "memory/" and the
// scope and the name, and the ID a product gives an omitted entry of
// the [Manifest] when it records what the block left out beside the
// parts it holds.
func PartID(scope Scope, name string) string { return "memory/" + string(scope) + "/" + name }

func scopePartID(scope Scope) string   { return "memory/" + string(scope) }
func omittedPartID(scope Scope) string { return "memory/" + string(scope) + ":omitted" }

// JoinParts returns the block the parts compose: their texts joined
// with [PartSeparator], in order.
func JoinParts(parts []Part) string {
	texts := make([]string, 0, len(parts))
	for _, p := range parts {
		texts = append(texts, p.Text)
	}
	return strings.Join(texts, PartSeparator)
}

// Render returns the in-context block: [RenderParts] joined with
// [PartSeparator]. A product that records its instructions as one
// string uses it; one that records parts uses RenderParts.
func Render(ctx context.Context, s Store, scopes []Scope, opts ...RenderOption) (string, Manifest, error) {
	parts, man, err := RenderParts(ctx, s, scopes, opts...)
	if err != nil {
		return "", Manifest{}, err
	}
	return JoinParts(parts), man, nil
}

// RenderParts returns the in-context block as its parts, and the
// [Manifest] of what it holds. In order: the title; for each scope in
// the order given, its heading, then each entry that fits in list
// order, its heading carrying its size, the store's limit and its
// description, then its content; after a scope's entries, the line
// naming the ones left out; and last, the summary: the counts, the
// block's own size against the bound, and the entry limit.
//
// The summary comes last because it is the one part every write
// changes, and the block is the instructions, the request's prefix,
// which every prompt cache in use caches by prefix: a write keeps every
// part before the entry it touched in the cached prefix. Nothing in a
// part depends on whether it is last.
//
// The bound is on the joined block, not on the content it holds: the
// title, the summary, the scope headings, the per-entry headings with
// their descriptions, the list of what was left out and the separators
// are all counted, because they are all sent to the model, and the
// summary reports the block's own size so what the model reads is what
// the window pays. An entry whose part does not fit in what is left of
// the bound, or of its scope's cap under [WithScopeMaxBytes], is skipped,
// recorded in [Manifest.Omitted] with [OmitBudget] and listed by name
// after its scope's entries so the model knows what memory_search can
// fetch; entries after it are still considered, so one large entry
// cannot hide the small ones that sort after it. Order is never
// changed: the block holds the entries in list order, whichever were
// skipped.
//
// The output is determined by the store's state and the bounds alone,
// so an unchanged store renders the same parts and costs nothing in the
// session, and the manifest's hashes are the hashes of the content the
// block shows. A product records the manifest when [Manifest.Hash]
// differs from the last one it recorded, under [ManifestNS]; see
// [Manifest.Record]. The summary's own width is reserved before the
// entries are placed, at the widest the counts could be, so a block can
// come out a few bytes under the bound; it never comes out over it. The
// title, the summary and one heading per scope are always written, so
// a bound too small for those, or under one, is refused with
// [ErrBudget] and no block.
//
// Content is not transformed, except that one trailing newline is
// dropped, since the separator after the part supplies it. A heading
// inside an entry's content at level one to three would read as
// structure, so the tool descriptions ask the model for level four or
// none.
func RenderParts(ctx context.Context, s Store, scopes []Scope, opts ...RenderOption) ([]Part, Manifest, error) {
	o := renderOptions{maxTotal: DefaultMaxTotalBytes}
	for _, opt := range opts {
		opt(&o)
	}
	if o.maxTotal < 1 {
		return nil, Manifest{}, fmt.Errorf("%w: the bound is %d bytes", ErrBudget, o.maxTotal)
	}
	for scope, n := range o.scopeMax {
		if n < 0 {
			return nil, Manifest{}, fmt.Errorf("%w: scope %q is capped at %d bytes", ErrBudget, scope, n)
		}
	}
	for i, scope := range scopes {
		if !ValidScope(scope) {
			return nil, Manifest{}, fmt.Errorf("%w: scope %q is not kebab-case", ErrInvalid, scope)
		}
		// A scope rendered twice would name its parts twice, which a
		// session refuses.
		if slices.Contains(scopes[:i], scope) {
			return nil, Manifest{}, fmt.Errorf("%w: scope %q is given twice", ErrInvalid, scope)
		}
	}
	limit := s.MaxEntryBytes()
	lists := make([][]Entry, len(scopes))
	total := 0
	for i, scope := range scopes {
		es, err := s.List(ctx, scope)
		if err != nil {
			return nil, Manifest{}, err
		}
		lists[i] = es
		total += len(es)
	}
	// What the block holds whatever it shows: the title, the summary at
	// the widest its numbers can be, since it is written last and
	// reports the block's size, and one heading per scope. Taking it off
	// the bound before anything else means the entries of an early scope
	// cannot spend what a later scope's heading needs.
	fixed := len(title) + cost(summary(total, total, o.maxTotal, o.maxTotal, o.maxTotal, limit))
	for i, scope := range scopes {
		fixed += cost(scopeText(scope, len(lists[i]) == 0))
	}
	if fixed > o.maxTotal {
		return nil, Manifest{}, fmt.Errorf("%w: the title, the summary and %d scope headings take %d bytes, the bound is %d", ErrBudget, len(scopes), fixed, o.maxTotal)
	}

	man := Manifest{Entries: []ManifestEntry{}}
	parts := []Part{{ID: TitlePartID, Text: title}}
	spent := 0 // the entries and the omission lines placed so far
	for i, scope := range scopes {
		es := lists[i]
		parts = append(parts, Part{ID: scopePartID(scope), Text: scopeText(scope, len(es) == 0)})
		if len(es) == 0 {
			continue
		}
		room := o.maxTotal - fixed - spent
		if n, ok := o.scopeMax[scope]; ok {
			room = min(room, n)
		}
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
				parts = append(parts, Part{ID: PartID(scope, e.Name), Text: entryText(e, limit)})
				man.Entries = append(man.Entries, me)
				continue
			}
			me.Reason = OmitBudget
			man.Omitted = append(man.Omitted, me)
			dropped = append(dropped, e)
		}
		if len(dropped) > 0 {
			if line := omitLine(dropped, room-used); line != "" {
				parts = append(parts, Part{ID: omittedPartID(scope), Text: line})
				used += cost(line)
			}
		}
		spent += used
	}
	// The summary reports the block's own size, so settling it is a
	// fixed point: the reservation above is as wide as the summary can
	// be, and writing the size into it narrows it, which narrows the
	// size, so the passes below descend to the width the summary keeps.
	// Only the size's own digits move that width, because the free
	// count is written at the width of the bound, so each pass is no
	// wider than the one before and a few of them reach the width that
	// holds. Every pass is inside the bound, since none is wider than
	// the reservation.
	shown, left := len(man.Entries), len(man.Omitted)
	rest := len(JoinParts(parts)) + len(PartSeparator) // all but the summary's text
	size := fixed + spent
	last := ""
	for range 8 {
		last = summary(shown, left, size, o.maxTotal-size, o.maxTotal, limit)
		n := rest + len(last)
		if n == size {
			break
		}
		size = n
	}
	parts = append(parts, Part{ID: SummaryPartID, Text: last})
	return parts, man, nil
}

const (
	title     = "# Memory"
	noEntries = "No entries."
	omitHead  = "Not shown, over the block budget; fetch with memory_search: "
	omitCount = "Not shown, over the block budget: %d entries."
	moreTail  = ", and %d more"
)

// cost is what a part other than the first adds to the block: its text
// and the separator before it.
func cost(text string) int { return len(PartSeparator) + len(text) }

// scopeText is a scope's heading part, which says so when the scope
// has no entries.
func scopeText(scope Scope, empty bool) string {
	if empty {
		return "## " + string(scope) + PartSeparator + noEntries
	}
	return "## " + string(scope)
}

// pack decides which of es the block has room for, in list order,
// skipping one that does not fit and going on to the next, so a large
// entry cannot hide the small ones after it. It returns the decision
// per entry and the bytes they take.
func pack(es []Entry, limit, room int) ([]bool, int) {
	in := make([]bool, len(es))
	used := 0
	for i, e := range es {
		n := cost(entryText(e, limit))
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
	return cost(omitHead) + item + len(fmt.Sprintf(moreTail, len(es)))
}

func omitItem(e Entry) string { return fmt.Sprintf("%s (%d bytes)", e.Name, e.Size()) }

// omitLine names the entries the block left out, so the model knows
// what memory_search can fetch, within the room left for it, separator
// included: the names that fit, then how many more there are. With room
// for neither it gives the count alone, and with room for nothing it
// gives nothing; the manifest carries every omission either way.
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
		if cost(omitHead)+b.Len()+len(piece)+tail > room {
			break
		}
		b.WriteString(piece)
		listed++
	}
	if listed == 0 {
		if short := fmt.Sprintf(omitCount, len(dropped)); cost(short) <= room {
			return short
		}
		return ""
	}
	out := omitHead + b.String()
	if rest := len(dropped) - listed; rest > 0 {
		out += fmt.Sprintf(moreTail, rest)
	}
	return out
}

// summary is the block's last line: what it holds and what it cost.
//
// The free count is written at the width of the bound, padded with
// spaces, so that the summary's own width depends on the size it
// reports and on nothing else. Let it shrink as the size shrinks and
// the two swap at a power of ten: the summary is then wider for the
// smaller size than for the larger, no width is a fixed point of the
// size it states, and the block reports a length it does not have.
func summary(shown, omitted, size, free, max, limit int) string {
	return fmt.Sprintf("Entries: %d shown, %d omitted. Block: %d of %d bytes (%*d free). Entry limit: %d bytes.",
		shown, omitted, size, max, len(strconv.Itoa(max)), free, limit)
}

// entryText renders one entry's part: its heading, a blank line, and
// its content less one trailing newline.
func entryText(e Entry, limit int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "### %s (%d of %d bytes)", e.Name, e.Size(), limit)
	if d := e.Description(); d != "" {
		b.WriteString(" — ")
		b.WriteString(d)
	}
	b.WriteString("\n\n")
	b.WriteString(strings.TrimSuffix(e.Content, "\n"))
	return b.String()
}

// Usage is one paragraph telling the model how to use the memory tools
// over the block [Render] produced. A product appends it to its
// instructions when it offers [Tools].
func Usage() string {
	return "The Memory block is what you remember between sessions, rendered from the memory store at the start of each turn; it is not part of the conversation. Save a new fact with memory_save, giving a kebab-case name and a description in meta. Edit an existing entry with memory_patch, giving the exact old_text once and its replacement; prefer it to memory_save, which rewrites the entry whole. Remove an entry with memory_forget. Entries the block omits, and any entry you want to read whole, are reachable with memory_search; its results stay in the conversation, so search for what you need. Each entry's heading shows its size and the limit, and the block's last line shows the budget; a write over the limit is refused with both numbers, so split or trim before you save. Use headings of level four or none inside an entry."
}
