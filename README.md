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
	Put(ctx context.Context, e Entry, opts ...PutOption) error
	Forget(ctx context.Context, scope Scope, name string) error
	Search(ctx context.Context, scopes []Scope, query string, limit int) ([]Entry, error)
	Journal(ctx context.Context, after uint64) iter.Seq2[Change, error]
	MaxEntryBytes() int
}
```

`Put` creates or replaces the whole entry and is the store's only
write, so the journal is a list of full states, each with its hash.
`IfHash(h)` makes a `Put` conditional on the stored hash, `""` meaning
the entry must not exist, so a write built on a stale read fails with
`ErrConflict` instead of clobbering. `Forget` writes a tombstone that
keeps the last content. Every `Change` carries a `Seq` that orders the
journal within the store, the `Prev` hash it replaced, and the
`Session` that wrote it, from `WithSession(ctx, id)`, so a reader can
chain records and see a fork. Content over the bound is refused with a
`SizeError` naming the attempted size, the limit and what the entry
holds now.

## Wiring

```go
mem, err := filestore.Open(filepath.Join(home, "memory"))
scopes := []agentmemory.Scope{"user", "project"}

block, manifest, err := agentmemory.Render(ctx, mem, scopes)
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
		record(m) // into a custom session entry under agentmemory:render
		return nil
	},
}
ctx = agentmemory.WithSession(ctx, sessionID) // attributes the journal
```

`Render` produces the block: a header with the counts and the budget,
one section per scope, one heading per entry with its size and the
limit and its description, then the content verbatim. Entries are
included in list order until the next would take the content total
over `MaxTotalBytes` (32 KiB by default); the rest are listed under
their scope so the model knows what `memory_search` can fetch. The
output is determined by the store's state and the bounds alone, so an
unchanged store renders the same bytes, and the `Manifest` lists what
the block held and omitted, by scope, name, hash and size, for the
session's provenance.

```
# Memory

Entries: 2 shown, 0 omitted. Used: 41 of 32768 bytes (32727 free). Entry limit: 4096 bytes.

## user

### style (28 of 4096 bytes) — How the user likes answers

Short answers.

Code in Go.

### timezone (13 of 4096 bytes)

Europe/London
```

## The tools

`Tools(store, scopes)` returns four tools restricted to the scopes the
product allows; a scope outside the list is an error the model sees,
and a call that omits the scope uses the first.

| tool | arguments | does |
|---|---|---|
| `memory_save` | `scope`, `name`, `content`, `meta` | create, or replace whole |
| `memory_patch` | `scope`, `name`, `old_text`, `new_text` | replace one exact occurrence |
| `memory_forget` | `scope`, `name` | write a tombstone |
| `memory_search` | `query`, `scopes`, `limit` | find entries the block omits |

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
