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
func (m *MemStore) Put(ctx context.Context, e Entry, opts ...PutOption) error {
	o := ResolvePutOptions(opts...)
	m.mu.Lock()
	defer m.mu.Unlock()
	var stored *Entry
	if cur, ok := m.live[e.Scope][e.Name]; ok {
		stored = &cur
	}
	if err := CheckEntry(e, m.max, stored); err != nil {
		return err
	}
	if err := o.Check(e.Scope, e.Name, stored); err != nil {
		return err
	}
	now := m.now()
	e = cloneEntry(e)
	e.Hash = Hash(e.Content)
	e.Updated = now
	e.Deleted = false
	if m.live[e.Scope] == nil {
		m.live[e.Scope] = map[string]Entry{}
	}
	m.live[e.Scope][e.Name] = e
	prev := ""
	if stored != nil {
		prev = stored.Hash
	}
	m.appendLocked(ctx, e, prev, now)
	return nil
}

// Forget implements Store.
func (m *MemStore) Forget(ctx context.Context, scope Scope, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.live[scope][name]
	if !ok {
		return fmt.Errorf("%w: %s/%s", ErrNotFound, scope, name)
	}
	delete(m.live[scope], name)
	now := m.now()
	e.Deleted = true
	e.Updated = now
	m.appendLocked(ctx, e, e.Hash, now)
	return nil
}

func (m *MemStore) appendLocked(ctx context.Context, e Entry, prev string, at time.Time) {
	m.journal = append(m.journal, Change{
		Seq:     uint64(len(m.journal)) + 1,
		Entry:   cloneEntry(e),
		Prev:    prev,
		Session: SessionFrom(ctx),
		At:      at,
	})
}

// Search implements Store with [Match], listing by scope order then
// name.
func (m *MemStore) Search(_ context.Context, scopes []Scope, query string, limit int) ([]Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Entry
	for _, scope := range scopes {
		for _, e := range m.listLocked(scope) {
			if !Match(e, query) {
				continue
			}
			out = append(out, e)
			if limit > 0 && len(out) == limit {
				return out, nil
			}
		}
	}
	return out, nil
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

var _ Store = (*MemStore)(nil)
