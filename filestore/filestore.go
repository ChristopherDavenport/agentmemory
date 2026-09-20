// Package filestore is the reference [agentmemory.Store]: one
// directory per scope, one Markdown file per entry with a frontmatter,
// an INDEX.md per scope listing every live entry, and one append-only
// journal.jsonl at the root. A person can open, edit, diff and commit
// the directory; git shows what the agent remembered and when.
//
//	memory/
//	  journal.jsonl
//	  user/
//	    INDEX.md
//	    style.md
//	  project/
//	    INDEX.md
//	    build.md
//
// Writes are atomic (write, fsync, rename) and serialised by one lock
// file at the root that names its holder, is taken over when its
// holder is dead, and is waited for only so long. Reads take no lock:
// a rename is atomic, so a reader sees an entry before or after a
// write, never torn.
//
// The package imports agentmemory and the standard library only.
package filestore

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"iter"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ChristopherDavenport/agentmemory"
)

// Store is a directory-backed [agentmemory.Store]. Several handles on
// one directory, in one process or many, share the store; the lock
// serialises their writes.
type Store struct {
	dir         string
	max         int
	lockTimeout time.Duration
	now         func() time.Time

	// mu serialises this process's writers before the lock file does,
	// so two handles in one process do not spin on each other.
	mu sync.Mutex
}

// Option configures [Open].
type Option func(*Store)

// WithMaxEntryBytes sets the content bound; the default is
// [agentmemory.DefaultMaxEntryBytes].
func WithMaxEntryBytes(n int) Option {
	return func(s *Store) { s.max = n }
}

// WithLockTimeout sets how long a write waits for a live holder of the
// lock; the default is [DefaultLockTimeout].
func WithLockTimeout(d time.Duration) Option {
	return func(s *Store) { s.lockTimeout = d }
}

// WithClock sets the clock that stamps Updated and At, for tests that
// want a fixed one.
func WithClock(now func() time.Time) Option {
	return func(s *Store) { s.now = now }
}

// Open opens the store at dir, creating the directory when it does not
// exist.
func Open(dir string, opts ...Option) (*Store, error) {
	s := &Store{
		dir:         dir,
		max:         agentmemory.DefaultMaxEntryBytes,
		lockTimeout: DefaultLockTimeout,
		now:         func() time.Time { return time.Now().UTC() },
	}
	for _, opt := range opts {
		opt(s)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("filestore: create %s: %w", dir, err)
	}
	return s, nil
}

// Dir returns the store's directory.
func (s *Store) Dir() string { return s.dir }

// MaxEntryBytes implements agentmemory.Store.
func (s *Store) MaxEntryBytes() int { return s.max }

// indexName is the per-scope index. Entries are kebab-case, so the
// uppercase name cannot collide with one.
const indexName = "INDEX.md"

func (s *Store) scopeDir(scope agentmemory.Scope) string {
	return filepath.Join(s.dir, string(scope))
}

func (s *Store) entryPath(scope agentmemory.Scope, name string) string {
	return filepath.Join(s.scopeDir(scope), name+".md")
}

func (s *Store) journalPath() string { return filepath.Join(s.dir, journalName) }
func (s *Store) lockPath() string    { return filepath.Join(s.dir, lockName) }

// check refuses a scope or name that is not kebab-case before it is
// used as a path, so nothing outside the store is ever read.
func check(scope agentmemory.Scope, name string) error {
	if !agentmemory.ValidScope(scope) {
		return fmt.Errorf("%w: scope %q is not kebab-case", agentmemory.ErrInvalid, scope)
	}
	if !agentmemory.ValidName(name) {
		return fmt.Errorf("%w: name %q is not kebab-case", agentmemory.ErrInvalid, name)
	}
	return nil
}

// Get implements agentmemory.Store.
func (s *Store) Get(_ context.Context, scope agentmemory.Scope, name string) (*agentmemory.Entry, error) {
	if err := check(scope, name); err != nil {
		return nil, err
	}
	e, err := s.read(scope, name)
	if err != nil {
		return nil, err
	}
	if e == nil {
		return nil, fmt.Errorf("%w: %s/%s", agentmemory.ErrNotFound, scope, name)
	}
	return e, nil
}

// read loads one entry file, or returns nil for none.
func (s *Store) read(scope agentmemory.Scope, name string) (*agentmemory.Entry, error) {
	path := s.entryPath(scope, name)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("filestore: read %s: %w", path, err)
	}
	d := decode(data)
	e := &agentmemory.Entry{
		Scope:   scope,
		Name:    name,
		Content: d.content,
		Meta:    d.meta,
		Hash:    agentmemory.Hash(d.content),
		Updated: d.updated,
	}
	if e.Updated.IsZero() {
		// A file a person wrote by hand: the file's own time is the
		// best there is.
		if info, err := os.Stat(path); err == nil {
			e.Updated = info.ModTime().UTC()
		}
	}
	return e, nil
}

// List implements agentmemory.Store. A file in the scope directory
// whose name is not kebab-case plus ".md" is not an entry and is not
// listed.
func (s *Store) List(_ context.Context, scope agentmemory.Scope) ([]agentmemory.Entry, error) {
	if !agentmemory.ValidScope(scope) {
		return nil, fmt.Errorf("%w: scope %q is not kebab-case", agentmemory.ErrInvalid, scope)
	}
	return s.list(scope)
}

func (s *Store) list(scope agentmemory.Scope) ([]agentmemory.Entry, error) {
	entries, err := os.ReadDir(s.scopeDir(scope))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("filestore: list %s: %w", scope, err)
	}
	var out []agentmemory.Entry
	for _, de := range entries {
		name, ok := strings.CutSuffix(de.Name(), ".md")
		if !ok || de.IsDir() || !agentmemory.ValidName(name) {
			continue
		}
		e, err := s.read(scope, name)
		if err != nil {
			return nil, err
		}
		if e != nil {
			out = append(out, *e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Put implements agentmemory.Store: the entry file is written whole
// and renamed into place, the scope's index is rewritten, and the
// change is appended to the journal, all under the lock.
func (s *Store) Put(ctx context.Context, e agentmemory.Entry, opts ...agentmemory.PutOption) error {
	o := agentmemory.ResolvePutOptions(opts...)
	// Validate before taking the lock, so a bad entry never waits; the
	// size check is repeated under the lock with the stored size.
	if err := agentmemory.CheckEntry(e, 0, nil); err != nil {
		return err
	}
	release, err := s.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	stored, err := s.read(e.Scope, e.Name)
	if err != nil {
		return err
	}
	if err := agentmemory.CheckEntry(e, s.max, stored); err != nil {
		return err
	}
	if err := o.Check(e.Scope, e.Name, stored); err != nil {
		return err
	}
	now := s.now()
	e.Hash = agentmemory.Hash(e.Content)
	e.Updated = now
	e.Deleted = false
	if len(e.Meta) == 0 {
		e.Meta = nil
	}
	if err := writeAtomic(s.entryPath(e.Scope, e.Name), encode(e)); err != nil {
		return err
	}
	if err := s.reindex(e.Scope); err != nil {
		return err
	}
	prev := ""
	if stored != nil {
		prev = stored.Hash
	}
	return s.record(ctx, e, prev, now)
}

// Forget implements agentmemory.Store: the file is removed, the index
// rewritten, and a tombstone with the last content appended to the
// journal.
func (s *Store) Forget(ctx context.Context, scope agentmemory.Scope, name string) error {
	if err := check(scope, name); err != nil {
		return err
	}
	release, err := s.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	stored, err := s.read(scope, name)
	if err != nil {
		return err
	}
	if stored == nil {
		return fmt.Errorf("%w: %s/%s", agentmemory.ErrNotFound, scope, name)
	}
	if err := os.Remove(s.entryPath(scope, name)); err != nil {
		return fmt.Errorf("filestore: remove %s/%s: %w", scope, name, err)
	}
	if err := s.reindex(scope); err != nil {
		return err
	}
	now := s.now()
	e := *stored
	e.Deleted = true
	e.Updated = now
	return s.record(ctx, e, stored.Hash, now)
}

// record appends the change with the next sequence number. The caller
// holds the lock.
func (s *Store) record(ctx context.Context, e agentmemory.Entry, prev string, at time.Time) error {
	seq, err := s.lastSeq()
	if err != nil {
		return err
	}
	return s.appendJournal(agentmemory.Change{
		Seq:     seq + 1,
		Entry:   e,
		Prev:    prev,
		Session: agentmemory.SessionFrom(ctx),
		At:      at,
	})
}

// Search implements agentmemory.Store with [agentmemory.Match],
// listing by scope order then name.
func (s *Store) Search(ctx context.Context, scopes []agentmemory.Scope, query string, limit int) ([]agentmemory.Entry, error) {
	var out []agentmemory.Entry
	for _, scope := range scopes {
		es, err := s.List(ctx, scope)
		if err != nil {
			return nil, err
		}
		for _, e := range es {
			if !agentmemory.Match(e, query) {
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

// Journal implements agentmemory.Store. A line that is not a record
// is yielded as an error and reading continues, so one damaged line
// does not hide the records after it.
func (s *Store) Journal(_ context.Context, after uint64) iter.Seq2[agentmemory.Change, error] {
	return func(yield func(agentmemory.Change, error) bool) {
		s.readJournal(after, yield)
	}
}

// Reconcile journals what changed outside the store: a person's edit
// to an entry file, a file they added, a file they removed. It
// compares every entry file with the journal's last state for that
// name and appends a change by nobody, Session empty, for each
// difference, then rewrites the indexes. It returns the changes it
// recorded. A product that lets people edit the directory runs it
// before it renders.
func (s *Store) Reconcile(ctx context.Context) ([]agentmemory.Change, error) {
	release, err := s.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	// The journal's last word on every name.
	last := map[string]agentmemory.Entry{}
	var seq uint64
	var readErr error
	s.readJournal(0, func(c agentmemory.Change, err error) bool {
		if err != nil {
			readErr = err
			return false
		}
		last[key(c.Entry.Scope, c.Entry.Name)] = c.Entry
		seq = c.Seq
		return true
	})
	if readErr != nil {
		return nil, readErr
	}
	// The files.
	scopes, err := s.scopes()
	if err != nil {
		return nil, err
	}
	live := map[string]agentmemory.Entry{}
	for _, scope := range scopes {
		es, err := s.list(scope)
		if err != nil {
			return nil, err
		}
		for _, e := range es {
			live[key(e.Scope, e.Name)] = e
		}
	}
	// Every difference, in a fixed order so two runs agree.
	keys := make([]string, 0, len(live)+len(last))
	for k := range live {
		keys = append(keys, k)
	}
	for k := range last {
		if _, ok := live[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	now := s.now()
	var changes []agentmemory.Change
	touched := map[agentmemory.Scope]bool{}
	for _, k := range keys {
		cur, isLive := live[k]
		known, isKnown := last[k]
		var c agentmemory.Change
		switch {
		case isLive && isKnown && !known.Deleted && known.Hash == cur.Hash && sameMeta(known.Meta, cur.Meta):
			continue
		case isLive:
			// Created or edited by hand. The file's own time is when
			// that happened; the updated line, if any, is when the
			// store last wrote the file.
			if agentmemory.CheckEntry(cur, s.max, nil) != nil {
				// Over the bound or otherwise unstorable: leave it to
				// the person, and out of the journal.
				continue
			}
			if info, err := os.Stat(s.entryPath(cur.Scope, cur.Name)); err == nil {
				cur.Updated = info.ModTime().UTC()
			}
			prev := ""
			if isKnown && !known.Deleted {
				prev = known.Hash
			}
			c = agentmemory.Change{Entry: cur, Prev: prev, At: now}
		case isKnown && !known.Deleted:
			// Removed by hand: a tombstone with the last content.
			e := known
			e.Deleted = true
			e.Updated = now
			c = agentmemory.Change{Entry: e, Prev: known.Hash, At: now}
		default:
			continue
		}
		seq++
		c.Seq = seq
		if err := s.appendJournal(c); err != nil {
			return changes, err
		}
		changes = append(changes, c)
		touched[c.Entry.Scope] = true
	}
	for scope := range touched {
		if err := s.reindex(scope); err != nil {
			return changes, err
		}
	}
	return changes, nil
}

// scopes lists the scope directories.
func (s *Store) scopes() ([]agentmemory.Scope, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("filestore: list scopes: %w", err)
	}
	var out []agentmemory.Scope
	for _, de := range entries {
		if de.IsDir() && agentmemory.ValidName(de.Name()) {
			out = append(out, agentmemory.Scope(de.Name()))
		}
	}
	return out, nil
}

func key(scope agentmemory.Scope, name string) string { return string(scope) + "/" + name }

func sameMeta(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// reindex rewrites a scope's INDEX.md: one line per live entry with
// its description, for the person reading the directory. The caller
// holds the lock.
func (s *Store) reindex(scope agentmemory.Scope) error {
	es, err := s.list(scope)
	if err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", scope)
	if len(es) == 0 {
		b.WriteString("No entries.\n")
	}
	for _, e := range es {
		fmt.Fprintf(&b, "- [%s](%s.md)", e.Name, e.Name)
		if d := e.Description(); d != "" {
			b.WriteString(" — ")
			b.WriteString(d)
		}
		b.WriteByte('\n')
	}
	return writeAtomic(filepath.Join(s.scopeDir(scope), indexName), []byte(b.String()))
}

// writeAtomic writes data to a temporary file beside path, syncs it,
// and renames it into place, so a reader sees the old file or the new
// one and never a partial write.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("filestore: create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("filestore: create temporary file in %s: %w", dir, err)
	}
	name := tmp.Name()
	_, werr := tmp.Write(data)
	if werr == nil {
		werr = tmp.Sync()
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(name, path)
	}
	if werr != nil {
		os.Remove(name)
		return fmt.Errorf("filestore: write %s: %w", path, werr)
	}
	if d, err := os.Open(dir); err == nil {
		// Make the rename durable where the platform allows a
		// directory to be synced; where it does not, the file is.
		_ = d.Sync()
		d.Close()
	}
	return nil
}

var _ agentmemory.Store = (*Store)(nil)
