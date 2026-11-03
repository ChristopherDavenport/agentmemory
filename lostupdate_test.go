package agentmemory

import (
	"context"
	"fmt"
	"iter"
	"strings"
	"testing"
)

// journalStore is a store whose journal is a fixed list of records and
// errors, which is what a reader of a damaged journal sees: the
// in-memory store cannot hold a record it cannot read.
type journalStore struct {
	*MemStore
	steps []journalStep
}

type journalStep struct {
	c   Change
	err error
}

func (j journalStore) Journal(_ context.Context, after uint64) iter.Seq2[Change, error] {
	return func(yield func(Change, error) bool) {
		for _, s := range j.steps {
			if s.err == nil && s.c.Seq <= after {
				continue
			}
			if !yield(s.c, s.err) {
				return
			}
		}
	}
}

// rec is a record of entry user/<name> with content, built on prev and
// landing on replaced.
func rec(seq uint64, name, content, prev, replaced string) journalStep {
	return journalStep{c: Change{Seq: seq, Entry: Entry{Scope: "user", Name: name, Content: content, Hash: Hash(content)}, Prev: prev, Replaced: replaced}}
}

func damage(line int) journalStep {
	return journalStep{err: fmt.Errorf("journal line %d: damaged", line)}
}

// TestLostUpdatesPastDamage is #46: a reader that meets a record it
// cannot read goes on, reports the lost updates on both sides of it and
// the damage beside them, and attributes a break in the chain after the
// damage to the damage.
func TestLostUpdatesPastDamage(t *testing.T) {
	h := Hash
	// Record 2, the write that took "a" from one to two, is damaged.
	// Record 3 was built on one and landed on two: a fork, which its own
	// prev and replaced show, and a break against the last state the
	// reader holds, one, which the damage explains. Record 4 is a fork
	// before nothing: it is reported whether or not the damage was read.
	steps := []journalStep{
		rec(1, "a", "one", "", ""),
		damage(2),
		rec(3, "a", "three", h("one"), h("two")),
		rec(4, "b", "bee", "", ""),
		rec(5, "b", "bee2", h("stale"), h("bee")),
	}
	st := journalStore{MemStore: NewMemStore(), steps: steps}
	tests := []struct {
		name  string
		after uint64
		want  []string // seq:fork/break/damage
	}{
		{"from the start", 0, []string{"3:fork,break,damage", "5:fork"}},
		{"from after the first record", 1, []string{"3:fork", "5:fork"}},
		{"from after the fork", 3, []string{"5:fork"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lost, err := LostUpdates(context.Background(), st, tt.after)
			if err == nil || !strings.Contains(err.Error(), "journal line 2") {
				t.Errorf("err = %v, want the damage at line 2", err)
			}
			if got := describeLost(lost); fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Errorf("LostUpdates = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestLostUpdatesAfterACursor is #49: a fork is a property of the record
// alone, so one right after the cursor is reported; a break is reported
// only where the window holds the entry's earlier record.
func TestLostUpdatesAfterACursor(t *testing.T) {
	h := Hash
	steps := []journalStep{
		rec(1, "a", "one", "", ""),
		rec(2, "a", "two", h("one"), h("one")),
		rec(3, "a", "three", h("one"), h("two")), // a fork: built on one, landed on two
		rec(4, "a", "four", h("gap"), h("gap")),  // a break: the window holds three
		rec(5, "c", "see", h("x"), h("x")),       // a break only from 0, where c was not live
	}
	st := journalStore{MemStore: NewMemStore(), steps: steps}
	tests := []struct {
		after uint64
		want  []string
	}{
		{0, []string{"3:fork", "4:break", "5:break"}},
		{1, []string{"3:fork", "4:break"}},
		{2, []string{"3:fork", "4:break"}},
		{3, nil},
		{4, nil},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("after %d", tt.after), func(t *testing.T) {
			lost, err := LostUpdates(context.Background(), st, tt.after)
			if err != nil {
				t.Fatal(err)
			}
			if got := describeLost(lost); fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Errorf("LostUpdates = %v, want %v", got, tt.want)
			}
		})
	}
	// Over is what the record wrote over: the state the window holds for
	// a break, the record's replaced for a fork the window has no
	// predecessor for.
	lost, _ := LostUpdates(context.Background(), st, 2)
	if len(lost) != 2 {
		t.Fatalf("LostUpdates(after 2) = %d records, want 2", len(lost))
	}
	if lost[0].Over != h("two") || lost[1].Over != h("three") {
		t.Errorf("Over = %s, %s", lost[0].Over, lost[1].Over)
	}
}

func describeLost(lost []LostUpdate) []string {
	var out []string
	for _, l := range lost {
		var kinds []string
		if l.Change.Prev != l.Change.Replaced {
			kinds = append(kinds, "fork")
		}
		if l.Break {
			kinds = append(kinds, "break")
		}
		if l.AfterDamage {
			kinds = append(kinds, "damage")
		}
		out = append(out, fmt.Sprintf("%d:%s", l.Change.Seq, strings.Join(kinds, ",")))
	}
	return out
}

// hidden hides a store's optional interfaces, so a caller takes the
// fallback.
type hidden struct{ Store }

func TestLastSeq(t *testing.T) {
	ctx := context.Background()
	st := NewMemStore()
	for _, s := range []Store{st, hidden{st}} {
		if n, err := LastSeq(ctx, s); n != 0 || err != nil {
			t.Errorf("LastSeq(empty %T) = %d, %v", s, n, err)
		}
	}
	for i := range 3 {
		if _, err := st.Put(ctx, Entry{Scope: "user", Name: fmt.Sprintf("n%d", i), Content: "c"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := Store(st).(LastSeqer); !ok {
		t.Error("the in-memory store does not answer LastSeq")
	}
	for _, s := range []Store{st, hidden{st}} {
		if n, err := LastSeq(ctx, s); n != 3 || err != nil {
			t.Errorf("LastSeq(%T) = %d, %v; want 3", s, n, err)
		}
	}
	// The fallback reads to the end and goes past damage, which is not a
	// record and carries no number, and returns it beside the number.
	js := journalStore{MemStore: NewMemStore(), steps: []journalStep{rec(1, "a", "one", "", ""), rec(2, "a", "two", Hash("one"), Hash("one")), damage(3)}}
	n, err := LastSeq(ctx, hidden{js})
	if n != 2 || err == nil {
		t.Errorf("LastSeq over a damaged tail = %d, %v; want 2 and the damage", n, err)
	}
}
