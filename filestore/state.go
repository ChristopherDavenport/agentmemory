package filestore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ChristopherDavenport/agentmemory"
)

// stateName is the cursor beside the journal: how far the store has
// read it and what it last knew of every live entry. It is what makes
// a person's edit cheap to notice, because noticing one means
// comparing the files with the journal's last word, and reading the
// whole journal for that costs more every turn for ever. The file is
// derived, never authoritative: a missing, damaged or stale one is
// rebuilt from the journal, and the journal is the record.
//
// The hashes are the whole state: the content's, and one over the
// metadata, so a person's edit to an entry's frontmatter is noticed
// without keeping a copy of every description.
const stateName = ".state.json"

type journalState struct {
	// Seq is the last sequence number the state has seen.
	Seq uint64 `json:"seq"`
	// Off is where the next record starts in the journal, so catching
	// up reads what was appended and not the file.
	Off int64 `json:"off"`
	// Entries is the last state of every live entry, by "scope/name".
	// A tombstoned name is absent, which is what a name the store has
	// never seen looks like, and both mean the next write to it is a
	// create.
	Entries map[string]stateEntry `json:"entries,omitempty"`

	// read says the cursor came from a file that was there and whole,
	// so a caller that changed nothing knows there is nothing to write.
	// It is not part of the file.
	read bool
}

type stateEntry struct {
	Hash string `json:"hash"`
	Meta string `json:"meta,omitempty"`
}

func (s *Store) statePath() string { return filepath.Join(s.dir, stateName) }

// metaHash is the hash of an entry's metadata, "" for none, so the
// state can tell a frontmatter edit from an unchanged file without
// holding the metadata itself.
func metaHash(meta map[string]string) string {
	if len(meta) == 0 {
		return ""
	}
	keys := make([]string, 0, len(meta))
	for k := range meta {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(meta[k])
		b.WriteByte('\n')
	}
	return agentmemory.Hash(b.String())
}

// note records what the journal now holds for the entry of a change.
func (st *journalState) note(e agentmemory.Entry) {
	k := key(e.Scope, e.Name)
	if e.Deleted {
		delete(st.Entries, k)
		return
	}
	st.Entries[k] = stateEntry{Hash: e.Hash, Meta: metaHash(e.Meta)}
}

// agrees reports whether the entry a file holds is the state the
// journal last recorded for it.
func (se stateEntry) agrees(e agentmemory.Entry) bool {
	return se.Hash == e.Hash && se.Meta == metaHash(e.Meta)
}

// loadState reads the cursor and catches it up with the journal, so
// what it returns is the journal's last word on every entry. The
// caller holds the lock. A cursor that is missing, unreadable or
// points past the journal's end is rebuilt by reading the journal
// whole, which is what the store did on every call before the cursor
// existed. A line that is not a record is skipped, since the state is
// derived and a damaged line is reported by [Store.Journal] to whoever
// reads it.
func (s *Store) loadState() (*journalState, error) {
	st := &journalState{Entries: map[string]stateEntry{}}
	data, err := os.ReadFile(s.statePath())
	switch {
	case err == nil:
		if uerr := json.Unmarshal(data, st); uerr != nil {
			st = &journalState{Entries: map[string]stateEntry{}}
			break
		}
		if st.Entries == nil {
			st.Entries = map[string]stateEntry{}
		}
		st.read = true
	case errors.Is(err, fs.ErrNotExist):
	default:
		return nil, fmt.Errorf("filestore: read %s: %w", s.statePath(), err)
	}
	off, err := s.scanJournal(st.Off, func(c agentmemory.Change, err error) bool {
		if err != nil {
			return true // damaged lines belong to the journal's readers
		}
		st.Seq = c.Seq
		st.note(c.Entry)
		return true
	})
	if err != nil {
		return nil, err
	}
	if off < st.Off {
		// The journal was replaced or truncated behind the store, so
		// what the cursor holds is about a file that is gone.
		return s.rebuildState()
	}
	st.Off = off
	return st, nil
}

func (s *Store) rebuildState() (*journalState, error) {
	st := &journalState{Entries: map[string]stateEntry{}}
	off, err := s.scanJournal(0, func(c agentmemory.Change, err error) bool {
		if err == nil {
			st.Seq = c.Seq
			st.note(c.Entry)
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	st.Off = off
	return st, nil
}

// saveState writes the cursor. The caller holds the lock.
func (s *Store) saveState(st *journalState) error {
	data, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("filestore: encode %s: %w", stateName, err)
	}
	return writeAtomic(s.statePath(), append(data, '\n'))
}

// nextSeq is the sequence number the next record takes. The cursor has
// been caught up with the journal, so its number is the journal's
// last; the tail is read as well, one small read, because a journal
// edited or replaced behind the store must not end with two records
// sharing a number.
func (s *Store) nextSeq(st *journalState) (uint64, error) {
	tail, err := s.lastSeq()
	if err != nil {
		return 0, err
	}
	if tail > st.Seq {
		st.Seq = tail
	}
	return st.Seq + 1, nil
}

// appendChange numbers the record, appends it and advances the cursor.
// The caller holds the lock and saves the cursor once it is done.
func (s *Store) appendChange(st *journalState, c agentmemory.Change) (*agentmemory.Change, error) {
	seq, err := s.nextSeq(st)
	if err != nil {
		return nil, err
	}
	c.Seq = seq
	off, err := s.appendJournal(c)
	if err != nil {
		return nil, err
	}
	st.Seq = c.Seq
	st.Off = off
	st.note(c.Entry)
	return &c, nil
}

// outside builds the record for an entry whose file is not what the
// journal last held, and nil when they agree: a person edited, added
// or removed it outside the store. cur is what the file holds now, or
// nil when there is none. The record is attributed to nobody, since
// nobody in a session made it, and marked [agentmemory.SourceReconciled].
//
// A write calls this before writing over the file it read, so the
// person's version is in the journal before the agent's replaces it;
// that is the gap Reconcile alone could not close, because once the
// agent has written, the file and the journal agree again and there is
// nothing left to record.
func (s *Store) outside(st *journalState, scope agentmemory.Scope, name string, cur *agentmemory.Entry, at time.Time) (*agentmemory.Change, error) {
	k := key(scope, name)
	known, isKnown := st.Entries[k]
	switch {
	case cur == nil && !isKnown:
		return nil, nil
	case cur == nil:
		// Removed by hand: a tombstone with the last content, which is
		// in the journal's last record for the name. That read is the
		// whole journal, and this is the one case that needs it.
		last, err := s.lastRecordFor(scope, name)
		if err != nil || last == nil {
			return nil, err
		}
		e := last.Entry
		e.Deleted = true
		e.Updated = at
		return &agentmemory.Change{Entry: e, Prev: known.Hash, Replaced: known.Hash, At: at, Source: agentmemory.SourceReconciled}, nil
	case isKnown && known.agrees(*cur):
		return nil, nil
	}
	if agentmemory.CheckEntry(*cur, s.max, nil) != nil {
		// Over the bound or otherwise unstorable: leave it to the
		// person, and out of the journal.
		return nil, nil
	}
	e := *cur
	// The file's own time is when the person changed it; the updated
	// line, if any, is when the store last wrote the file.
	if info, err := os.Stat(s.entryPath(scope, name)); err == nil {
		e.Updated = info.ModTime().UTC()
	}
	prev := ""
	if isKnown {
		prev = known.Hash
	}
	return &agentmemory.Change{Entry: e, Prev: prev, Replaced: prev, At: at, Source: agentmemory.SourceReconciled}, nil
}

// lastRecordFor returns the journal's last record for one entry, or
// nil. It reads the journal whole, so it is for the case that has no
// other answer: a file a person removed, whose content only the
// journal still has.
func (s *Store) lastRecordFor(scope agentmemory.Scope, name string) (*agentmemory.Change, error) {
	var last *agentmemory.Change
	if _, err := s.scanJournal(0, func(c agentmemory.Change, err error) bool {
		if err != nil {
			return true
		}
		if c.Entry.Scope == scope && c.Entry.Name == name {
			rec := c
			last = &rec
		}
		return true
	}); err != nil {
		return nil, err
	}
	return last, nil
}
