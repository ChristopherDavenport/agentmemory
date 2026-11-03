package agentmemory

import (
	"context"
	"errors"
)

// LostUpdate is one journal record that was not built on the state it
// replaced, or that does not continue the chain the reader holds for
// its entry. The write landed, so the store is consistent; what it
// overwrote is only in the journal, and an auditor reads this to find
// out what a session may have discarded.
//
// Two shapes reach it, and one record may have both. A fork is a record
// whose [Change.Prev] is not its [Change.Replaced]: it was built on a
// state another writer had already replaced, or on one the journal
// never held, such as a person's edit the store could not take. That is
// a property of the record alone, so it is reported wherever the record
// lies. A break is a record whose Replaced is not the last state the
// reader holds for its entry: the chain is broken there, because a
// record between the two is missing from what the reader could read, or
// the journal was edited behind the store. It needs the entry's earlier
// record, so it is reported only where the window read holds one.
type LostUpdate struct {
	// Change is the record.
	Change Change
	// Over is what the record wrote over as far as the reader can say:
	// the last state the window holds for the entry, "" when that is no
	// live entry, or the record's Replaced when the window holds nothing
	// earlier for the entry.
	Over string
	// Break says the record's Replaced is not Over: the chain breaks at
	// this record.
	Break bool
	// AfterDamage says the reader met a record it could not read after
	// the entry's earlier record, or after the start of the window, and
	// before this one, so a Break may be the damage: the missing record
	// may be the one the chain runs through.
	AfterDamage bool
}

// LostUpdates reads s's journal from after and returns, in order, the
// records that lost an update: each record whose [Change.Prev] is not
// its [Change.Replaced], wherever it lies, and each whose Replaced is
// not the last state the window holds for its entry. A store whose
// writers all anchor their writes, as memory_patch does with [IfHash]
// and memory_save with [BasedOn], and whose journal is whole, returns
// none.
//
// The cursor is [Store.Journal]'s: 0 reads the whole journal, and holds
// the start of every entry, which is no live entry; a later number reads
// from there, and an entry's first record in that window has nothing to
// be a break against, so it is judged as a fork alone.
//
// A record the journal cannot read does not stop the read: LostUpdates
// goes on, reports what it finds on both sides, marks a break after the
// damage with AfterDamage, and returns every error the journal yielded,
// joined, beside the list.
func LostUpdates(ctx context.Context, s Store, after uint64) ([]LostUpdate, error) {
	type held struct {
		hash   string
		damage int // the damage seen when the state was taken
	}
	last := map[string]held{}
	damage := 0
	var errs []error
	var out []LostUpdate
	for c, err := range s.Journal(ctx, after) {
		if err != nil {
			errs = append(errs, err)
			damage++
			continue
		}
		key := string(c.Entry.Scope) + "/" + c.Entry.Name
		h, ok := last[key]
		if !ok && after == 0 {
			// The whole journal holds the entry's start: not live.
			h, ok = held{}, true
		}
		lu := LostUpdate{Change: c, Over: c.Replaced}
		if ok {
			lu.Over = h.hash
			lu.Break = c.Replaced != h.hash
			lu.AfterDamage = lu.Break && damage > h.damage
		}
		if lu.Break || c.Prev != c.Replaced {
			out = append(out, lu)
		}
		// A tombstone leaves no live entry, so the next write to the
		// name is a create and replaces nothing.
		next := held{hash: c.Entry.Hash, damage: damage}
		if c.Entry.Deleted {
			next.hash = ""
		}
		last[key] = next
	}
	return out, errors.Join(errs...)
}

// LastSeqer is implemented by a store that answers its journal's last
// sequence number without the journal being read, as every store in
// this repository does: the in-memory store's length, filestore's tail
// and sqlite's largest seq. See [LastSeq].
type LastSeqer interface {
	// LastSeq returns the sequence number after which the journal
	// yields nothing now, 0 for an empty journal: the seq of the last
	// record the store can read, or a greater number the store holds for
	// a record it cannot. The next record appended takes a greater one.
	LastSeq(ctx context.Context) (uint64, error)
}

// LastSeq returns s's last sequence number: from s when it is a
// [LastSeqer], and otherwise by reading the journal to its end. A
// background pass that resumes from a cursor needs it without the
// journal, which is unbounded.
//
// The fallback counts only records it can read, since a damaged one
// carries no number it can trust, and returns the errors the journal
// yielded, joined, beside the number it reached.
func LastSeq(ctx context.Context, s Store) (uint64, error) {
	if ls, ok := s.(LastSeqer); ok {
		return ls.LastSeq(ctx)
	}
	var seq uint64
	var errs []error
	for c, err := range s.Journal(ctx, 0) {
		if err != nil {
			errs = append(errs, err)
			continue
		}
		seq = max(seq, c.Seq)
	}
	return seq, errors.Join(errs...)
}
