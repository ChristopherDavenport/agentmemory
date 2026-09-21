# Changelog

All user-visible changes to this library. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
uses [Semantic Versioning](https://semver.org/); before v1.0.0 minor
versions may break the API.

## Unreleased

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
