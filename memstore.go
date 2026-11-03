package agentmemory

import (
	"context"
	"fmt"
	"iter"
	"maps"
	"slices"
	"sort"
	"sync"
	"time"
)

// MemStore is the in-memory [Store]: the reference for the contract,
// the store for tests, and a store for a product whose memory need
// not outlive the process. It is safe for concurrent use.
type MemStore struct {
	max int
	now func() time.Time

	mu      sync.Mutex
	live    map[Scope]map[string]Entry
	journal []Change
}

// MemOption configures [NewMemStore].
type MemOption func(*MemStore)

// WithMaxEntryBytes sets the content bound; the default is
// [DefaultMaxEntryBytes].
func WithMaxEntryBytes(n int) MemOption {
	return func(m *MemStore) { m.max = n }
}

// WithClock sets the clock that stamps Updated and At, for tests that
// want a fixed one.
func WithClock(now func() time.Time) MemOption {
	return func(m *MemStore) { m.now = now }
}

// NewMemStore returns an empty in-memory store.
func NewMemStore(opts ...MemOption) *MemStore {
	m := &MemStore{
		max:  DefaultMaxEntryBytes,
		now:  func() time.Time { return time.Now().UTC() },
		live: map[Scope]map[string]Entry{},
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// MaxEntryBytes implements Store.
func (m *MemStore) MaxEntryBytes() int { return m.max }

// Get implements Store.
func (m *MemStore) Get(_ context.Context, scope Scope, name string) (*Entry, error) {
	if err := CheckRef(scope, name); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.live[scope][name]
	if !ok {
		return nil, fmt.Errorf("%w: %s/%s", ErrNotFound, scope, name)
	}
	e = cloneEntry(e)
	return &e, nil
}

// List implements Store.
func (m *MemStore) List(_ context.Context, scope Scope) ([]Entry, error) {
	if err := CheckScope(scope); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.listLocked(scope), nil
}

func (m *MemStore) listLocked(scope Scope) []Entry {
	out := make([]Entry, 0, len(m.live[scope]))
	for _, e := range m.live[scope] {
		out = append(out, cloneEntry(e))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Put implements Store.
func (m *MemStore) Put(ctx context.Context, e Entry, opts ...PutOption) (*Change, error) {
	o := ResolvePutOptions(opts...)
	m.mu.Lock()
	defer m.mu.Unlock()
	var stored *Entry
	if cur, ok := m.live[e.Scope][e.Name]; ok {
		stored = &cur
	}
	if err := CheckEntry(e, m.max, stored); err != nil {
		return nil, err
	}
	if err := o.Check(e.Scope, e.Name, stored); err != nil {
		return nil, err
	}
	now := m.now()
	e = cloneEntry(e)
	e.Hash = Hash(e.Content)
	e.MetaHash = MetaHash(e.Meta)
	e.Updated = now
	e.Deleted = false
	if m.live[e.Scope] == nil {
		m.live[e.Scope] = map[string]Entry{}
	}
	m.live[e.Scope][e.Name] = e
	replaced := ""
	if stored != nil {
		replaced = stored.Hash
	}
	return m.appendLocked(ctx, e, o.BaseFor(stored), replaced, now), nil
}

// Forget implements Store.
func (m *MemStore) Forget(ctx context.Context, scope Scope, name string) (*Change, error) {
	if err := CheckRef(scope, name); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.live[scope][name]
	if !ok {
		return nil, fmt.Errorf("%w: %s/%s", ErrNotFound, scope, name)
	}
	delete(m.live[scope], name)
	now := m.now()
	e.Deleted = true
	e.Updated = now
	return m.appendLocked(ctx, e, e.Hash, e.Hash, now), nil
}

// appendLocked adds the record and returns the caller's own copy of it.
func (m *MemStore) appendLocked(ctx context.Context, e Entry, prev, replaced string, at time.Time) *Change {
	c := Change{
		Seq:      uint64(len(m.journal)) + 1,
		Entry:    cloneEntry(e),
		Prev:     prev,
		Replaced: replaced,
		Session:  SessionFrom(ctx),
		At:       at,
	}
	m.journal = append(m.journal, c)
	c.Entry = cloneEntry(c.Entry)
	return &c
}

// Search implements Store with [Match], listing by scope order then
// name.
func (m *MemStore) Search(ctx context.Context, scopes []Scope, query string, limit int) ([]Entry, error) {
	return m.SearchWith(ctx, scopes, query, limit, SearchOptions{})
}

// SearchOptions implements [Searcher]: every option.
func (m *MemStore) SearchOptions() []string { return AllSearchOptions() }

// SearchWith implements [Searcher] with [SearchOptions.Filter] over the
// scopes in order.
func (m *MemStore) SearchWith(_ context.Context, scopes []Scope, query string, limit int, opts SearchOptions) ([]Entry, error) {
	for _, scope := range scopes {
		if err := CheckScope(scope); err != nil {
			return nil, err
		}
	}
	if err := opts.Check(m.SearchOptions()); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var all []Entry
	for _, scope := range scopes {
		all = append(all, m.listLocked(scope)...)
	}
	return opts.Filter(all, query, limit), nil
}

// Journal implements Store.
func (m *MemStore) Journal(_ context.Context, after uint64) iter.Seq2[Change, error] {
	return func(yield func(Change, error) bool) {
		m.mu.Lock()
		var changes []Change
		if after < uint64(len(m.journal)) {
			changes = slices.Clone(m.journal[after:])
		}
		m.mu.Unlock()
		for _, c := range changes {
			c.Entry = cloneEntry(c.Entry)
			if !yield(c, nil) {
				return
			}
		}
	}
}

// LastSeq implements [LastSeqer]: the journal's length, since the
// in-memory journal is numbered from 1 with no gaps.
func (m *MemStore) LastSeq(context.Context) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return uint64(len(m.journal)), nil
}

// cloneEntry copies the entry so a caller's map and the store's stay
// separate. An empty meta becomes nil, so an entry compares equal
// whether it was stored with an empty map or none.
func cloneEntry(e Entry) Entry {
	if len(e.Meta) == 0 {
		e.Meta = nil
	} else {
		e.Meta = maps.Clone(e.Meta)
	}
	return e
}

var (
	_ Store     = (*MemStore)(nil)
	_ LastSeqer = (*MemStore)(nil)
	_ Searcher  = (*MemStore)(nil)
)
