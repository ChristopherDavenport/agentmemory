// Package sqlite is an [agentmemory.Store] over one SQLite database
// file with full-text search. It is a nested module so the driver
// (modernc.org/sqlite, pure Go) stays out of the root module's
// dependency graph. Entries are rows of memory_entries, the journal is
// memory_journal, whose line column holds the same JSON record
// filestore's journal.jsonl would, so a database can be dumped to that
// file without conversion, and Search runs over memory_entries_fts, an
// FTS5 index of each entry's name, meta and content, ranked by
// relevance. Every table is prefixed memory_, so the file can be
// shared with other stores, such as agentsession's.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/url"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/ChristopherDavenport/agentmemory"
	driver "modernc.org/sqlite" // registers the "sqlite" driver
	sqlite3 "modernc.org/sqlite/lib"
)

// busyTimeout is how long a connection waits on another's lock before
// failing with SQLITE_BUSY, and how long Open keeps retrying the
// schema when SQLite fails without waiting.
const busyTimeout = 5 * time.Second

// The tables carry the module's prefix so the store can share a
// database with others, agentsession's sqlite store among them, whose
// own table is named entries.
const schema = `
CREATE TABLE IF NOT EXISTS memory_entries (
	scope   TEXT NOT NULL,
	name    TEXT NOT NULL,
	content TEXT NOT NULL,
	meta    TEXT NOT NULL,
	hash    TEXT NOT NULL,
	updated TEXT NOT NULL,
	PRIMARY KEY (scope, name)
) STRICT;
CREATE TABLE IF NOT EXISTS memory_journal (
	seq   INTEGER PRIMARY KEY,
	scope TEXT NOT NULL,
	name  TEXT NOT NULL,
	line  TEXT NOT NULL
) STRICT;
CREATE INDEX IF NOT EXISTS memory_journal_entry ON memory_journal(scope, name, seq);
CREATE VIRTUAL TABLE IF NOT EXISTS memory_entries_fts USING fts5(
	scope UNINDEXED,
	name,
	meta,
	content,
	tokenize = 'unicode61'
);
`

// tables names each table the schema creates with the columns it has,
// in order. Open refuses a database whose table of one of these names
// has other columns, rather than failing at the first read or write.
var tables = []struct {
	name, legacy string
	columns      []string
}{
	{"memory_entries", "entries", []string{"scope", "name", "content", "meta", "hash", "updated"}},
	{"memory_journal", "journal", []string{"seq", "scope", "name", "line"}},
	{"memory_entries_fts", "entries_fts", []string{"scope", "name", "meta", "content"}},
}

// Store is a SQLite-backed [agentmemory.Store]. It is safe for
// concurrent use within one process, and several processes may share
// the file: every write is one immediate transaction, so writers queue
// on the database's own lock.
type Store struct {
	w, r *sql.DB
	path string
	max  int
	now  func() time.Time
}

// Option configures [Open].
type Option func(*Store)

// WithMaxEntryBytes sets the content bound; the default is
// [agentmemory.DefaultMaxEntryBytes].
func WithMaxEntryBytes(n int) Option {
	return func(s *Store) { s.max = n }
}

// WithClock sets the clock that stamps Updated and At, for tests that
// want a fixed one.
func WithClock(now func() time.Time) Option {
	return func(s *Store) { s.now = now }
}

// Open opens or creates the database at path and applies the schema.
// Every connection runs in WAL mode with a busy timeout, and writes go
// through a single-connection pool so writers queue instead of
// failing.
func Open(path string, opts ...Option) (*Store, error) {
	s := &Store{path: path, max: agentmemory.DefaultMaxEntryBytes, now: func() time.Time { return time.Now().UTC() }}
	for _, opt := range opts {
		opt(s)
	}
	pragmas := url.Values{}
	// The busy timeout comes first: journal_mode takes a lock on the
	// database, so a connection that sets it while another process is
	// writing has to wait rather than fail, and a connection that fails
	// there never sets the timeout at all.
	for _, p := range []string{fmt.Sprintf("busy_timeout(%d)", busyTimeout.Milliseconds()), "journal_mode(WAL)", "synchronous(NORMAL)"} {
		pragmas.Add("_pragma", p)
	}
	dsn := "file:" + path + "?" + pragmas.Encode()
	w, err := sql.Open("sqlite", dsn+"&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("sqlite: open writer: %w", err)
	}
	w.SetMaxOpenConns(1)
	r, err := sql.Open("sqlite", dsn)
	if err != nil {
		w.Close()
		return nil, fmt.Errorf("sqlite: open reader: %w", err)
	}
	r.SetMaxOpenConns(4 * runtime.NumCPU())
	// The schema goes in through the writer pool's immediate
	// transaction, so two processes opening one database at once queue
	// on the database's write lock under the busy timeout instead of
	// one of them failing with SQLITE_BUSY. On a database that does not
	// exist yet that is not enough: the first connection of each
	// process switches the new file to WAL, and SQLite fails that switch
	// with SQLITE_BUSY at once, without the busy handler, when another
	// process holds the file. applySchema retries it within the timeout.
	if err := applySchema(w); err != nil {
		w.Close()
		r.Close()
		return nil, err
	}
	s.w, s.r = w, r
	return s, nil
}

// applySchema applies the schema, trying again with a growing pause
// while SQLite reports the database busy, for as long as the busy
// timeout would have waited.
func applySchema(w *sql.DB) error {
	deadline := time.Now().Add(busyTimeout)
	pause := time.Millisecond
	for {
		err := applySchemaOnce(w)
		if err == nil || !isBusy(err) || time.Now().Add(pause).After(deadline) {
			return err
		}
		time.Sleep(pause)
		pause = min(2*pause, 50*time.Millisecond)
	}
}

// isBusy reports whether err is SQLite's SQLITE_BUSY, in any of its
// extended forms.
func isBusy(err error) bool {
	var se *driver.Error
	return errors.As(err, &se) && se.Code()&0xff == sqlite3.SQLITE_BUSY
}

func applySchemaOnce(w *sql.DB) error {
	tx, err := w.Begin()
	if err != nil {
		return fmt.Errorf("sqlite: begin: %w", err)
	}
	defer tx.Rollback()
	if err := migrate(tx); err != nil {
		return err
	}
	if _, err := tx.Exec(schema); err != nil {
		return fmt.Errorf("sqlite: apply schema: %w", err)
	}
	for _, t := range tables {
		cols, err := columns(tx, t.name)
		if err != nil {
			return err
		}
		if !slices.Equal(cols, t.columns) {
			return fmt.Errorf("sqlite: table %s has columns (%s), want (%s): the database holds another program's table of that name",
				t.name, strings.Join(cols, ", "), strings.Join(t.columns, ", "))
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: apply schema: %w", err)
	}
	return nil
}

// migrate renames the tables of a database an earlier release wrote,
// entries, journal and entries_fts, to the prefixed names, inside the
// schema transaction. It acts only when none of the prefixed tables
// exists and all three unprefixed ones have this store's columns, so
// another program's entries table, agentsession's for one, is left
// alone. The journal's index has no rename; it is dropped, and the
// schema creates it again under its new name.
func migrate(tx *sql.Tx) error {
	for _, t := range tables {
		cols, err := columns(tx, t.name)
		if err != nil {
			return err
		}
		if cols != nil {
			return nil
		}
		cols, err = columns(tx, t.legacy)
		if err != nil {
			return err
		}
		if !slices.Equal(cols, t.columns) {
			return nil
		}
	}
	for _, t := range tables {
		if _, err := tx.Exec(`ALTER TABLE "` + t.legacy + `" RENAME TO "` + t.name + `"`); err != nil {
			return fmt.Errorf("sqlite: migrate: rename %s: %w", t.legacy, err)
		}
	}
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_schema WHERE type = 'index' AND name = 'journal_entry' AND tbl_name = 'memory_journal'`).Scan(&n); err != nil {
		return fmt.Errorf("sqlite: migrate: %w", err)
	}
	if n > 0 {
		if _, err := tx.Exec(`DROP INDEX journal_entry`); err != nil {
			return fmt.Errorf("sqlite: migrate: drop journal_entry: %w", err)
		}
	}
	return nil
}

// columns returns the named table's columns in order, or nil when
// there is no such table.
func columns(tx *sql.Tx, table string) ([]string, error) {
	rows, err := tx.Query(`SELECT name FROM pragma_table_info(?) ORDER BY cid`, table)
	if err != nil {
		return nil, fmt.Errorf("sqlite: columns of %s: %w", table, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("sqlite: columns of %s: %w", table, err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: columns of %s: %w", table, err)
	}
	return out, nil
}

// Close runs PRAGMA optimize and closes both pools.
func (s *Store) Close() error {
	_, _ = s.w.Exec("PRAGMA optimize")
	err := s.w.Close()
	if rerr := s.r.Close(); err == nil {
		err = rerr
	}
	return err
}

// Path returns the database file.
func (s *Store) Path() string { return s.path }

// MaxEntryBytes implements agentmemory.Store.
func (s *Store) MaxEntryBytes() int { return s.max }

// querier is what Get reads through: the reader pool, or the write
// transaction inside Put and Forget.
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

const selectEntry = `SELECT content, meta, hash, updated FROM memory_entries WHERE scope = ? AND name = ?`

// get loads one entry, or returns nil for none.
func get(ctx context.Context, q querier, scope agentmemory.Scope, name string) (*agentmemory.Entry, error) {
	var content, meta, hash, updated string
	err := q.QueryRowContext(ctx, selectEntry, string(scope), name).Scan(&content, &meta, &hash, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: read %s/%s: %w", scope, name, err)
	}
	return buildEntry(scope, name, content, meta, hash, updated)
}

func buildEntry(scope agentmemory.Scope, name, content, meta, hash, updated string) (*agentmemory.Entry, error) {
	e := &agentmemory.Entry{Scope: scope, Name: name, Content: content, Hash: hash}
	if meta != "" && meta != "{}" {
		if err := json.Unmarshal([]byte(meta), &e.Meta); err != nil {
			return nil, fmt.Errorf("sqlite: meta of %s/%s: %w", scope, name, err)
		}
	}
	t, err := time.Parse(time.RFC3339Nano, updated)
	if err != nil {
		return nil, fmt.Errorf("sqlite: updated of %s/%s: %w", scope, name, err)
	}
	e.Updated = t
	return e, nil
}

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
func (s *Store) Get(ctx context.Context, scope agentmemory.Scope, name string) (*agentmemory.Entry, error) {
	if err := check(scope, name); err != nil {
		return nil, err
	}
	e, err := get(ctx, s.r, scope, name)
	if err != nil {
		return nil, err
	}
	if e == nil {
		return nil, fmt.Errorf("%w: %s/%s", agentmemory.ErrNotFound, scope, name)
	}
	return e, nil
}

// List implements agentmemory.Store.
func (s *Store) List(ctx context.Context, scope agentmemory.Scope) ([]agentmemory.Entry, error) {
	if !agentmemory.ValidScope(scope) {
		return nil, fmt.Errorf("%w: scope %q is not kebab-case", agentmemory.ErrInvalid, scope)
	}
	rows, err := s.r.QueryContext(ctx, `SELECT name, content, meta, hash, updated FROM memory_entries WHERE scope = ? ORDER BY name`, string(scope))
	if err != nil {
		return nil, fmt.Errorf("sqlite: list %s: %w", scope, err)
	}
	defer rows.Close()
	var out []agentmemory.Entry
	for rows.Next() {
		var name, content, meta, hash, updated string
		if err := rows.Scan(&name, &content, &meta, &hash, &updated); err != nil {
			return nil, fmt.Errorf("sqlite: list %s: %w", scope, err)
		}
		e, err := buildEntry(scope, name, content, meta, hash, updated)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list %s: %w", scope, err)
	}
	return out, nil
}

// Put implements agentmemory.Store: one immediate transaction reads
// the stored entry, checks the bound and the precondition against it,
// writes the row, refreshes the search index and appends the journal
// record with the next sequence number.
func (s *Store) Put(ctx context.Context, e agentmemory.Entry, opts ...agentmemory.PutOption) (*agentmemory.Change, error) {
	o := agentmemory.ResolvePutOptions(opts...)
	if err := agentmemory.CheckEntry(e, 0, nil); err != nil {
		return nil, err
	}
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("sqlite: begin: %w", err)
	}
	defer tx.Rollback()
	stored, err := get(ctx, tx, e.Scope, e.Name)
	if err != nil {
		return nil, err
	}
	if err := agentmemory.CheckEntry(e, s.max, stored); err != nil {
		return nil, err
	}
	if err := o.Check(e.Scope, e.Name, stored); err != nil {
		return nil, err
	}
	now := s.now()
	e.Hash = agentmemory.Hash(e.Content)
	e.Updated = now
	e.Deleted = false
	if len(e.Meta) == 0 {
		e.Meta = nil
	}
	meta := "{}"
	if e.Meta != nil {
		data, err := json.Marshal(e.Meta)
		if err != nil {
			return nil, fmt.Errorf("sqlite: encode meta: %w", err)
		}
		meta = string(data)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO memory_entries (scope, name, content, meta, hash, updated) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (scope, name) DO UPDATE SET content = excluded.content, meta = excluded.meta, hash = excluded.hash, updated = excluded.updated`,
		string(e.Scope), e.Name, e.Content, meta, e.Hash, stamp(now))
	if err != nil {
		return nil, fmt.Errorf("sqlite: write %s/%s: %w", e.Scope, e.Name, err)
	}
	if err := reindex(ctx, tx, e.Scope, e.Name, &e); err != nil {
		return nil, err
	}
	replaced := ""
	if stored != nil {
		replaced = stored.Hash
	}
	c, err := record(ctx, tx, agentmemory.Change{Entry: e, Prev: o.BaseFor(stored), Replaced: replaced,
		Session: agentmemory.SessionFrom(ctx), At: now})
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("sqlite: commit: %w", err)
	}
	return c, nil
}

// Forget implements agentmemory.Store.
func (s *Store) Forget(ctx context.Context, scope agentmemory.Scope, name string) (*agentmemory.Change, error) {
	if err := check(scope, name); err != nil {
		return nil, err
	}
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("sqlite: begin: %w", err)
	}
	defer tx.Rollback()
	stored, err := get(ctx, tx, scope, name)
	if err != nil {
		return nil, err
	}
	if stored == nil {
		return nil, fmt.Errorf("%w: %s/%s", agentmemory.ErrNotFound, scope, name)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_entries WHERE scope = ? AND name = ?`, string(scope), name); err != nil {
		return nil, fmt.Errorf("sqlite: remove %s/%s: %w", scope, name, err)
	}
	if err := reindex(ctx, tx, scope, name, nil); err != nil {
		return nil, err
	}
	now := s.now()
	e := *stored
	e.Deleted = true
	e.Updated = now
	c, err := record(ctx, tx, agentmemory.Change{Entry: e, Prev: stored.Hash, Replaced: stored.Hash,
		Session: agentmemory.SessionFrom(ctx), At: now})
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("sqlite: commit: %w", err)
	}
	return c, nil
}

// reindex replaces the named entry's row in the search index with e,
// or removes it when e is nil.
func reindex(ctx context.Context, tx *sql.Tx, scope agentmemory.Scope, name string, e *agentmemory.Entry) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_entries_fts WHERE scope = ? AND name = ?`, string(scope), name); err != nil {
		return fmt.Errorf("sqlite: index: %w", err)
	}
	if e == nil {
		return nil
	}
	values := make([]string, 0, len(e.Meta))
	for _, v := range e.Meta {
		values = append(values, v)
	}
	sort.Strings(values)
	if _, err := tx.ExecContext(ctx, `INSERT INTO memory_entries_fts (scope, name, meta, content) VALUES (?, ?, ?, ?)`,
		string(e.Scope), e.Name, strings.Join(values, "\n"), e.Content); err != nil {
		return fmt.Errorf("sqlite: index: %w", err)
	}
	return nil
}

// record appends c to the journal with the next sequence number and
// returns it. The line holds the whole record, sequence included, as
// filestore writes it.
func record(ctx context.Context, tx *sql.Tx, c agentmemory.Change) (*agentmemory.Change, error) {
	var last sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(seq) FROM memory_journal`).Scan(&last); err != nil {
		return nil, fmt.Errorf("sqlite: journal sequence: %w", err)
	}
	c.Seq = uint64(last.Int64) + 1
	line, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("sqlite: encode journal record: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO memory_journal (seq, scope, name, line) VALUES (?, ?, ?, ?)`,
		int64(c.Seq), string(c.Entry.Scope), c.Entry.Name, string(line)); err != nil {
		return nil, fmt.Errorf("sqlite: append journal: %w", err)
	}
	return &c, nil
}

// Search implements agentmemory.Store over the FTS5 index: every word
// of the query must match a token of the entry's name, meta or
// content, as a prefix and ignoring case, and results come in
// relevance order. An empty query lists the scopes' entries in scope
// order then name.
func (s *Store) Search(ctx context.Context, scopes []agentmemory.Scope, query string, limit int) ([]agentmemory.Entry, error) {
	if len(scopes) == 0 {
		return nil, nil
	}
	for _, scope := range scopes {
		if !agentmemory.ValidScope(scope) {
			return nil, fmt.Errorf("%w: scope %q is not kebab-case", agentmemory.ErrInvalid, scope)
		}
	}
	words := strings.Fields(query)
	if len(words) == 0 {
		var out []agentmemory.Entry
		for _, scope := range scopes {
			es, err := s.List(ctx, scope)
			if err != nil {
				return nil, err
			}
			out = append(out, es...)
			if limit > 0 && len(out) >= limit {
				return out[:limit], nil
			}
		}
		return out, nil
	}
	match := make([]string, 0, len(words))
	for _, w := range words {
		match = append(match, `"`+strings.ReplaceAll(w, `"`, `""`)+`" *`)
	}
	args := []any{strings.Join(match, " AND ")}
	marks := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		marks = append(marks, "?")
		args = append(args, string(scope))
	}
	q := `SELECT e.scope, e.name, e.content, e.meta, e.hash, e.updated
		FROM memory_entries_fts f JOIN memory_entries e ON e.scope = f.scope AND e.name = f.name
		WHERE memory_entries_fts MATCH ? AND f.scope IN (` + strings.Join(marks, ", ") + `)
		ORDER BY f.rank, e.scope, e.name`
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: search: %w", err)
	}
	defer rows.Close()
	var out []agentmemory.Entry
	for rows.Next() {
		var scope, name, content, meta, hash, updated string
		if err := rows.Scan(&scope, &name, &content, &meta, &hash, &updated); err != nil {
			return nil, fmt.Errorf("sqlite: search: %w", err)
		}
		e, err := buildEntry(agentmemory.Scope(scope), name, content, meta, hash, updated)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: search: %w", err)
	}
	return out, nil
}

// Journal implements agentmemory.Store.
func (s *Store) Journal(ctx context.Context, after uint64) iter.Seq2[agentmemory.Change, error] {
	return func(yield func(agentmemory.Change, error) bool) {
		rows, err := s.r.QueryContext(ctx, `SELECT seq, line FROM memory_journal WHERE seq > ? ORDER BY seq`, int64(after))
		if err != nil {
			yield(agentmemory.Change{}, fmt.Errorf("sqlite: journal: %w", err))
			return
		}
		defer rows.Close()
		for rows.Next() {
			var seq int64
			var line string
			if err := rows.Scan(&seq, &line); err != nil {
				yield(agentmemory.Change{}, fmt.Errorf("sqlite: journal: %w", err))
				return
			}
			var c agentmemory.Change
			if err := json.Unmarshal([]byte(line), &c); err != nil {
				if !yield(agentmemory.Change{}, fmt.Errorf("sqlite: journal record %d: %w", seq, err)) {
					return
				}
				continue
			}
			c.Seq = uint64(seq)
			if !yield(c, nil) {
				return
			}
		}
		if err := rows.Err(); err != nil {
			yield(agentmemory.Change{}, fmt.Errorf("sqlite: journal: %w", err))
		}
	}
}

// stamp renders a time as the entries table stores it.
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

var _ agentmemory.Store = (*Store)(nil)
