# Changelog

All user-visible changes to this library. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
uses [Semantic Versioning](https://semver.org/); before v1.0.0 minor
versions may break the API.

## Unreleased

### Breaking

- `Change.Prev` is now the hash the write was built on, not the hash the
  store held: the base the caller named with `IfHash` or the new
  `BasedOn`, or, when the caller named none, the stored hash, which is
  what it was before. The hash a change replaced moved to the new
  `Change.Replaced`, which is what records chain through, so a reader
  that walked `Prev` to chain the journal reads `Replaced` now (#1).

- `Render`'s `MaxTotalBytes` bounds the block, not the content of the
  entries it holds: the header, the scope headings, the per-entry
  headings with their descriptions and the list of what was left out
  are all counted, and the header reports the block's own size as
  `Block: n of m bytes (k free)` rather than the content total as
  `Used:`. A store of many short entries rendered several times its
  budget before; the same store now renders inside it and shows fewer
  entries (#3).
- `Render` skips an entry that does not fit and goes on to the next
  instead of omitting it and everything after it, so a large entry no
  longer hides the smaller ones that sort after it. Block order is
  still list order (#8).

- `Store.Put` and `Store.Forget` return the journal record they
  appended, `(*Change, error)`, so a caller records a write without
  reading the journal back. A refused write returns nil. Every store
  and any store elsewhere takes the new signature, and `storetest`
  checks that the record returned is the record the journal holds (#6).
- The four tools return an `agenttool.Result` rather than a string, so
  a write can carry its record; the text the model sees is unchanged
  (#6).
- The module requires `agenttool` v0.0.5 for `Recordable` (#6).

- `memory_save` keeps the entry's metadata when the call leaves `meta`
  out, instead of deleting the description the rendered block, the
  index and `Search` use; `{}` clears it. The tool reads the entry
  first and anchors the write with `BasedOn`, so the result says what
  it replaced and what became of the metadata, and the journal shows a
  save that landed on another channel's write (#4).

### Added

- `Manifest.Hash`, `ManifestNS` (`agentmemory:render`) and
  `Manifest.Record`: the manifest has an identity, so a product records
  it when the render has moved rather than writing a byte-identical one
  every turn, and under a namespace a reader recognises without knowing
  the product. The README documents the pattern (#9, #10).
- `WriteRecord` and `WriteNS` (`agentmemory:write`): a memory write's
  result carries the journal record it produced as
  `agenttool.Result.Details`, which implements `agenttool.Recordable`,
  so a recorder writes it beside the call under a namespace a reader
  recognises and the model never sees it (#6).
- `ManifestEntry.Reason` on an omitted entry, with the `OmitBudget`
  constant, so a session can record why the model was not given an
  entry (#3, #8).
- `BasedOn(hash)`, a `PutOption` that records the hash a write was
  built on without making it a precondition, and
  `PutOptions.BaseFor(stored)` for a store to resolve it (#1).
- `LostUpdates(ctx, store, after)` and `LostUpdate`: the journal records
  whose `Prev` is not the state they replaced, which is a write composed
  from a state another writer had already replaced, or an entry changed
  outside the journal. `storetest` holds every store to it (#1).

### Fixed

- `filestore`: the takeover of a lock whose holder is dead is atomic.
  A writer now links the lock to a name derived from the dead holder's
  identity, which only one writer's link can create, and proves it
  linked the lock it inspected before removing it, so two writers that
  find one dead holder's lock no longer both take it over, both read
  the same next sequence number from the journal's tail and both append
  it. `BreakLock` also removes a claim a taker left behind (#2).
- `sqlite`: `Open` sets the busy timeout before the pragma that takes a
  lock on the database and applies the schema inside the writer's
  immediate transaction, so two processes opening one database at once
  queue on the write lock instead of one of them failing with
  `SQLITE_BUSY`. The sequence number was already allocated inside the
  transaction that writes the record, and the column is the journal
  table's primary key, so sqlite never had filestore's race; a test
  now holds it to that (#2).

## v0.0.1 - 2026-09-20

- Initial release: `Scope`, `Entry`, `Change` and the `Store` contract
  with `IfHash`, a sequence-cursored `Journal` whose records chain
  through `Prev`, and session attribution through `WithSession`;
  `CheckEntry`, `Hash`, `Match` and the typed `SizeError` and
  `ConflictError`; `NewMemStore`; `Tools` returning `memory_save`,
  `memory_patch`, `memory_forget` and `memory_search` over the scopes a
  product allows; `Render` with `Manifest` and `Usage`; the `filestore`
  package with atomic writes, a holder-naming lock, per-scope indexes,
  a JSONL journal and `Reconcile`; the `sqlite` nested module with
  FTS5 search; and the `storetest` conformance suite.
