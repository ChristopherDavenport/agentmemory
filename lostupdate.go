package agentmemory

import "context"

// LostUpdate is one journal record that was not built on the state it
// replaced. The write landed, so the store is consistent; what it
// overwrote is only in the journal, and an auditor reads this to find
// out what a session may have discarded.
//
// Two shapes reach it. When [Change.Replaced] is Over, another writer's
// record landed between the read this write was built on and the write
// itself, and the content that record left is not in the entry any
// more: a lost update. When Replaced is not Over, the entry changed
// outside the journal, which for filestore means a person edited the
// file before the store noticed; filestore's Reconcile records that as
// a change of its own, so it appears as a record rather than a gap.
type LostUpdate struct {
	// Change is the record whose base is not what it replaced.
	Change Change
	// Over is the hash the journal left for that entry just before this
	// record, "" when the entry did not exist there.
	Over string
}

// LostUpdates reads s's journal from after and returns, in order, the
// records whose [Change.Prev] is not the state the journal left for
// that entry: the writes that were built on something else. A store
// whose writers all anchor their writes, as memory_patch does with
// [IfHash] and memory_save with [BasedOn], returns none.
//
// The cursor is [Store.Journal]'s: 0 reads the whole journal, and a
// later number reads from there, in which case the first record seen
// for an entry has no predecessor in the window and is not reported.
// The read stops at the first error the journal yields and returns what
// it has with it.
func LostUpdates(ctx context.Context, s Store, after uint64) ([]LostUpdate, error) {
	last := map[string]string{}
	seen := map[string]bool{}
	var out []LostUpdate
	for c, err := range s.Journal(ctx, after) {
		if err != nil {
			return out, err
		}
		key := string(c.Entry.Scope) + "/" + c.Entry.Name
		if (seen[key] || after == 0) && c.Prev != last[key] {
			out = append(out, LostUpdate{Change: c, Over: last[key]})
		}
		seen[key] = true
		if c.Entry.Deleted {
			// A tombstone leaves no content, so the next write to the
			// name is a create and its base is empty.
			last[key] = ""
		} else {
			last[key] = c.Entry.Hash
		}
	}
	return out, nil
}
