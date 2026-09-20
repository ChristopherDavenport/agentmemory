# Feedback from the design studies

Findings the design studies under `../examples` raised against this
module, held here because it has no issue tracker yet. Each section is
one finding in the shape of an issue: why it matters, the evidence, a
failure scenario and the smallest fix. The plan in `plans/` is the
document these correct; apply them there before the code is written,
and move each section to an issue when a repository exists.

Generated 2026-09-20 from the studies' issue drafts.

## 1. tools: a full-replace write is the only way to edit an entry, and it costs the prompt twice


### Why this is necessary

The plan gives the model one way to change a remembered fact: read the entry,
edit it, and write the whole thing back. That makes every correction to a 4 KiB
note a 4 KiB tool call, which then sits in the transcript for the rest of the
session beside the rendered block that already holds the same text, so the
model reads the note twice on every later turn. It is also the write that
silently loses a concurrent edit, because the new value is built from a read
that may already be stale. Letta shipped the same tool and then moved away from
it.

### Evidence

agentmemory has no repository yet; citations are to the plan as written.
`docs/plans/memory-layer.md:97` states the choice: "`Put` is create-or-replace
of the whole content. Patching is the model's job: it reads, edits, and puts.
That keeps the journal a list of full states, each independently hashable, at
the cost of a larger write, which the size bound keeps small." The bound keeps
the store small and does nothing about the write, which is paid three times: in
the call arguments, in the transcript that keeps them, and in the config delta
that re-renders the block.

A probe that makes the same edit to the same 4,000 byte entry both ways, under
the echo adapter with the run recorded and every hash verifying, shows the
`function_call` entry at 4,316 bytes against 412, the session at 14,683 against
10,863, and the next request's prompt, instructions and input together, at
12,459 bytes against 4,618. That is 35% more session and 2.7x the prompt for
one edit.

Letta, at tag 0.16.8, offers `core_memory_replace(label, old_content,
new_content)` whose content "must be an exact match", and
`memory_replace(label, old_string, new_string)` which additionally refuses when
the string is absent or appears more than once. `memory_rethink(label,
new_memory)` is its only full replace, and Letta's shared-memory documentation
grades it "No (last-writer-wins)" against `memory_replace`'s "Mostly (fails if
target string changed)".

### Failure scenario

The model keeps a 4 KiB style note and corrects one sentence. It spends 8 KiB
of session, doubles the note in its own window for the rest of the
conversation, and if another channel edited the note between the read and the
write, discards that edit with nothing said.

### Suggested fix

Add a fourth tool, `memory_patch(scope, name, old_text, new_text)`, exact
match, refusing when `old_text` is absent or appears more than once and saying
which, and tell the model in the tool description to prefer it over
`memory_save`. The store applies the patch, so `Put` stays the only write and
the journal stays a list of full states, each independently hashable.

It must not add a patch form to `Put` or to `Store`. The patch is a tool
concern, and a store whose write is a diff cannot keep that journal property.
It must not replace `memory_save`, which is still how an entry is created.

Found by the letta-memory design study against Letta.

## 2. store: two sessions writing one entry lose a write, and the journal cannot say which


### Why this is necessary

Memory shared across several chat channels means several sessions writing one
store at once, the case the plan is built for. A `Put` has no precondition and
a journal record names no predecessor, so when two channels edit the same entry
the later write wins whole and nothing anywhere can tell that the earlier one
was lost. The journal looks healthy, the entry looks plausible, and a fact the
user gave the assistant is gone. A product cannot detect this, cannot repair
it, and cannot tell the user it happened.

### Evidence

agentmemory has no repository yet; citations are to the plan as written.
`docs/plans/memory-layer.md:149` promises, for `filestore`, atomic writes
"serialised with a lock file, so two sessions writing the same scope do not
tear a file; last write wins on the entry, and the journal keeps both".
`Change` is `{Entry, Session, At}` (`docs/plans/memory-layer.md:90`), with no
field for the content the change replaced.

A probe that builds that filestore and runs the race, two channels reading one
entry and each writing back the whole content with its own fact added, shows
that every promise held literally and none of them helped. Nothing tore, the
last write won, and the journal holds three records that read as three
plausible successive states. One channel's fact is absent from the live entry
and present only in a journal record that nothing links to a fork. The probe
also reports that no record names the content it replaced and no record says
which read it was based on. The same race through patch tools loses nothing,
because each edit is anchored in the stored value, and an edit whose anchor
another channel removed fails out loud.

Letta is in the same position and says so. At tag 0.16.8 `block` carries a
`version` column with `__mapper_args__ = {"version_id_col": version}`, and
`block_history` rows carry `(block_id, sequence_number)` and an actor, and the
documentation still says multiple agents doing `memory_rethink` on one block
"leads to lost updates", because an optimistic version guards the row and not
the read the new value was built from.

### Failure scenario

The user tells the assistant on Slack that they have moved to Bristol and on
Telegram that they prefer Go, within the same second. Two channels read the
same profile entry, each appends its fact, each writes the whole entry. One
fact is gone. The next render shows the survivor as the whole truth, and no
later reader can find the loss.

### Suggested fix

Two small additions. `Put` takes an optional `IfHash(h string)` precondition
and fails when the stored content hash differs, `""` meaning the entry must not
exist. `Change` gains `Prev string`, the content hash the change replaced,
empty for a create, so a journal reader can chain records and see a fork.

It must not make `IfHash` mandatory. A create and a deliberate overwrite are
both legitimate. These two additions detect the loss; preventing it needs the
model's default tool anchored in the stored value, which is the `memory_patch`
issue.

Found by the letta-memory design study against Letta.

## 3. render: the size bound is enforced on write and never shown to the model


### Why this is necessary

The plan refuses a write over the per-entry bound so the model is told to split
or trim rather than have its write silently cut, which is the right call. But
nothing the model can see says how large an entry is or what its limit is, so
the refusal arrives after it has already composed 4 KiB it cannot store, and
arrives without the one number it needs to compute a trim. The model can only
guess, and each guess costs a turn. Both reference designs render the budget;
only this one refuses without it.

### Evidence

agentmemory has no repository yet; citations are to the plan as written.
`docs/plans/memory-layer.md:105` states the bound and the refusal. The `Render`
description at `docs/plans/memory-layer.md:128` and the `Manifest` beside it
carry sizes for the session's provenance. The rendered block is "one section
per scope, one entry per section, in index order", with the names of what the
total bound left out and nothing about sizes or limits.

The write path works. A probe driving an over-bound save through `agenttool`
shows the model receiving `Error: memory: entry user/style is 4200 bytes, over
the 4096 byte limit; split it or trim it`, because `agenttool` renders a tool
error in exactly that form (agenttool `123291f`, `tool.go:99`). The message
names only the attempted size, so the model still cannot tell how much room the
entry had.

Letta, at tag 0.16.8, renders both numbers per block inside the block itself,
as `<metadata>- read_only=true - chars_current={len(value)} -
chars_limit={limit}</metadata>`, and checks nothing on write. Claude Code's
auto memory bounds its index at the first 200 lines or 25 KB, lets an
over-limit write land, and then returns an error telling the model to rewrite
the index, with a warning while the file is merely near the limit. The plan is
right to refuse where they do not; it is alone in refusing without having shown
the budget.

### Failure scenario

The model has been adding to a note for weeks and it stands at 4,050 bytes. It
composes a 4.1 KiB replacement and is refused, guesses at a trim and is refused
again at 4,100 bytes, and succeeds on the third try. That is three turns, and
nothing the model could see named the stored size.

### Suggested fix

`Render` writes each entry's current size and its limit into that entry's
heading, and the block's own header states the total budget and what remains.
The store's over-bound error names the stored size as well as the attempted
one, so the trim is arithmetic rather than a guess.

It must not spend much window doing it. Two numbers per entry and two for the
block, not a table. It must not make the numbers something a product can turn
off, because `Render`'s output has to stay fully determined by the store's
state and the bounds, or the golden fixtures and the manifest hashes mean
nothing.

Found by the letta-memory design study against Letta.

## 4. filestore: the write lock names no holder, has no deadline and is never taken over


### Why this is necessary

The plan serialises writes to a scope with a lock file and says nothing else
about it, so the obvious implementation leaves the file behind when its holder
dies. Every later write then waits forever on a lock nobody holds, with nothing
to say who held it and no deadline to give up at. Because a memory write
happens inside a tool call, the model's turn waits with it, so a crashed daemon
comes back silent. The sibling module already solved this for the same
situation.

### Evidence

agentmemory has no repository yet; citations are to the plan as written.
`docs/plans/memory-layer.md:149` is the whole specification: writes are "atomic
(write, fsync, rename) and serialised with a lock file, so two sessions writing
the same scope do not tear a file". There is no mention of the holder's
identity, of a wait bound, or of a lock whose holder is gone.

A probe that builds that filestore, writes a lock file by hand to stand in for
a crashed writer, and then calls `Put` reports the call still waiting after 250
ms with no holder named and no deadline. It returns only when the file is
removed out of band.

The answer exists next door. In agentsession `65a5714`, `jsonl`'s lock writes
the holder's identity and a second process on the same session gets a typed
`ErrSessionLocked` naming the holder's PID, while a lock left by a dead PID on
the same host is taken over silently (`jsonl/lock.go:14`, `jsonl/lock.go:36`,
`jsonl/lock.go:63`, `jsonl/lock.go:98`). A memory store shared by four channels
in one daemon, plus a CLI the user runs by hand, is the same problem.

### Failure scenario

The daemon is killed while writing an entry. On restart, the first
`memory_save` on that scope blocks. The tool call blocks, the turn blocks, and
the channel stops answering. No error is logged, because nothing failed. The
process is healthy and the assistant answers nothing.

### Suggested fix

Specify the lock, mirroring `agentsession/jsonl`: the file holds the holder's
PID and host, a blocked writer returns a typed error naming the holder rather
than waiting indefinitely, a lock whose PID is dead on the same host is taken
over, and the wait is bounded by the caller's context so an aborted run does
not hang.

It must not import `agentsession`. The root module depends on `openresponses`,
`agenttool` and the standard library, and `filestore` on the standard library
alone. A second implementation tested against the same cases is the intended
answer, and the plan should say so rather than leave the reader to invent the
weak version.

Found by the letta-memory design study against Letta.

## 5. store: Journal takes a wall-clock cursor, which cannot order two writers


### Why this is necessary

`Journal` answers "what did memory hold at turn N" without the session, and a
process tailing the store reads it to notice what another channel wrote. Its
only cursor is a timestamp, so two writes in the same millisecond cannot be
ordered and a resume either re-delivers a record or skips one, depending on a
boundary the plan does not specify. A single writer never hits it. With memory
shared across several channels, the module's own case, it is a race that passes
every test and drops a notification in production.

### Evidence

agentmemory has no repository yet; citations are to the plan as written.
`Journal(ctx context.Context, since time.Time) iter.Seq2[Change, error]` is at
`docs/plans/memory-layer.md:85`, and `Change.At time.Time`
(`docs/plans/memory-layer.md:93`) is the only ordering field on a record.
Nothing says whether `since` is inclusive or exclusive, and there is no
sequence to fall back on when two records share an instant.

A probe that runs two writers against one filestore reports no tie on this
machine, because the records landed microseconds apart. Nothing in the contract
makes that spacing reliable.

Letta keyed its equivalent on a sequence. At tag 0.16.8, `block_history` rows
are unique on `(block_id, sequence_number)` and
`block.current_history_entry_id` names the current one, so a reader walks the
chain rather than a clock.

### Failure scenario

A notifier tails the journal to invalidate one channel's cached render when
another channel writes. Two writes land in the same millisecond. The notifier
resumes from the later timestamp and never sees the earlier record, so that
channel renders a block missing one fact for the rest of its session, and the
model answers from it.

### Suggested fix

`Change` gains `Seq uint64`, monotonic within the store, and `Journal` takes it
as an opaque cursor rather than a time. `filestore` derives it from the
journal's record count under the lock it already holds; `sqlite` from a rowid.
`At` stays, for display and for the record.

It must not become a global ordering promise across stores, only within one.
This is the same change as the `Prev` field in the lost-update issue. Together
they make the journal a chain, which also makes compacting it tractable,
because a reader can then be told to drop everything before a sequence number
that has a full state at it.

Found by the letta-memory design study against Letta.

## 6. tools: memory_search output is permanent and its limit has no default


### Why this is necessary

Search makes the render bound survivable, because what the bound leaves out has
to stay reachable. A tool is the right place for it, since a tool call and its
output are recorded, replayable and covered by the request hash. The cost is
that the output never leaves. The rendered block is replaced on every turn; a
search result is appended once and stays for the rest of the session, and an
unbounded default means one call can put more text in the window permanently
than the whole in-context block is allowed to hold.

### Evidence

agentmemory has no repository yet; citations are to the plan as written.
`memory_search` "takes a query, optional scopes and a limit, and returns
matching entries as text, name and scope first"
(`docs/plans/memory-layer.md:119`). No default limit is given, and nothing caps
the size of a result, while `Render` is held to `MaxTotalBytes`, 32 KiB
(`docs/plans/memory-layer.md:105`). An entry may be up to 4 KiB, so an
unbounded search over eight matches can return more than the in-context block's
whole budget, once, forever.

In agentturn `2fd849e` the loop resolves tools once per turn and the same list
serves the turn's batch (`config.go:61`, `run.go:365`), so a search tool
records as an ordinary `function_call` and output; a `Transform` that injected
results instead would record nothing and break `Verify`, which is filed against
agentturn.

### Failure scenario

The block omits six entries under its total bound, so the model searches. The
default limit is zero and means unbounded, five 4 KiB entries come back, and 20
KiB of memory is now pinned in the transcript beside a 32 KiB block capped to
avoid that. Two more searches and the pinned results exceed the block's whole
budget.

### Suggested fix

Give `memory_search` a documented default limit, low, and a per-result and
total byte cap of its own so a result cannot exceed the in-context budget.
Letta's nearest equivalent defaults to ten results, which is generous against
32 KiB. Add one line to the plan's non-goals or invariants saying search output
is permanent in the transcript, so a product running long sessions knows it
wants `agentturn/compact` configured, whose folds are recordable.

It must not become a `Transform` that prunes old results. Pruning has a
recordable form only through a fold, and that belongs to the product's
compaction, not to this module.

Found by the letta-memory design study against Letta.

