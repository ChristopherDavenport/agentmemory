package agentmemory

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// The search options a store may support beside the query, by the
// names [Searcher.SearchOptions] reports and memory_search offers.
const (
	// SearchAny matches an entry holding any word of the query rather
	// than every word, ranked by the store.
	SearchAny = "any"
	// SearchMeta matches an entry whose metadata holds every given key
	// with the given value.
	SearchMeta = "meta"
	// SearchUpdatedAfter matches an entry last written after a time,
	// exclusive.
	SearchUpdatedAfter = "updated_after"
	// SearchUpdatedBefore matches an entry last written before a time,
	// exclusive.
	SearchUpdatedBefore = "updated_before"
)

// ErrUnsupported is returned for a search option the store does not
// support: a store refuses one rather than ignore it, so a caller never
// takes an unfiltered result for a filtered one.
var ErrUnsupported = errors.New("agentmemory: search option not supported")

// allSearchOptions are the options the reference search supports.
var allSearchOptions = []string{SearchAny, SearchMeta, SearchUpdatedAfter, SearchUpdatedBefore}

// AllSearchOptions returns every option's name, for a store that
// supports them all, as the reference stores do.
func AllSearchOptions() []string { return slices.Clone(allSearchOptions) }

// SearchOptions are the options of one search beside its query. The
// zero value is a plain search, every word of the query, as
// [Store.Search] runs it.
type SearchOptions struct {
	// Any matches an entry holding any word of the query, ranked by the
	// store, rather than every word.
	Any bool
	// Meta keeps the entries whose metadata holds every key with the
	// value given, exactly. Keys are names.
	Meta map[string]string
	// UpdatedAfter and UpdatedBefore keep the entries whose Updated is
	// after, or before, the time, exclusive; the zero time is no bound.
	UpdatedAfter, UpdatedBefore time.Time
}

// Names returns the names of the options o sets, in the order of the
// constants.
func (o SearchOptions) Names() []string {
	var out []string
	if o.Any {
		out = append(out, SearchAny)
	}
	if o.Meta != nil {
		out = append(out, SearchMeta)
	}
	if !o.UpdatedAfter.IsZero() {
		out = append(out, SearchUpdatedAfter)
	}
	if !o.UpdatedBefore.IsZero() {
		out = append(out, SearchUpdatedBefore)
	}
	return out
}

// Check refuses options a store does not support, given the names it
// supports, with [ErrUnsupported], and a meta key that is not a name
// with [ErrInvalid].
func (o SearchOptions) Check(supported []string) error {
	for _, n := range o.Names() {
		if !slices.Contains(supported, n) {
			return fmt.Errorf("%w: %s", ErrUnsupported, n)
		}
	}
	for k := range o.Meta {
		if !ValidName(k) {
			return fmt.Errorf("%w: meta key %q is not kebab-case", ErrInvalid, k)
		}
	}
	return nil
}

// Keep reports whether e passes the options' filters, meta and the
// updated bounds; the query is the caller's.
func (o SearchOptions) Keep(e Entry) bool {
	for k, v := range o.Meta {
		if have, ok := e.Meta[k]; !ok || have != v {
			return false
		}
	}
	if !o.UpdatedAfter.IsZero() && !e.Updated.After(o.UpdatedAfter) {
		return false
	}
	if !o.UpdatedBefore.IsZero() && !e.Updated.Before(o.UpdatedBefore) {
		return false
	}
	return true
}

// Filter is the reference search over entries, listed in the order a
// plain search lists them: those that pass the options' filters and
// match query by [Match], or, under Any, hold at least one of its words,
// ranked by how many distinct words they hold, most first, ties in the
// order given; at most limit of them when limit is positive. The
// in-memory store and filestore search with it.
func (o SearchOptions) Filter(entries []Entry, query string, limit int) []Entry {
	words := queryWords(query)
	type scored struct {
		e Entry
		n int
	}
	var hits []scored
	for _, e := range entries {
		if !o.Keep(e) {
			continue
		}
		n := matchCount(e, words)
		switch {
		case len(words) == 0:
		case o.Any && n == 0, !o.Any && n < len(words):
			continue
		}
		hits = append(hits, scored{e, n})
		if !o.Any && limit > 0 && len(hits) == limit {
			break
		}
	}
	if o.Any {
		slices.SortStableFunc(hits, func(a, b scored) int { return b.n - a.n })
	}
	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]Entry, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.e)
	}
	return out
}

// Searcher is implemented by a store that supports search options. A
// caller searches with options through [SearchWith], which refuses
// them for a store that is not a Searcher.
type Searcher interface {
	// SearchOptions names the options SearchWith supports, from the
	// Search constants.
	SearchOptions() []string
	// SearchWith is [Store.Search] with options. It refuses an option
	// the store does not support with [ErrUnsupported], rather than
	// ignore it.
	SearchWith(ctx context.Context, scopes []Scope, query string, limit int, opts SearchOptions) ([]Entry, error)
}

// SearchSupport returns the options s supports: [Searcher.SearchOptions]
// for a Searcher, none for another store.
func SearchSupport(s Store) []string {
	if ss, ok := s.(Searcher); ok {
		return ss.SearchOptions()
	}
	return nil
}

// SearchWith searches s with options: through [Searcher.SearchWith]
// when s is a Searcher, and otherwise through [Store.Search] when opts
// sets none, refusing any it sets with [ErrUnsupported].
func SearchWith(ctx context.Context, s Store, scopes []Scope, query string, limit int, opts SearchOptions) ([]Entry, error) {
	if ss, ok := s.(Searcher); ok {
		return ss.SearchWith(ctx, scopes, query, limit, opts)
	}
	if err := opts.Check(nil); err != nil {
		return nil, err
	}
	return s.Search(ctx, scopes, query, limit)
}

// queryWords splits a query as [Match] does: at white space,
// lowercased, each distinct word once.
func queryWords(query string) []string {
	var out []string
	for _, w := range strings.Fields(strings.ToLower(query)) {
		if !slices.Contains(out, w) {
			out = append(out, w)
		}
	}
	return out
}

// matchCount returns how many distinct words appear in e's name, one
// of its metadata values or its content, ignoring case.
func matchCount(e Entry, words []string) int {
	if len(words) == 0 {
		return 0
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
	n := 0
	for _, w := range words {
		if strings.Contains(text, w) {
			n++
		}
	}
	return n
}
