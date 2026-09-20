// Package agentmemory is memory for Go agents over Open Responses: what
// the agent keeps between sessions about the user, the project and its
// own past work, in a scoped store with an append-only journal, changed
// only through tools and rendered into the instructions with a manifest
// for the session's provenance.
//
// An [Entry] is one remembered thing: a kebab-case name unique within a
// [Scope], Markdown content bounded by the store's limit, and metadata
// such as a description. A [Store] holds entries and a journal of every
// change; [NewMemStore] is the in-memory one, filestore the reference
// one a person can open, edit and commit, and sqlite the one with
// full-text search. [Tools] returns the four tools through which the
// model reaches a store, so every write is a function call in the
// transcript. [Render] turns the entries of the scopes a product names
// into the block it puts in agentturn's Config.Instructions, bounded in
// total, and the [Manifest] of what the block held.
//
// The package depends on openresponses, agenttool and the standard
// library. The agent loop is never imported; a product wires the block
// and the tools into its configuration.
package agentmemory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"iter"
	"strings"
	"time"
	"unicode/utf8"
)

// Scope says whose memory an entry is. Products define the values;
// "user", "project" and "session" are the conventional three. A scope
// has the same grammar as a name: see [ValidName].
type Scope string

// Entry is one remembered thing.
type Entry struct {
	Scope Scope `json:"scope"`
	// Name is kebab-case and unique within the scope.
	Name string `json:"name"`
	// Content is Markdown, bounded by the store's limit.
	Content string `json:"content"`
	// Meta is what the product and the model add beside the content:
	// the description the index and the block show, a type, anything
	// else. Keys are kebab-case; values are one line each.
	Meta map[string]string `json:"meta,omitempty"`
	// Hash is "sha256:" and the hex digest of Content. A store sets it.
	Hash string `json:"hash"`
	// Updated is when the store last wrote the entry. A store sets it.
	Updated time.Time `json:"updated"`
	// Deleted marks a tombstone in the journal; Content is then the
	// last content the entry held. A store never returns one from Get
	// or List.
	Deleted bool `json:"deleted,omitempty"`
}

// MetaDescription is the meta key the index and the rendered block
// show beside an entry.
const MetaDescription = "description"

// Description returns the entry's description, or "".
func (e Entry) Description() string { return e.Meta[MetaDescription] }

// Size returns the content's size in bytes, the number the bound
// applies to.
func (e Entry) Size() int { return len(e.Content) }

// Change is one journal record: the entry as it was after the change,
// what it replaced, and who made it.
type Change struct {
	// Seq is the record's position in the store's journal, monotonic
	// within one store and the cursor [Store.Journal] takes. It
	// promises nothing across stores.
	Seq uint64 `json:"seq"`
	// Entry is the entry after the change; a tombstone carries the
	// last content with Deleted set.
	Entry Entry `json:"entry"`
	// Prev is the hash of the content the change replaced, "" for a
	// create. A reader chains records through it and sees a fork where
	// two writers built on one predecessor.
	Prev string `json:"prev,omitempty"`
	// Session is the session that wrote the change, from
	// [WithSession], or "" for a person or an unattributed caller.
	Session string `json:"session,omitempty"`
	// At is when the change was made, for display and the record. It
	// never orders records; Seq does.
	At time.Time `json:"at"`
}

// Store holds entries and the journal of every change to them. Every
// implementation passes storetest.
type Store interface {
	// Get returns one live entry, or [ErrNotFound].
	Get(ctx context.Context, scope Scope, name string) (*Entry, error)
	// List returns the live entries of a scope by name. An unknown
	// scope lists nothing.
	List(ctx context.Context, scope Scope) ([]Entry, error)
	// Put creates or replaces the whole entry, content and meta, and
	// appends the new state to the journal. It refuses an invalid
	// entry with [ErrInvalid], content over [Store.MaxEntryBytes] with
	// [ErrTooLarge], and, under [IfHash], a stored hash other than the
	// one expected with [ErrConflict]. Hash and Updated on e are
	// ignored and set by the store.
	Put(ctx context.Context, e Entry, opts ...PutOption) error
	// Forget writes a tombstone: the entry leaves Get and List, and
	// the journal keeps its last content. A missing entry is
	// [ErrNotFound].
	Forget(ctx context.Context, scope Scope, name string) error
	// Search returns the live entries of the scopes that match query,
	// at most limit of them when limit is positive. What matches and
	// in what order is the store's; the reference stores use [Match]
	// and list by scope then name, and sqlite ranks by full-text
	// relevance.
	Search(ctx context.Context, scopes []Scope, query string, limit int) ([]Entry, error)
	// Journal yields every change with Seq greater than after, in
	// order; 0 reads from the beginning. A reader that stops early may
	// resume with the last Seq it saw.
	Journal(ctx context.Context, after uint64) iter.Seq2[Change, error]
	// MaxEntryBytes is the content size Put refuses to exceed.
	MaxEntryBytes() int
}

// DefaultMaxEntryBytes is the per-entry bound every store starts with.
const DefaultMaxEntryBytes = 4 << 10

// PutOption configures one Put.
type PutOption func(*PutOptions)

// PutOptions is the resolved form of a Put's options, for a Store to
// read through [ResolvePutOptions].
type PutOptions struct {
	// IfHash is the hash the stored entry must have when Conditional
	// is set, "" meaning the entry must not exist.
	IfHash      string
	Conditional bool
}

// IfHash makes the Put conditional: it fails with [ErrConflict] when
// the stored content hash is not h, so a write built on a stale read
// fails instead of clobbering. "" means the entry must not exist.
func IfHash(h string) PutOption {
	return func(o *PutOptions) {
		o.IfHash = h
		o.Conditional = true
	}
}

// ResolvePutOptions applies the options to a zero PutOptions.
func ResolvePutOptions(opts ...PutOption) PutOptions {
	var o PutOptions
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// Check reports an [ErrConflict] when the options are conditional and
// stored, the live entry or nil for none, does not satisfy them. A
// store calls it under its write lock, after reading the entry.
func (o PutOptions) Check(scope Scope, name string, stored *Entry) error {
	if !o.Conditional {
		return nil
	}
	have := ""
	if stored != nil {
		have = stored.Hash
	}
	if have != o.IfHash {
		return &ConflictError{Scope: scope, Name: name, Want: o.IfHash, Have: have}
	}
	return nil
}

// Errors a Store returns, each wrapped with the detail of the case.
var (
	// ErrNotFound is returned for an entry that is not live.
	ErrNotFound = errors.New("agentmemory: no such entry")
	// ErrInvalid is returned for a scope, name, content or meta that
	// breaks the rules in [CheckEntry].
	ErrInvalid = errors.New("agentmemory: invalid entry")
	// ErrTooLarge is returned for content over the store's bound; the
	// error is a [SizeError].
	ErrTooLarge = errors.New("agentmemory: entry over the size limit")
	// ErrConflict is returned by a conditional Put whose precondition
	// fails; the error is a [ConflictError].
	ErrConflict = errors.New("agentmemory: stored entry differs from the one expected")
)

// SizeError is the [ErrTooLarge] a Put returns, with the numbers the
// model needs to trim: the attempted size, the limit, and what the
// entry holds now.
type SizeError struct {
	Scope Scope
	Name  string
	// Size is the attempted content size and Limit the bound.
	Size, Limit int
	// Stored is the size the live entry holds, or -1 when there is
	// none.
	Stored int
}

func (e *SizeError) Error() string {
	msg := fmt.Sprintf("agentmemory: entry %s/%s is %d bytes, over the %d byte limit", e.Scope, e.Name, e.Size, e.Limit)
	if e.Stored >= 0 {
		msg += fmt.Sprintf("; it holds %d bytes now", e.Stored)
	}
	return msg + "; split it or trim it"
}

// Is reports [ErrTooLarge].
func (e *SizeError) Is(target error) bool { return target == ErrTooLarge }

// ConflictError is the [ErrConflict] a conditional Put returns: the
// hash the caller expected and the one the store holds, "" for no
// entry on either side.
type ConflictError struct {
	Scope      Scope
	Name       string
	Want, Have string
}

func (e *ConflictError) Error() string {
	switch {
	case e.Want == "":
		return fmt.Sprintf("agentmemory: entry %s/%s already exists with hash %s", e.Scope, e.Name, e.Have)
	case e.Have == "":
		return fmt.Sprintf("agentmemory: entry %s/%s no longer exists; expected hash %s", e.Scope, e.Name, e.Want)
	}
	return fmt.Sprintf("agentmemory: entry %s/%s has hash %s, not %s; read it again", e.Scope, e.Name, e.Have, e.Want)
}

// Is reports [ErrConflict].
func (e *ConflictError) Is(target error) bool { return target == ErrConflict }

// Hash returns "sha256:" and the hex digest of content, the form
// [Entry.Hash] and [Change.Prev] carry.
func Hash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Limits on names and metadata that every store enforces.
const (
	// MaxNameBytes bounds a scope or a name.
	MaxNameBytes = 64
	// MaxMetaBytes bounds the meta of one entry, keys and values
	// together, separately from the content bound.
	MaxMetaBytes = 1 << 10
)

// ValidName reports whether s is kebab-case: lowercase ASCII letters
// and digits in groups joined by single hyphens, between 1 and
// [MaxNameBytes] bytes. Scopes, names and meta keys all follow it, so
// a name is a file name, a directory name and a frontmatter key
// without escaping.
func ValidName(s string) bool {
	if s == "" || len(s) > MaxNameBytes {
		return false
	}
	prev := byte('-')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' && prev != '-':
		default:
			return false
		}
		prev = c
	}
	return prev != '-'
}

// ValidScope reports whether s is a well-formed scope: see [ValidName].
func ValidScope(s Scope) bool { return ValidName(string(s)) }

// reservedMetaKeys are the frontmatter fields the file store writes
// itself, so meta may not use them.
var reservedMetaKeys = map[string]bool{"name": true, "updated": true}

// CheckEntry reports what a store refuses: an invalid scope or name,
// empty content or content that is not UTF-8, a meta key that is not a
// name or is reserved ("name", "updated"), a meta value with a line
// break or surrounding space, meta over [MaxMetaBytes] together, each
// as [ErrInvalid]; and
// content over limit as a [SizeError], with stored's size when the
// entry exists. Every store calls it in Put, so the rules are the
// same everywhere.
func CheckEntry(e Entry, limit int, stored *Entry) error {
	if !ValidScope(e.Scope) {
		return fmt.Errorf("%w: scope %q is not kebab-case (lowercase letters, digits and single hyphens, up to %d bytes)", ErrInvalid, e.Scope, MaxNameBytes)
	}
	if !ValidName(e.Name) {
		return fmt.Errorf("%w: name %q is not kebab-case (lowercase letters, digits and single hyphens, up to %d bytes)", ErrInvalid, e.Name, MaxNameBytes)
	}
	if e.Content == "" {
		return fmt.Errorf("%w: %s/%s has no content; use forget to remove an entry", ErrInvalid, e.Scope, e.Name)
	}
	if !utf8.ValidString(e.Content) {
		return fmt.Errorf("%w: %s/%s content is not valid UTF-8", ErrInvalid, e.Scope, e.Name)
	}
	metaBytes := 0
	for k, v := range e.Meta {
		if !ValidName(k) {
			return fmt.Errorf("%w: %s/%s meta key %q is not kebab-case", ErrInvalid, e.Scope, e.Name, k)
		}
		if reservedMetaKeys[k] {
			return fmt.Errorf("%w: %s/%s meta key %q is reserved", ErrInvalid, e.Scope, e.Name, k)
		}
		if strings.ContainsAny(v, "\r\n") {
			return fmt.Errorf("%w: %s/%s meta %q spans lines; a value is one line", ErrInvalid, e.Scope, e.Name, k)
		}
		if strings.TrimSpace(v) != v {
			return fmt.Errorf("%w: %s/%s meta %q has leading or trailing space", ErrInvalid, e.Scope, e.Name, k)
		}
		if !utf8.ValidString(v) {
			return fmt.Errorf("%w: %s/%s meta %q is not valid UTF-8", ErrInvalid, e.Scope, e.Name, k)
		}
		metaBytes += len(k) + len(v)
	}
	if metaBytes > MaxMetaBytes {
		return fmt.Errorf("%w: %s/%s meta is %d bytes, over the %d byte limit", ErrInvalid, e.Scope, e.Name, metaBytes, MaxMetaBytes)
	}
	if limit > 0 && len(e.Content) > limit {
		se := &SizeError{Scope: e.Scope, Name: e.Name, Size: len(e.Content), Limit: limit, Stored: -1}
		if stored != nil {
			se.Stored = stored.Size()
		}
		return se
	}
	return nil
}

// Match reports whether e matches query as the reference stores
// search: every whitespace-separated word of query appears, ignoring
// case, in the entry's name, one of its meta values or its content.
// An empty query matches every entry.
func Match(e Entry, query string) bool {
	words := strings.Fields(strings.ToLower(query))
	if len(words) == 0 {
		return true
	}
	var b strings.Builder
	b.WriteString(e.Name)
	for _, v := range e.Meta {
		b.WriteByte('\n')
		b.WriteString(v)
	}
	b.WriteByte('\n')
	b.WriteString(e.Content)
	text := strings.ToLower(b.String())
	for _, w := range words {
		if !strings.Contains(text, w) {
			return false
		}
	}
	return true
}

type sessionKey struct{}

// WithSession attaches the session ID that writes through ctx, so a
// store records it on each change. One store serves many sessions at
// once, so the attribution travels on the context rather than the
// store; a product sets it on the context it runs the agent with.
func WithSession(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, sessionKey{}, id)
}

// SessionFrom returns the session ID attached to ctx, or "".
func SessionFrom(ctx context.Context) string {
	id, _ := ctx.Value(sessionKey{}).(string)
	return id
}
