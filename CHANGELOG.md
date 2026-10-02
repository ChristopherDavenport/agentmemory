# Changelog

All user-visible changes to this library. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
uses [Semantic Versioning](https://semver.org/); before v1.0.0 minor
versions may break the API.

## Unreleased

### Added

- `ManifestFold.Record(m)` returns the smallest record of a manifest
  over the manifests the fold holds: the whole, or the delta on
  whichever of the last `ManifestFoldDepth` manifests in force gives
  the smaller, a tie going to the one in force. A writer that has
  folded the session's path but kept no manifest of its own, as a kit
  taking a session up after a restart has, wrote `RecordSince` the
  manifest in force, the other agent's, and so the whole manifest at
  its first hand-back; the fold held its own last manifest all along
  (agentkit#68).

### Fixed

- `filestore`: a journal whose last complete line was not a record, as
  two writes cut off in a row leave it, refused every later write with
  "journal's last record is not a change", for ever, since nothing
  repairs the journal. The next sequence number is now read from the
  last line that is a record, walking back past damaged lines as
  readers already did (#12).
- `filestore`: a journal line that decoded but carried no `seq`, such
  as `{}`, counted as record 0 and reset the cursor, so the next write
  took a number already used. A record has a positive `seq`; a line
  without one is damage, reported and skipped (#12).

### Documentation

- `docs/filestore-format.md` specifies the file store's on-disk format
  for a program that reads or appends to the directory beside the
  reference store: the layout and names, the entry file and its
  frontmatter, the journal record's members and encoding, how `seq` is
  allocated and recovered, what `prev` and `replaced` chain and what a
  fork is, tombstones, reconciled changes, the torn-tail and damaged
  line rules, the order and durability of a write, the lock and its
  takeover, the cursor, and the rules a second writer follows.
  `filestore/format_test.go` reads the document's examples and holds
  the package to them (#12).

## v0.0.9 - 2026-10-01

### Added

- `WithScopeMaxBytes(scope, n)` caps what one scope's entries, and the
  line naming its omissions, may take of the block, inside
  `WithMaxTotalBytes`. Every scope packed against the one bound in
  order, so a full scope starved the scopes after it: 20 environment
  entries under 3,575 bytes left no room for 2 user entries, and
  reordering moved the starvation onto the other scope. An entry a cap
  leaves out is `OmitBudget`; a cap under zero is refused with
  `ErrBudget`. Without it nothing changes (#38).
- `ManifestFold` folds a session's records under `ManifestNS` and
  resolves a delta whose base is any of the last `ManifestFoldDepth`
  (eight) distinct manifests in force, not only the current one. In a
  handoff between two agents with their own memories, the manifest in
  force at a hand-back is the other agent's, so `RecordSince` on it
  wrote the whole manifest every time: 84 KB per hand-back for 600
  entries. The agent's own last manifest is now a base a reader
  resolves, so a writer passes it to `RecordSince` when that delta is
  the smaller (#37).

### Changed

- The record format under `ManifestNS`: a delta may be based on any of
  the last `ManifestFoldDepth` distinct manifests in force.
  `ApplyManifestRecord` still resolves only the manifest in force and
  refuses such a delta with `ErrManifestBase`, so a writer records one
  only once the session's readers fold with `ManifestFold`.

### Dependencies

- agenttool v0.0.12 to v0.0.14, in the root module and in `sqlite`, and
  agentturn v0.0.13 to v0.0.15, which the tools' integration test alone
  depends on. No API of this module changes with them.

## v0.0.8 - 2026-10-01

### Breaking

- `Render` and `RenderParts` refuse a bound they cannot meet with the
  new `ErrBudget` and no block. `WithMaxTotalBytes(n)` with `n` under
  one meant the 32 KiB default, so a caller whose share of a budget
  came to zero or less, which is when there is no room, got the
  largest block the module renders; and a bound under what the title,
  the summary and one heading per scope take was exceeded without a
  word. Leaving the option out still means `DefaultMaxTotalBytes`
  (#17).

### Documentation

- `filestore.Reconcile`'s doc and the README say that it opens every
  entry file, so its cost grows with the number of entries, a few
  milliseconds at five hundred. The 44 µs v0.0.2 reported is the
  journal read from the cursor, not the call. Comparing modification
  times instead would miss an edit made in place, so the scan stays
  (#18).

### Dependencies

- agenttool v0.0.11 to v0.0.12, in the root module and in `sqlite`, and
  agentturn v0.0.12 to v0.0.13, which the tools' integration test alone
  depends on. No API of this module changes with them.

## v0.0.7 - 2026-09-29

### Added

- `Manifest.RecordSince(prev)` records a manifest as a delta on the
  last one the session recorded: `base` and `hash`, the two manifests'
  hashes, and `entries` and `omitted` with each run of entries that
  stayed in place and unchanged written `{"keep":n}` and the rest
  whole. `Manifest.Record` repeated every entry the block showed or
  left out, about 140 bytes each, on every write, so under 600 entries
  a 3 byte patch recorded 84 KB of manifest; its delta is one entry,
  about 350 bytes. It writes the whole record when that is no larger.
  `ApplyManifestRecord` folds a record of either form onto the manifest
  in force, and refuses a delta on another manifest with
  `ErrManifestBase`. The README's wiring records the delta after a
  session's first manifest (#33).
- `memory_search` claims `agenttool.ReplaySafe`, so a harness resuming
  a session runs again a search a crash cut off, where it answered it
  as possibly run with its output lost. The writers still claim
  nothing: a save run again records the session's own first run as a
  lost update, a patch run again fails or edits twice, and a forget run
  again removes what another session saved in between (#32).

### Dependencies

- agenttool v0.0.10 to v0.0.11, in the root module and in `sqlite`, and
  agentturn v0.0.11 to v0.0.12, which the tools' integration test alone
  depends on and which brings agentsession v0.0.15 to it. No API of this
  module changes with them.

## v0.0.6 - 2026-09-29

### Breaking

- **sqlite: the tables are renamed** `memory_entries`,
  `memory_journal` and `memory_entries_fts`, with the journal's index
  `memory_journal_entry`. agentsession's sqlite store also names its
  table `entries`, and both created theirs with `CREATE TABLE IF NOT
  EXISTS`, so on one shared file whichever opened second adopted the
  other's table: with sessions first the first `memory_save` failed with
  `no such column: content`, and with memory first the session store
  refused to open. The two now share a file. `Open` renames the tables
  of a database an earlier release wrote in its schema transaction,
  when all three have this store's columns and none of the new names
  exists; another program's `entries` is left alone. A release before
  this one that opens a migrated file creates empty unprefixed tables
  and does not see the renamed ones, so every process on a file moves
  together. Anything that queries the tables by name moves with them
  (#29).

### Added

- sqlite: `Open` fails, naming the table and its columns, when a table
  of one of the store's names has other columns, where before the
  mismatch surfaced as a column error at the first read or write.

### Dependencies

- agenttool v0.0.9 to v0.0.10, in the root module and in `sqlite`, and
  agentturn v0.0.10 to v0.0.11, which the tools' integration test alone
  depends on. No API of this module changes with them. modernc.org/sqlite
  stays at v1.59.0: v1.60 requires Go 1.26, above this module's floor.

## v0.0.5 - 2026-09-28

### Breaking

- **The rendered block changes.** The line with the counts and the
  budget, `Entries: … Block: n of m bytes (k free). Entry limit: …`,
  is the block's last line instead of its second, and the block no
  longer ends with a newline, so it is one byte shorter. The line
  changes on every write, and the block is the instructions, the prefix
  every provider's prompt cache keys on: with it first, a write
  invalidated the cached prefix at byte 83, before every entry, tool
  and item; now a write keeps every entry before the one it touched.
  Everything else in the block is byte for byte what it was, and
  `Manifest` does not change. Every golden that pins a rendered block,
  here or in a consumer, moves; a product that joined the block to
  what follows with a blank line still gets one. `Usage` says the
  budget is on the block's last line (#23).
- **`Render` is `RenderParts` joined.** `RenderParts(ctx, s, scopes,
  opts...)` returns the block as `[]Part`, `Part{ID, Text}`: the title
  (`memory`, `TitlePartID`), each scope heading (`memory/<scope>`), each
  entry (`memory/<scope>/<name>`, `PartID`), each scope's omission line
  (`memory/<scope>:omitted`) and the summary line (`memory:summary`,
  `SummaryPartID`), with the `Manifest`. `JoinParts` joins them with
  `PartSeparator`, one blank line, which is agentsession's
  `PartSeparator`; the type and the constant are this module's own,
  since it does not import the session format. A product recording its
  instructions as parts, through agentturn v0.0.10's
  `session.WithInstructionsParts`, hands agentsession these parts
  beside its own instead of parsing the block, and a write to one entry
  is recorded as that entry's part and the summary. An empty scope's
  `No entries.` is in its heading's part. A scope given twice, which
  would name its parts twice and which agentsession refuses, is now
  `ErrInvalid` from `Render` and `RenderParts` (#24; feeds agentturn
  #114 and agentkit).

### Added

- `WithRendered(func() Manifest)`: `memory_save` names the hash the
  rendered block showed for the entry as the write's base, rather than
  the hash of its own read. The model composed the content from the
  block, and a write another session made after the render is the one
  the save discards; with the option that save's `Prev` is the block's
  hash, so `LostUpdates` reports it, and the result line tells the model
  the entry changed after the render. An entry the block did not show,
  a create or one it omitted, is based on the save's own read as
  before. With the option set, the result line ends by saying which base
  the write took. `memory_patch` is unchanged; its edit is anchored in
  the stored text. A product sets the option to return the manifest of
  its last `Render` (#22).
- `WithReadScopes(scopes...)`: scopes the model may read and not write.
  They join `memory_search`'s `scopes` enum and are searched when a
  call names none, so an entry the block omitted from a scope the
  product renders but does not hand to `Tools` is reachable, as the
  block says. `memory_save`, `memory_patch` and `memory_forget` refuse
  them with `scope <name> is read-only`, which their descriptions state.
  `Tools` panics on a read scope that is not kebab-case, is given twice
  or is also writable, and now on a writable scope given twice (#26).
- The four tools carry `agenttool.Annotations`, each with a title:
  `memory_search` read-only, `memory_save` and `memory_forget`
  destructive, since a save replaces the entry whole, and
  `memory_patch` neither; none is open-world. The title keeps
  `memory_patch`'s annotations from being the zero value, which a host
  such as mcpserver serves as none, and MCP then defaults to destructive
  and open-world. The schema check the tools add is built on
  `agenttool.Wrap`, which forwards every property the tool declares,
  instead of an embedding that forwarded `Strict` and `Sequential` alone
  (#27).

### Changed

- `memory_search`'s description says every scope it reaches is searched
  when the call names none, where with more than one scope it said to
  name one on every call, which was the writers' rule; with one scope it
  says `scopes` may be left out. The argument has always been
  optional. The description, and so the tool's definition hash,
  changes.

### Fixed

- sqlite: `Open` on a database that does not exist yet no longer fails
  with `SQLITE_BUSY` when several processes open it at once. Each
  process's first connection switches the new file to WAL, and SQLite
  refuses that switch at once, without consulting the busy handler,
  while another process holds the file; `Open` now retries the schema
  transaction on `SQLITE_BUSY` with a short growing pause, for as long
  as the five second busy timeout would have waited. A new test opens
  a fresh path from four processes at the same instant, which the
  goroutine test could not reach (#25).

### Dependencies

- agenttool v0.0.8 to v0.0.9, in the root module and in `sqlite`, and
  agentturn v0.0.9 to v0.0.10, which the tools' integration test alone
  depends on.

## v0.0.4 - 2026-09-28

### Dependencies

- agenttool v0.0.7 to v0.0.8, in the root module and in `sqlite`, and
  agentturn v0.0.8 to v0.0.9, which the tools' integration test alone
  depends on. No API of this module changes with them.

## v0.0.3 - 2026-09-24

### Dependencies

- openresponses v0.0.9 to v0.0.12 and agenttool v0.0.5 to v0.0.7, in
  the root module and in `sqlite`, and agentturn v0.0.5 to v0.0.8,
  which the tools' integration test alone depends on. No API of this
  module changes with them.

## v0.0.2 - 2026-09-23

### Breaking

- `Store.Put` and `Store.Forget` return the journal record they
  appended, `(*Change, error)`, so a caller records a write without
  reading the journal back. A refused write returns nil. Every store,
  here or elsewhere, takes the new signature, and `storetest` checks
  that the record returned is the record the journal holds (#6).
- The four tools return an `agenttool.Result` rather than a string, so
  a write can carry that record; the text the model sees is unchanged.
  The module requires `agenttool` v0.0.5 for `Recordable` (#6).
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
  `Used:`. The free count is written at the width of the bound, padded
  with spaces, so that the header's width depends on the size it
  reports and on nothing else and the size it states is the block's own
  length. A store of many short entries rendered several times its
  budget before; the same store now renders inside it and shows fewer
  entries (#3).
- `Render` skips an entry that does not fit and goes on to the next
  instead of omitting it and everything after it, so a large entry no
  longer hides the smaller ones that sort after it. Block order is
  still list order (#8).
- The scopes a product allows are in each tool's JSON Schema as an
  enum, not only in the description, and `scope` is a required argument
  when there is more than one: a call that omits it is now an error
  naming the choices rather than a write into the first scope listed,
  which under the README's own split is the widest. With one scope the
  argument may still be left out. A call is checked against that schema
  before it runs, so a missing required argument is an error the model
  can read rather than an empty string: a `memory_patch` without
  `new_text` used to delete `old_text` (#5).
- `memory_save` keeps the entry's metadata when the call leaves `meta`
  out, instead of deleting the description the rendered block, the
  index and `Search` use; `{}` clears it. The tool reads the entry
  first and anchors the write with `BasedOn`, so the result says what
  it replaced and what became of the metadata, and the journal shows a
  save that landed on another channel's write (#4).
- `filestore.Reconcile` reads the journal from a cursor kept beside it
  in `.state.json` rather than whole, so a product that reconciles
  before every render stops paying for the store's whole history on
  every turn: reading a 4,000 record journal took 9.6 ms and reading
  from the cursor takes 44 µs, whatever the journal holds, and a run
  with nothing to do writes nothing. The file is derived: a missing or
  damaged one, one whose offset is past the journal's end, and one
  written against another journal, which it tells by the hash of the
  journal's first line, are all rebuilt from the journal (#7).
- A write through `filestore` records a person's edit to the entry it
  is about to replace, as a change by nobody with
  `Source: "reconciled"`, so nothing a person wrote is lost when the
  agent writes before anything reconciles, and the journal no longer
  ends up with a `Prev` naming a hash no record holds. The record waits
  for the entry file, so a write that fails still writes nothing (#7).

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
- `Change.Source` and `SourceReconciled`: where a change the store did
  not write came from (#7).
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
  it. `BreakLock` also removes a claim a taker left behind. A file
  system that gives no links, or a directory this process may not
  write, comes back as `ErrLocked` naming the holder rather than as an
  error about a file the caller never asked for, and a writer releases
  only a lock it can prove is its own (#2).
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
