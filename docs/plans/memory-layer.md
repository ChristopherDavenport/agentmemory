# Plan: memory layer

Instructions the agent writes. Skills and AGENTS.md are instructions
people author and the agent reads; memory is what the agent keeps
between sessions about the user, the project and its own past work.
It needs a store with a write path, history and scoping, which no
read-only source has, and both dex (project memory beside the
checkout) and dexclaw (per-user memory shared across channels) want it,
so it is its own module rather than a corner of `agentskill`.

The reference shapes are Letta's memory blocks, bounded text that sits
in the window and the agent edits through tools, with archival memory
behind a search tool, and Claude Code's memory directory, one fact per
Markdown file with an index loaded each session. This plan takes the
file as the unit and the block's size bound, so memory is readable and
diffable by the user and the window stays predictable. The Letta study
decides the rest; see Open questions.

## Goals

- A store contract for named, scoped entries with an append-only
  journal, so "what did memory hold at turn N" is answerable without
  the session.
- The agent changes memory only through tools built with `agenttool`,
  so every write is a function call in the transcript.
- The always-in-context part renders into `Config.Instructions` with a
  bound per entry and in total; the rest is reachable through a search
  tool.
- A file-backed reference store the user can open, edit and commit.
- A manifest of what was rendered, with content hashes, for the
  session's provenance.
- Root depends on `openresponses`, `agenttool` and the standard
  library. Retrieval with a dependency is a nested module.

## Non-goals

- The conversation history. That is the session tree; searching past
  sessions is `agentsession`'s store, not a memory store.
- Deciding what is worth remembering. That is instructions to the
  model, and a product writes them.
- Embeddings or a vector index in the root. A nested module may add
  them behind the same `Search`.
- Cross-user sharing or access control. A store is opened for one
  scope set by the product; policy about who may read it is the
  product's.

## Module and packages

Separate module, `github.com/ChristopherDavenport/agentmemory`.

```
agentmemory/                 Entry, Scope, Store, Journal, the tools, Render, Manifest
agentmemory/filestore        one Markdown file per entry plus an index; the root package and the standard library only
agentmemory/sqlite           a Store with full-text search (nested module: modernc.org/sqlite)
agentmemory/storetest        the conformance suite every Store must pass, as agentsession/storetest
```

`agentturn` is never imported. A product renders memory into its
config and adds the tools to its tool list.

## Core types

```go
// Scope says whose memory an entry is. Products define the values;
// "user", "project" and "session" are the conventional three.
type Scope string

// Entry is one remembered thing.
type Entry struct {
    Scope   Scope
    Name    string            // kebab-case, unique within the scope
    Content string            // Markdown, bounded by the store's limit
    Meta    map[string]string // description, type, whatever the product adds
    Hash    string            // "sha256:..." of Content
    Updated time.Time
    Deleted bool              // a tombstone; the journal keeps the last content
}

type Store interface {
    Get(ctx context.Context, scope Scope, name string) (*Entry, error)
    List(ctx context.Context, scope Scope) ([]Entry, error)          // live entries, by name
    Put(ctx context.Context, e Entry, opts ...PutOption) (*Change, error)  // create or replace; appends and returns the record
    Forget(ctx context.Context, scope Scope, name string) (*Change, error) // writes a tombstone, returns the record
    Search(ctx context.Context, scopes []Scope, query string, limit int) ([]Entry, error)
    Journal(ctx context.Context, after uint64) iter.Seq2[Change, error] // records with Seq > after
    MaxEntryBytes() int                                              // the bound Put enforces
}

// IfHash makes a Put conditional: it fails with ErrConflict when the
// stored content hash is not h, so a write built on a stale read fails
// instead of clobbering. "" means the entry must not exist.
func IfHash(h string) PutOption

// BasedOn records the hash the write was built on without enforcing
// it, for a writer that composes the whole entry from a read and means
// to land on whatever is there.
func BasedOn(h string) PutOption

// Change is one journal record: the entry as it was after the change,
// what it replaced, and who made it.
type Change struct {
    Seq      uint64    // monotonic within the store; the Journal cursor
    Entry    Entry
    Prev     string    // hash of the content the write was built on, "" for a create
    Replaced string    // hash of the content it landed on, "" for a create
    Session  string    // the session ID that wrote it, "" for a person
    At       time.Time // for display and the record; never an ordering
}
```

`Put` is create-or-replace of the whole content and the store's only
write, so the journal stays a list of full states, each independently
hashable. The model does not patch by reading, editing and putting:
that write is paid three times (the call arguments, the transcript
that keeps them, the config delta that re-renders the block) and it
silently loses a concurrent edit, because the new value is built from
a read that may be stale. Editing is the `memory_patch` tool's job,
below; the store applies the patch and `Put` remains the unit.

`IfHash` is optional. A create and a deliberate overwrite are both
legitimate; the precondition is for callers that anchor a write in
what they read. `Replaced` and `Seq` make the journal a chain: a reader
follows each record to the one it replaced. `Prev` is the writer's own
claim about the state it composed from, which is what makes a fork
visible: a record whose `Prev` is not its `Replaced` is a write built
on a state another writer had already replaced, and `LostUpdates` lists
them. A store fills `Prev` from its own value when the caller claims
none, so an unanchored write is indistinguishable from an ordinary
edit; `memory_save` therefore names its base with `BasedOn`. `Seq` orders records within one store and promises nothing
across stores. `Journal(after)` is exclusive, so a reader resumes with
the last `Seq` it saw; `0` reads from the beginning. The session ID is
carried on the context, `WithSession(ctx, id)`, because one store
serves many sessions at once.

### Bounds

A store has `MaxEntryBytes` (default 4 KiB) and `Render` has
`MaxTotalBytes` (default 32 KiB). A `Put` over the entry bound fails
with the attempted size, the limit and the size the entry holds now,
so the model is told to split or trim rather than have its write
silently cut, and the trim is arithmetic rather than a guess. `Render`
includes entries in index order while they fit, skipping one that does
not and going on to the next, and lists what it left out, so the model
knows what it can fetch and one large entry cannot hide the small ones
after it. The bound is rendered as well as enforced: each entry's
heading carries its size and the limit, and the block's header states
the total budget and what remains, so the model can see the wall before
it hits it. The bound is on the block and not on the content it holds:
the header, the headings, the descriptions and the list of omissions
are counted, because the window pays for them, and the header reports
the block's own size. The manifest's omissions carry the reason, so a
session can record what the model was not given.

### The tools

Built with `agenttool.New`, four tools, names prefixed `memory_`:

- `memory_save` takes scope, name, content and optional meta, and
  creates or replaces the whole entry. It returns the stored hash and
  size. Over-bound content is an error naming the limit and the stored
  size.
- `memory_patch` takes scope, name, `old_text` and `new_text`, and
  replaces one exact occurrence of `old_text` in the stored content.
  It refuses when `old_text` is absent or appears more than once and
  says which. The tool description tells the model to prefer it over
  `memory_save` for an existing entry: the call is the size of the
  edit, and the edit is anchored in the stored value, so an edit whose
  anchor another session removed fails out loud. The tool reads, applies
  the patch and puts with `IfHash` of what it read, retrying on a
  conflict, so two sessions patching one entry keep both edits.
- `memory_forget` takes scope and name and writes a tombstone.
- `memory_search` takes a query, optional scopes and a limit, and
  returns matching entries as text, name and scope first. The limit
  defaults to 5, and the result is bounded in bytes (default 16 KiB,
  half the render budget); matches over the byte bound are listed by
  name. Search output is permanent: the rendered block is replaced
  each turn, but a tool result stays in the transcript for the rest
  of the session, so a product running long sessions wants
  `agentturn/compact` configured.

`Tools(store, scopes)` returns the four restricted to the scopes the
product allows; a scope outside the list is an error the model sees,
and a call that omits the scope uses the first one listed.

Each tool returns an `agenttool.Result`. A write sets `Details` to a
`WriteRecord`, the journal record the write produced, which implements
`agenttool.Recordable` under `WriteNS`, so a recorder writes it beside
the call without knowing the type and a session joins to the store's
journal without re-reading it. The model sees the output line alone.

### Rendering

```go
// Render returns the in-context block: one section per scope, one
// entry per section, in index order, bounded as above.
func Render(ctx context.Context, s Store, scopes []Scope, opts ...RenderOption) (string, Manifest, error)

// Manifest lists what Render included, for the session's provenance.
type Manifest struct {
    Entries []ManifestEntry // scope, name, hash, bytes
    Omitted []ManifestEntry // left out, same fields plus the reason
}
```

The block is Markdown: a header line with the counts and the budget,
one `##` section per scope, one `###` heading per entry carrying the
entry's size and the limit, the description under it, then the content
verbatim. `Usage` is one paragraph telling the model how to use the
tools, which a product appends when it offers them.

The product puts the block into `Config.Instructions` before each run,
and re-renders it in `Config.BeforeModelCall` when it wants the
freshest state each turn. Not in a `Transform`: a `Transform` cannot
reach the instructions and an item it prepends is never recorded, so
the session then fails `Verify`. A change to memory mid-session shows
up as a config delta on the next turn, which is the record of what the
model was reminded of and when. The manifest goes into a `custom`
entry under `agentmemory:render`; the `env` entry's file hashes cannot
carry the omitted names, the sizes, or a store without paths.

### `filestore`

One directory per scope, one `<name>.md` per entry with a frontmatter
of `name`, `updated` and any meta, and an `INDEX.md` per scope that
lists every live entry with its description, kept in sync on every
write. The journal is one append-only `journal.jsonl` at the root, so
one sequence orders every write to the store whatever the scope.
Writes are atomic (write, fsync, rename) and serialised with one lock
file at the root, so two sessions writing the store do not tear a file
or a sequence number; a `Put` without `IfHash` is last write wins on
the entry, and the journal keeps both with `Prev` naming what each
replaced. A tombstone removes the file and records the last content in
the journal. `Seq` is the last journal record's plus one, read under
the lock.

The lock is specified, mirroring `agentsession/jsonl`: the file holds
the holder's PID, host and start time; a writer waits for a live
holder, bounded by the caller's context and a store-level timeout, and
then returns a typed `ErrLocked` naming the holder rather than waiting
forever; a lock whose PID is dead on the same host is taken over,
exclusively, so that of several writers finding one dead holder's lock
exactly one removes it and the others wait for the lock it then holds;
a lock from another host is never taken over silently, and `BreakLock`
is the deliberate way. That is a second implementation of the same
lock, tested against the same cases, because this package cannot
import `agentsession`.

A person's edit to a file is not a `Put` and is not journaled as it
happens. `Reconcile` compares the files with the journal's last state
and journals each difference as a change by nobody, `Session` empty,
so a product that lets people edit the directory runs it before it
renders.

The directory is a valid `agentskill.Source` in reverse: a person can
read it, and git can diff it. That is the transparency argument for
files over a database as the reference store.

## Conventions shared with the siblings

The same three rules that `agentskill`, `agentpolicy` and `agenteval`
follow, written once here so this plan stands alone:

1. One decision vocabulary: the loop's Allow, Block and Defer. This
   module makes no decisions.
2. Sources through `fs.FS` with a manifest of locations and content
   hashes. `Manifest` is that for memory.
3. Recording through entries the session format already has. The
   rendered block is in the instructions and so in config deltas.
   Writes are function calls. The manifest goes into an `env` entry's
   file hashes or a `custom` entry under `agentmemory`; nothing is
   added to the RFC.

## Invariants

- Every change to a store is in its journal, tombstones included.
- `Render` output is fully determined by the store's state and the
  bounds; the manifest hashes are the hashes of the included content.
- A `Put` never stores content over the entry bound.
- `filestore` imports the root package and the standard library only.
- The model can only reach the store through the tools.
- Search output is permanent in the transcript; only the rendered
  block is replaced.

## Testing

`storetest` runs the same table against `filestore`, `sqlite` and an
in-memory store: validation, bounds, `IfHash` conflicts, tombstones,
journal order and chaining, concurrent writers from several goroutines,
and from two processes for `filestore`. `Render` has golden fixtures.
The tools are run under `agentturn` with the `echo` adapter in a test
that imports the loop as a test dependency only.

## Milestones

1. `Entry`, `Store`, `Change`, an in-memory store, `storetest`.
2. `filestore` with the journal, the lock and the atomic write.
3. The three tools and `Tools`.
4. `Render` and `Manifest` with golden fixtures.
5. `sqlite` with full-text `Search`, passing `storetest`.
6. dexclaw uses user scope across two channels; dex uses project
   scope beside the checkout.

## Open questions

- Whether the unit is a file or a Letta-style block with a fixed
  slot. Files here; the Letta study reopens it if slots turn out to
  matter for the model's editing behaviour.
- Whether tombstones should ever be compacted out of the journal, and
  by whom. The journal is unbounded here. With `Seq` and `Prev` it is
  tractable: drop everything before a sequence number that has a full
  state at it.

Closed by the design studies (`../feedback.md`): `Put` takes no patch,
the patch is the `memory_patch` tool's and the store applies it; a
memory written by one session is attributed through the journal, whose
record carries the writing session's ID, the hash and the hash it
replaced, so the manifest's hash resolves to a record and a writer.
