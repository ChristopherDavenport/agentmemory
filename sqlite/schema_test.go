package sqlite

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentmemory"
)

// legacySchema is the schema v0.0.5 and earlier created, with its
// unprefixed table names.
const legacySchema = `
CREATE TABLE entries (
	scope   TEXT NOT NULL,
	name    TEXT NOT NULL,
	content TEXT NOT NULL,
	meta    TEXT NOT NULL,
	hash    TEXT NOT NULL,
	updated TEXT NOT NULL,
	PRIMARY KEY (scope, name)
) STRICT;
CREATE TABLE journal (
	seq   INTEGER PRIMARY KEY,
	scope TEXT NOT NULL,
	name  TEXT NOT NULL,
	line  TEXT NOT NULL
) STRICT;
CREATE INDEX journal_entry ON journal(scope, name, seq);
CREATE VIRTUAL TABLE entries_fts USING fts5(
	scope UNINDEXED,
	name,
	meta,
	content,
	tokenize = 'unicode61'
);
`

// sessionSchema is agentsession/sqlite v0.0.9's entries table and its
// index, the tables this store once collided with.
const sessionSchema = `
CREATE TABLE IF NOT EXISTS sessions (
	id TEXT PRIMARY KEY
) STRICT;
CREATE TABLE IF NOT EXISTS entries (
	session_id TEXT    NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	seq        INTEGER NOT NULL,
	id         TEXT    NOT NULL,
	parent     TEXT,
	type       TEXT    NOT NULL,
	line       TEXT    NOT NULL,
	UNIQUE (session_id, seq),
	UNIQUE (session_id, id)
) STRICT;
CREATE INDEX IF NOT EXISTS entries_id ON entries(id);
`

func execRaw(t *testing.T, path, query string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func schemaNames(t *testing.T, path string) []string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT name FROM sqlite_schema WHERE type IN ('table', 'index') AND name NOT LIKE 'sqlite_%' AND name NOT LIKE '%fts_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		out = append(out, name)
	}
	return out
}

// TestMigrateLegacyTables opens a database an earlier release wrote and
// finds its entry, its journal and its search index under the new
// names, with nothing left under the old ones.
func TestMigrateLegacyTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	execRaw(t, path, legacySchema)
	at := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	stored := agentmemory.Entry{Scope: "user", Name: "tz", Content: "Europe/Lisbon", Hash: agentmemory.Hash("Europe/Lisbon"), Updated: at}
	line, err := json.Marshal(agentmemory.Change{Seq: 1, Entry: stored, At: at})
	if err != nil {
		t.Fatal(err)
	}
	execRaw(t, path, `INSERT INTO entries VALUES ('user', 'tz', 'Europe/Lisbon', '{}', ?, ?)`, stored.Hash, stamp(at))
	execRaw(t, path, `INSERT INTO entries_fts VALUES ('user', 'tz', '', 'Europe/Lisbon')`)
	execRaw(t, path, `INSERT INTO journal VALUES (1, 'user', 'tz', ?)`, string(line))

	s := openStore(t, path)
	ctx := t.Context()
	e, err := s.Get(ctx, "user", "tz")
	if err != nil || e.Content != "Europe/Lisbon" {
		t.Fatalf("Get = %v, %v; want the migrated entry", e, err)
	}
	found, err := s.Search(ctx, []agentmemory.Scope{"user"}, "lisbon", 0)
	if err != nil || len(found) != 1 {
		t.Fatalf("Search = %v, %v; want the migrated entry", found, err)
	}
	c, err := s.Put(ctx, agentmemory.Entry{Scope: "user", Name: "tz", Content: "Europe/Paris"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Seq != 2 {
		t.Errorf("Put Seq = %d, want 2 after the migrated record", c.Seq)
	}
	var seqs []uint64
	for c, err := range s.Journal(ctx, 0) {
		if err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, c.Seq)
	}
	if !slices.Equal(seqs, []uint64{1, 2}) {
		t.Errorf("journal = %v, want [1 2]", seqs)
	}
	want := []string{"memory_entries", "memory_entries_fts", "memory_journal", "memory_journal_entry"}
	if got := schemaNames(t, path); !slices.Equal(got, want) {
		t.Errorf("schema = %v, want %v", got, want)
	}
}

// TestShareWithSessions opens this store on one file with
// agentsession's tables, in each order: both open, and each writes to
// its own entries table.
func TestShareWithSessions(t *testing.T) {
	for _, sessionsFirst := range []bool{true, false} {
		name := "memory-first"
		if sessionsFirst {
			name = "sessions-first"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "shared.db")
			if sessionsFirst {
				execRaw(t, path, sessionSchema)
			}
			s := openStore(t, path)
			if !sessionsFirst {
				execRaw(t, path, sessionSchema)
			}
			if _, err := s.Put(t.Context(), agentmemory.Entry{Scope: "user", Name: "tz", Content: "Europe/Lisbon"}); err != nil {
				t.Fatal(err)
			}
			execRaw(t, path, `INSERT INTO sessions VALUES ('s1')`)
			execRaw(t, path, `INSERT INTO entries (session_id, seq, id, type, line) VALUES ('s1', 1, 'e1', 'message', '{}')`)
			s2 := openStore(t, path)
			if es, err := s2.List(t.Context(), "user"); err != nil || len(es) != 1 {
				t.Errorf("List after reopening = %v, %v; want one entry", es, err)
			}
		})
	}
}

// TestOpenRefusesForeignTable fails Open, naming the table, when a
// table of one of this store's names has other columns.
func TestOpenRefusesForeignTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	execRaw(t, path, `CREATE TABLE memory_entries (id TEXT PRIMARY KEY, body TEXT)`)
	s, err := Open(path)
	if err == nil {
		s.Close()
		t.Fatal("Open succeeded over a foreign memory_entries table")
	}
	if !strings.Contains(err.Error(), "memory_entries has columns (id, body)") {
		t.Errorf("Open error = %v; want it to name the table and its columns", err)
	}
}
