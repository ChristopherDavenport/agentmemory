# agentmemory

Memory for Go agents over
[Open Responses](https://www.openresponses.org): what the agent keeps
between sessions about the user, the project and its own past work, in
a scoped store with an append-only journal, changed only through tools
built with [agenttool](https://github.com/ChristopherDavenport/agenttool),
and rendered into the instructions with a manifest of what the model
was shown. A product wires the block and the tools into its
[agentturn](https://github.com/ChristopherDavenport/agentturn) config;
the loop is never imported here.

- The root module depends on `openresponses`, `agenttool` and the
  standard library.
- `filestore` is the reference store: one Markdown file per entry,
  which a person can open, edit, diff and commit. It imports the root
  package and the standard library.
- `sqlite` is a nested module on `modernc.org/sqlite` with full-text
  search.
- `storetest` is the conformance suite every store passes.

## Install

```sh
go get github.com/ChristopherDavenport/agentmemory
go get github.com/ChristopherDavenport/agentmemory/sqlite   # optional
```

Go 1.25 or later.

## The shape

An `Entry` is one remembered thing: a kebab-case `Name` unique within
a `Scope` (products define the scopes; `user`, `project` and `session`
are the conventional three), Markdown `Content` bounded by the store's
limit (4 KiB by default), and `Meta` such as a description. A `Store`
holds entries and a journal of every change:

```go
type Store interface {
	Get(ctx context.Context, scope Scope, name string) (*Entry, error)
	List(ctx context.Context, scope Scope) ([]Entry, error)
	Put(ctx context.Context, e Entry, opts ...PutOption) (*Change, error)
	Forget(ctx context.Context, scope Scope, name string) (*Change, error)
	Search(ctx context.Context, scopes []Scope, query string, limit int) ([]Entry, error)
	Journal(ctx context.Context, after uint64) iter.Seq2[Change, error]
	MaxEntryBytes() int
}
```

`Put` creates or replaces the whole entry and is the store's only
write, so the journal is a list of full states, each with its hash. It
returns the journal record it appended, so a write can be recorded
beside the call that made it.
`IfHash(h)` makes a `Put` conditional on the stored hash, `""` meaning
the entry must not exist, so a write built on a stale read fails with
`ErrConflict` instead of clobbering. `Forget` writes a tombstone that
keeps the last content. Every `Change` carries a `Seq` that orders the
journal within the store, the `Session` that wrote it, from
`WithSession(ctx, id)`, and two hashes: `Replaced`, the state the write
landed on, which chains the records, and `Prev`, the state it was built
on, which the caller names with `IfHash` or, without making it a
precondition, with `BasedOn`. A record whose `Prev` is not its
`Replaced` is a write composed from a state another writer had already
replaced, and `LostUpdates(ctx, store, after)` lists them, so an
auditor can say what a session discarded. Content over the bound is refused with a
`SizeError` naming the attempted size, the limit and what the entry
holds now.

## Wiring

```go
mem, err := filestore.Open(filepath.Join(home, "memory"))
scopes := []agentmemory.Scope{"user", "project"}

block, manifest, err := agentmemory.Render(ctx, mem, scopes)
var recorded string // the last manifest this session wrote
cfg := agentturn.Config{
	Model:        client,
	Instructions: prompt + "\n\n" + block + "\n\n" + agentmemory.Usage(),
	Tools:        append(tools, agentmemory.Tools(mem, scopes)...),
	// The freshest state each turn. Not a Transform: a Transform
	// cannot reach the instructions and what it injects is not
	// recorded.
	BeforeModelCall: func(ctx context.Context, req *openresponses.Request) error {
		b, m, err := agentmemory.Render(ctx, mem, scopes)
		if err != nil {
			return err
		}
		req.Instructions = prompt + "\n\n" + b + "\n\n" + agentmemory.Usage()
		// Record the manifest when it has moved. The render is a pure
		// function of the store and the bounds, so most turns produce
		// the manifest the turn before produced, and the recorder
		// compares nothing for an annotation.
		if h := m.Hash(); h != recorded {
			recorded = h
			ns, data := m.Record() // agentmemory:render, and the JSON
			return rec.Annotate(ctx, ns, json.RawMessage(data))
		}
		return nil
	},
}
ctx = agentmemory.WithSession(ctx, sessionID) // attributes the journal
```

The module never imports the loop or the session format, so the call
that writes the entry is the product's; the namespace and the bytes are
the module's, through `ManifestNS` and `Manifest.Record`, so a reader
of the session recognises the entry without knowing the product. Each
omission in the manifest carries its scope, name, size and reason, so
the record says what the model was not given and why.

`Render` produces the block: a header with the counts and the budget,
one section per scope, one heading per entry with its size and the
limit and its description, then the content verbatim. `MaxTotalBytes`
(32 KiB by default) bounds the block itself, header and headings and
descriptions included, and the header reports the block's own size. An
entry whose rendered form does not fit is skipped and the next is
still considered, so one large entry cannot hide the small ones after
it; what was left out is listed under its scope so the model knows what
`memory_search` can fetch. The output is determined by the store's
state and the bounds alone, so an unchanged store renders the same
bytes, and the `Manifest` lists what the block held and omitted, by
scope, name, hash, size and, for an omission, the reason, for the
session's provenance.

```
# Memory

Entries: 2 shown, 0 omitted. Block: 251 of 32768 bytes (32517 free). Entry limit: 4096 bytes.

## user

### style (28 of 4096 bytes) — How the user likes answers

Short answers.

Code in Go.

### timezone (13 of 4096 bytes)

Europe/London
```

## The tools

`Tools(store, scopes)` returns four tools restricted to the scopes the
product allows. The list is in each tool's schema as an enum on
`scope`, not only in its description, and with more than one scope
`scope` is required, so a call that omits it is an error the model can
read rather than a write into whichever scope the product listed
first. With one scope there is nothing to choose and the argument may
be left out. A scope outside the list is an error the model sees.

| tool | arguments | does |
|---|---|---|
| `memory_save` | `scope`, `name`, `content`, `meta` | create, or replace whole |
| `memory_patch` | `scope`, `name`, `old_text`, `new_text` | replace one exact occurrence |
| `memory_forget` | `scope`, `name` | write a tombstone |
| `memory_search` | `query`, `scopes`, `limit` | find entries the block omits |

Each write's result carries the journal record it produced as
`WriteRecord` in `agenttool.Result.Details`, which the model never
sees and a recorder writes beside the call under `WriteNS`
(`agentmemory:write`), so a session says which write produced the
memory and with what sequence number, hash and session.

`memory_save` replaces the entry, but a call that leaves `meta` out
keeps the metadata the entry has rather than deleting the description
the block and the index show; `{}` clears it, and the result says which
happened, what it replaced and what the write was built on. Its write
is anchored with `BasedOn`, so two channels that save one entry from
one state leave a journal `LostUpdates` can report.

`memory_patch` is the tool the model is told to prefer for an edit: the
call is the size of the change, and the edit is anchored in the stored
text, so an edit whose anchor another session removed fails out loud
and two sessions patching one entry keep both edits; the tool reads,
patches and puts with `IfHash`, retrying on a conflict. `memory_search`
returns five entries and 16 KiB by default, because a tool result
stays in the transcript where the block is replaced each turn.

## Stores

`filestore.Open(dir)` keeps one directory per scope, one `<name>.md`
per entry with a frontmatter of `name`, `updated` and the meta, an
`INDEX.md` per scope, and one `journal.jsonl` at the root:

```
memory/
  journal.jsonl
  user/
    INDEX.md
    style.md
```

Writes are atomic and serialised by a lock file that names its
holder, is taken over when the holder is dead on the same host, and is
waited for only so long (`ErrLocked`, `LockHolder`, `BreakLock`). The
takeover is exclusive: one writer of the several that find a dead
holder's lock removes it, so no two writers hold the store and take the
same sequence number.
`Reconcile` journals what a person changed by hand. `sqlite.Open(path)`
keeps the same rows and the same journal lines in one database and
searches an FTS5 index in relevance order. `NewMemStore()` is the
in-memory store. All three pass `storetest.Run`.

## Development

```sh
make check    # gofmt, tidy, vet, deps, staticcheck, govulncheck, race tests, every module
```

`Render` has golden fixtures under `testdata/render/`; regenerate with
`go test . -update` and review the diff. See `CONTRIBUTING.md`.

## License

MIT. See `LICENSE`.
