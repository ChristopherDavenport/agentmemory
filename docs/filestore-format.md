# The filestore format

Status: specified from the code at v0.0.9 and the fixes after it, and
held to it by `filestore/format_test.go`, which reads the examples in
this document and checks them against the package. A change to the
format starts here, in the examples, and the test says what the code
has to catch up on. Issue #12 asked for this document; it is not an
RFC, and agentsession's `docs/rfcs/0001` is the shape if it becomes one.

The key words MUST, MUST NOT, SHOULD and MAY are to be read as in
RFC 2119. They bind a writer of the directory other than the reference
store, which is `filestore`; where the reference store does something
the words do not require, the text says "the reference" does it.

## Summary

`filestore` is the reference `agentmemory.Store`: one directory per
scope, one Markdown file per entry, which a person can open, edit, diff
and commit, and one append-only `journal.jsonl` at the root that holds
every change the store has seen. The entry files are the live state,
and the journal is the record of how it got there: a numbered chain of
full states per entry, with a hash per state and the hash each state
replaced, so a reader can follow an entry back through time, find a
write that lost another, and recover from a write that was cut off.

Anything that reads or appends to the directory beside the reference
store, a viewer, a sync daemon, an agent in another language, needs
exactly what this document holds: the layout, the entry file, the
journal record, how a record is numbered, chained and encoded, what a
tombstone and a reconciled change look like, what the lock is, how a
torn tail is read and repaired, and which files are derived and may be
ignored or deleted.

## Directory layout

```
memory/
  journal.jsonl              the record: one JSON object per line
  .state.json                derived: the reference store's cursor into the journal
  .lock                      present while a writer holds the store
  .lock.taken-<fingerprint>  present while a writer takes a dead holder's lock over
  user/                      one directory per scope
    INDEX.md                 derived: the scope's live entries, for a person
    style.md                 one file per entry
  project/
    INDEX.md
    build.md
```

- The root MAY be any directory. The reference creates it when it is
  missing.
- A scope is a directory at the root whose name is a *name* (below).
  A directory with any other name is not a scope and is ignored.
- An entry is a file in a scope directory named `<name>.md`, where
  `<name>` is a *name*. A file with any other name is not an entry and
  is ignored; `INDEX.md` cannot collide with one because a name is
  lowercase.
- `journal.jsonl` is the record. It is the only file a writer appends
  to; every other file is written whole.
- `.state.json` and `INDEX.md` are derived from the journal and the
  entry files. They MAY be deleted at any time; the reference rebuilds
  them. A second writer MAY leave them alone under the rules in
  *Writing the store from another program*.
- `.tmp-*` files appear beside a file being written and are gone when
  the write has landed or failed. A reader ignores them.

### Names

A name is kebab-case: lowercase ASCII letters and digits in groups
joined by single hyphens, between 1 and 64 bytes.

```
name  = group *("-" group)
group = 1*(%x61-7A / %x30-39)      ; a-z, 0-9
```

Scopes, entry names and metadata keys all follow it, so a name is a
directory name, a file name and a frontmatter key without escaping.
Two metadata keys are reserved and MUST NOT appear in an entry's
metadata: `name` and `updated`.

### Bounds

| what | bound | where it comes from |
|---|---|---|
| a name | 64 bytes | `agentmemory.MaxNameBytes` |
| an entry's metadata, keys and values together | 1024 bytes | `agentmemory.MaxMetaBytes` |
| an entry's content | the store's bound; 4096 bytes by default | `filestore.WithMaxEntryBytes`, `agentmemory.DefaultMaxEntryBytes` |

The content bound is the product's setting and the format does not
record it. A second writer MUST use the bound the product it shares the
directory with uses. The reference refuses to store content over it,
and leaves a hand-written file over it out of the journal (see
*Reconciled changes*).

Content MUST be valid UTF-8 and MUST NOT be empty: an empty entry is
removed, not emptied. A metadata value MUST be valid UTF-8 on one line,
with no leading or trailing white space; it MAY be empty.

## Entry files

An entry file is a frontmatter block between `---` lines followed by
the content, byte for byte:

```markdown file=memory/user/style.md
---
name: style
updated: 2026-09-20T10:00:03Z
description: How to answer
type: feedback
---
Prefers short answers and tabs. Lives in Bristol.
```

### Writing

The reference writes:

1. the line `---`;
2. `name: <name>`, the entry's name;
3. `updated: <time>`, when the store wrote the file, as RFC 3339 in
   UTC with the fractional seconds the time has and no trailing zeros
   (Go's `time.RFC3339Nano`);
4. one `<key>: <value>` line per metadata key, in byte order of the
   keys; a key whose value is empty is written as `<key>:` with
   nothing after the colon;
5. the line `---`;
6. the content, with nothing added before or after it.

Every line of the block ends in `\n`. The content follows the closing
`---\n` directly, so the content's hash is the hash of the file's tail,
and a content that ends in a newline ends the file in one while a
content that does not, does not.

A second writer MUST write the content byte for byte and SHOULD write
the frontmatter as above, so that a person diffing the directory sees
one convention. It MUST write the file atomically: to a temporary file
in the same directory, synced, and renamed into place, so a reader
sees the old file or the new one and never a partial write.

### Reading

The reference reads an entry file as follows, and a second reader
SHOULD do the same, so that both see one entry in one file:

- A file that does not start with `---\n` has no frontmatter: the whole
  file is the content and there is no metadata. A person may write such
  a file by hand.
- A file that starts with `---\n` has its block end at the first
  `\n---\n` after the opening line, or at an immediately following
  `---\n` for an empty block. When there is no closing line, the whole
  file is content: a person's note that happens to open with a rule.
- In the block, each line is split at its first `:`; a line without
  one is ignored. The key and the value are trimmed of surrounding
  white space.
- `name` is ignored. The file name is the entry's name.
- `updated` is parsed as RFC 3339; a value that does not parse is
  treated as absent. When it is absent, the entry's `Updated` is the
  file's modification time.
- Any other key that is a name is metadata. A key that is not a name is
  ignored.

The content is everything after the closing `---\n`. Its hash is
`sha256:` followed by the lowercase hexadecimal SHA-256 digest of
exactly those bytes, which for the file above is
`sha256:3365f6ed0369b24253840a91206f90bec41fc5b78aa571fdbf1415a707ac9b3d`.
An editor that adds or removes a final newline changes the hash.

Reading does not validate. A file whose content is empty, over the
bound, or whose metadata breaks the rules is still an entry to `Get`
and `List`; it is only never journaled, so a person is left to fix it.

The `updated` line is when the store last wrote the file. A person's
edit does not move it; the reference records the person's time from
the file's modification time when it notices the edit, and the line
changes on the store's next write.

## The index

`INDEX.md` in each scope lists the live entries for a person reading
the directory:

```markdown file=memory/user/INDEX.md
# user

- [style](style.md) — How to answer
```

A heading of the scope's name, a blank line, then one `- [name](name.md)`
line per live entry in byte order of the names, followed by ` — ` (an
em dash) and the description when the entry has one, which is the
metadata value under `description`. A scope with no entries holds the
line `No entries.` in place of the list. The file is derived and the
reference rewrites it, whole and atomically, on every write to the
scope and whenever `Reconcile` finds a change in the scope.

## The journal

`journal.jsonl` at the root is the record of every change to every
scope: one JSON object per line, each a *record* in the shape of
`agentmemory.Change`, appended and never rewritten. One sequence orders
the whole store, whatever the scope.

### Records

| member | type | presence | meaning |
|---|---|---|---|
| `seq` | integer, ≥ 1 | required | the record's position in the journal; strictly increasing along the file |
| `entry` | object | required | the entry after the change |
| `entry.scope` | string, a name | required | the entry's scope |
| `entry.name` | string, a name | required | the entry's name |
| `entry.content` | string | required | the whole content after the change; for a tombstone, the last content the entry held |
| `entry.meta` | object of string to string | omitted when empty | the metadata after the change |
| `entry.hash` | string | required | `sha256:` and the digest of `entry.content` |
| `entry.updated` | RFC 3339 time | required | when the entry was last written; for a reconciled edit or add, the file's modification time |
| `entry.deleted` | `true` | omitted when false | the record is a tombstone |
| `prev` | string | omitted when empty | the hash of the content the write was built on, as the writer claims it |
| `replaced` | string | omitted when empty | the hash of the content the journal held for this entry when the change landed |
| `session` | string | omitted when empty | the session that wrote the change; empty for a person or an unattributed writer |
| `source` | string | omitted when empty | `reconciled` for a state the store found rather than wrote; empty for a write through the store |
| `at` | RFC 3339 time | required | when the change was made, for display and the record; it never orders records |

A record is a *create* when `replaced` is empty: the entry did not
exist, or its last record was a tombstone. Otherwise it is a *replace*.

A writer MUST set `entry.hash` to the digest of `entry.content` as
*Reading* defines it. The reference reader trusts the member rather
than recomputing it; a wrong one makes the next write notice a
difference between the file and the journal and record the file's
state as a reconciled change, so the store corrects itself at the cost
of a spurious record.

### Encoding

A record is one line: a JSON object serialised without raw newline
characters, terminated by `\n`. A writer MUST NOT write a newline
inside a record, and MUST NOT write blank lines.

Readers MUST accept the members in any order, MUST accept any valid
JSON escape in a string, and MUST ignore members they do not know. The
reference writer, Go's `encoding/json`, emits:

- the members in the order of the table above, and `entry.meta`'s keys
  in byte order;
- non-ASCII characters as UTF-8, except that `<`, `>` and `&` are
  written `\u003c`, `\u003e` and `\u0026`, and U+2028 and U+2029 as
  `\u2028` and `\u2029`;
- times as RFC 3339 in UTC, with the fractional seconds the time has
  and no trailing zeros: `2026-09-20T10:00:03Z`,
  `2026-09-20T10:00:03.5Z`, `2026-09-20T10:00:00.123456789Z`.

A reader MUST accept RFC 3339 with any number of fractional digits and
any offset. A writer SHOULD write UTC.

The format has no version member. It is identified by the file name
`journal.jsonl` at a store's root. Members may be added to this table
in a later version, each omitted when empty, so a reader that ignores
what it does not know reads a newer journal; the meaning of a member
listed here will not change.

### Sequence

`seq` is allocated by the writer, under the lock, as one more than the
`seq` of the last record in the journal, or 1 for a journal with none.
The last record is found by reading the file backwards from its end:
a trailing partial line is not a record and a line that is not a
record is skipped (see *Damage and the torn tail*). The reference also
keeps its cursor's last number and takes the greater of the two, which
is the same number unless the journal has been edited behind it.

Along the file `seq` MUST strictly increase. Readers MUST NOT assume
it is contiguous: a journal a person has edited may have gaps. A reader
resuming with `after` takes every record whose `seq` is greater than
`after`; `Journal(ctx, 0)` reads the whole journal.

`seq` promises nothing across stores, and nothing across journal files:
a journal replaced by another starts the history again.

### Chains and forks

Records for one entry chain through `replaced`: a record's `replaced`
is the `entry.hash` of the previous record for the same scope and name,
or is empty when there is none or the previous one is a tombstone. A
writer MUST keep the chain. When the file it is about to replace does
not hold the content and metadata the journal last recorded for the
entry, because a person edited the file, it MUST first append a
reconciled record for the state it found, and then its own record with
`replaced` naming that state. The reference does this inside every
`Put` and `Forget`. When the file is one the store cannot journal (see
*Reconciled changes*), nothing is appended for it and `replaced` names
the journal's last state as before, so the chain never names a hash no
record holds, and a write anchored in such a file's hash records the
journal's last state as `prev`, since a base no record holds would read
as a fork. The journal cannot tell such a file whose content is an older
recorded state from that state, so a write anchored in it is recorded
as built on the journal's last state too. The tombstone for such a file
carries the journal's last live record for the name, or the file's
content when there is none.

`prev` is the writer's claim about the state it built the write on: the
hash the caller anchored the write to, with `IfHash` or `BasedOn`, or,
when the caller claimed nothing, the hash the store held, which is
`replaced`. The two members are what make a lost update visible:

- `prev` equal to `replaced` is an ordinary edit, built on the state it
  replaced.
- `prev` different from `replaced` is a *fork*: a write built on a
  state another writer had already replaced. The write landed, so the
  store is consistent, but what it wrote over is only in the journal.
  `agentmemory.LostUpdates` lists these records.
- A writer that composes a whole entry from a read SHOULD set `prev` to
  the hash it read, since a write that claims nothing is recorded as
  built on whatever it landed on and the loss is invisible.

Record 4 of the worked example is a fork: two sessions read the entry
at record 2's hash, each wrote whole, record 3 landed first, and record
4 has `prev` naming record 2 where its `replaced` names record 3.
`LostUpdates` reads the journal through `Journal` and stops at the
first damaged line it yields, so over a journal with damage it reports
the forks before the damage.

### Tombstones

`Forget` appends a tombstone: the entry's last record with
`entry.deleted` set to `true`, `entry.updated` set to the time of the
change, and `prev` and `replaced` both the hash the journal last held
for the name, which is the hash of the content the file held once the
file has been noted; see *Chains and forks* for a file the store cannot
journal.
The content and metadata are kept in the record, so the journal still
has them. The entry file is removed and the name leaves `Get`, `List`
and the index. The next write to the name is a create, with `replaced`
empty; `LostUpdates` treats the state after a tombstone as empty.

### Reconciled changes

A person's edit to the directory is not a write through the store and
is not journaled as it happens. The store records it when it notices it,
in a record with `source` set to `reconciled` and no `session`, and
`prev` and `replaced` both the hash the journal last held for the name.
The reference notices in two places: `Reconcile`, which compares every
entry file with the journal's last state, and every `Put` or `Forget`,
which compares the file it is about to replace. Three cases:

- A file edited: the record carries the file's content and metadata,
  and `entry.updated` is the file's modification time, the person's
  time rather than the store's.
- A file added: a create, `prev` and `replaced` empty, as above.
- A file removed: a tombstone, whose content and metadata are those of
  the journal's last record for the name, `entry.updated` the time of
  the change.

A file the store could not store, with empty content, content over the
bound, or metadata that breaks the rules, is not journaled: the person
is left to fix it. A file whose content and metadata hash as the
journal's last record for the name has not changed, whatever its
modification time says.

`Reconcile` records the differences it finds in byte order of
`scope/name`, all with one `at`, and rewrites the index of each scope
it touched. A removed file is tombstoned with the last record the
journal still holds whole for the name; when it holds none, because the
only record's line is damaged, nothing is appended, and a cursor saved
before the damage keeps the name and reads the journal for it on each
run until a record for the name appears.

### Damage and the torn tail

A write that is cut off, by a crash or a full disk, leaves a partial
last line with no terminating `\n`. The rules:

- A reader MUST treat a trailing line without a `\n` as not a record:
  the record before it is the journal's last.
- A writer MUST, before appending, terminate a trailing partial line
  with `\n`, so that it stays one damaged line and the new record stays
  whole. The reference reads the file's last byte and prepends `\n` to
  its record when that byte is not one.
- A line that is not a record, because it is not valid JSON, is not an
  object of the shape above, has no positive `seq`, has no `entry` with
  a scope and a name that are names, or has no `entry.hash`, is
  *damaged*. A
  reader MUST NOT stop at a damaged line: the reference `Journal`
  yields an error naming the line and goes on to the records after it,
  and the cursor skips it. A writer allocating `seq` walks back past
  damaged lines to the last record.
- A line that is empty or white space only is skipped silently.

So a journal with a damaged line loses that one change and nothing
else, and a journal whose tail is a damaged line followed by a partial
one, which two cut-off writes in a row leave, takes the next write as
any other. The partial record's `seq`, if it had one, was never
allocated: the next record takes the number after the last whole one.

Before the write:

```jsonl file=torn/journal.jsonl noeol
{"seq":1,"entry":{"scope":"user","name":"editor","content":"Uses neovim.\n","hash":"sha256:bb849475c4187cc3b3d202a44fac78fd031244a2539675703b5c2415b8b82f85","updated":"2026-09-20T10:00:10Z"},"session":"slack-7f3a","at":"2026-09-20T10:00:10Z"}
{"seq":2,"entry":{"scope":"user","name":"sty
```

After session `slack-7f3a` creates `user/notes` at 10:00:11:

```jsonl file=torn-after/journal.jsonl
{"seq":1,"entry":{"scope":"user","name":"editor","content":"Uses neovim.\n","hash":"sha256:bb849475c4187cc3b3d202a44fac78fd031244a2539675703b5c2415b8b82f85","updated":"2026-09-20T10:00:10Z"},"session":"slack-7f3a","at":"2026-09-20T10:00:10Z"}
{"seq":2,"entry":{"scope":"user","name":"sty
{"seq":2,"entry":{"scope":"user","name":"notes","content":"Keep answers short.\n","hash":"sha256:245dd54375ed12684f5b7e4d954fec61ccbe08f3bc662dbb3421c173f9c45208","updated":"2026-09-20T10:00:11Z"},"session":"slack-7f3a","at":"2026-09-20T10:00:11Z"}
```

`Journal` over the second file yields record 1, an error for line 2,
and record 2.

## Durability and the order of a write

A write through the reference store happens in this order, all under
the lock:

1. Validate the entry, then take the lock.
2. Read the entry file as it is.
3. Check the bound against the stored size and any `IfHash`
   precondition. A write that fails here writes nothing.
4. Write the entry file: a `.tmp-*` file in the scope directory,
   written, synced, renamed over `<name>.md`, and the directory synced
   where the platform allows. For `Forget`, remove the file instead.
5. Rewrite the scope's `INDEX.md` the same way.
6. Append the reconciled record when the file read in step 2 was not
   the journal's last state for the entry.
7. Append the record: open the journal for append, terminate a partial
   last line, write the line and its `\n` in one write, sync.
8. Write `.state.json` atomically.
9. Release the lock.

What a crash at each point leaves:

- Before step 4: nothing. The store is as it was.
- After step 4 or 5, before step 7: the file is ahead of the journal.
  The next `Put` or `Forget` on the entry, or the next `Reconcile`,
  records the file's state as a reconciled change: the content is kept
  and the session that wrote it is lost.
- During step 7: a partial line, read and repaired as above.
- After step 7: the cursor is behind the journal and catches up on the
  next write.

Reads take no lock. A rename is atomic, so a reader of an entry file
sees the file before or after a write, never torn, and a reader of the
journal sees whole lines and at most one partial one at the end.

## The lock

A writer MUST hold the store's lock while it does any of the above.
The lock is the file `.lock` at the root, created with `O_CREAT|O_EXCL`
so that exactly one creator succeeds, holding the creator's identity as
one JSON object:

```json lock
{"pid":4242,"host":"laptop","since":"2026-09-20T10:00:07.5Z"}
```

`pid` is the holder's process ID, `host` its host name, and `since`
when it took the lock, as RFC 3339 to the nanosecond, which together
identify one holder: a host and a PID repeat, the time does not. `host`
MAY be absent when the holder could not learn its host name; such a
lock is never stale.
Releasing is removing the file, after reading it to check it still
names the releaser; a lock that names another holder, or is empty
because a new holder has created but not yet written it, is left alone.

A writer that finds the lock held:

- waits, polling from 1 ms and doubling to 50 ms, until the lock is
  released, its context is done, or the store's timeout passes
  (`DefaultLockTimeout`, 2 s), and then fails with `ErrLocked` naming
  the holder. A write holds the lock for one file write, so the wait is
  for a burst of writers, not a session.
- MAY take over a *stale* lock: one whose `host` is this host and
  whose `pid` names no running process (a `pid` of zero or less is never
  stale). A lock from another host is never stale, because the writer
  cannot tell; `BreakLock` is the deliberate way to remove it.

A takeover MUST be exclusive, since two writers that both remove the
lock both hold the store and allocate one `seq` twice. The reference
takes a lock over as follows, and a second writer that takes over MUST
do the same or not take over at all:

1. Hard-link `.lock` to `.lock.taken-<fingerprint>`, where
   `<fingerprint>` is the first 8 bytes, as 16 lowercase hexadecimal
   digits, of the SHA-256 digest of `<host>|<pid>|<since>` with
   `<since>` as nanoseconds since the Unix epoch, for the holder read.
   For the lock above the name is `.lock.taken-48b4f2386aa71ca2`.
   The link fails with "exists" when another writer is taking the same
   lock over, in which case wait; and with "not found" when the lock is
   gone, in which case try to create it.
2. Read the claim. When it names the holder read in step 1, remove
   `.lock`, then the claim. When it names another holder, a faster
   writer has already taken over and this is a second name for its
   lock: remove the claim only and look again.
3. Try to create the lock. Of the writers that found the dead holder,
   exactly one removed it and the create decides which of them holds
   the store.

A writer that dies between the link and the removal leaves the claim
beside the lock. Nothing is lost, but that lock is no longer taken
over: writes fail with `ErrLocked` naming the dead holder until
`BreakLock`, which removes `.lock` and every `.lock.taken-*`.

The reference also holds an in-process mutex ahead of the file, so two
handles in one process do not spin on each other. That is not part of
the format.

## The cursor

`.state.json` is the reference store's cursor into the journal, so a
person's edit is noticed by comparing the files with the journal's last
word without reading the journal again. It is derived and never
authoritative: a missing, unreadable, stale or damaged one is rebuilt
by reading the journal whole, and a second writer MAY ignore it under
the rules below. Its shape, for a reader that wants to use it:

```json file=memory/.state.json
{"seq":6,"off":2462,"head":"sha256:0268bff9269c0e728a65c785a0f7372e9c1de8af332728db10dd6276536130ad","entries":{"user/style":{"hash":"sha256:3365f6ed0369b24253840a91206f90bec41fc5b78aa571fdbf1415a707ac9b3d","meta":"sha256:22d575523a9f97fdf61541518acf18395f12c6e5b8edfdff6b525bfadadd6d75"}}}
```

- `seq` is the last record's `seq` the cursor has seen and `off` the
  byte offset after the last complete line it has read, where the next
  record starts.
- `head` is the hash of the journal's first line, newline included, or
  of its first 65536 bytes when the line is longer. It says which file
  the offsets belong to: a journal restored from a backup or written
  again is another file, and the cursor is rebuilt when it does not
  match.
- `entries` is the last state of every live entry, by `scope/name`: the
  content hash and a hash over the metadata, so a frontmatter edit is
  noticed without keeping every description. The metadata hash is the
  content hash of the string made of `<key>=<value>\n` for each key in
  byte order, and is omitted for an entry with no metadata. A
  tombstoned name is absent.

The file is written whole and atomically, with a final `\n`, after
every write that moved the cursor.

## Writing the store from another program

A program other than the reference store that changes the directory
MUST:

1. Hold the lock as above, for the whole of a write.
2. Allocate `seq` as above: one more than the last record in the
   journal, walking back past a partial or damaged tail.
3. Keep the chain: before replacing or removing an entry whose file
   does not hold the content and metadata the journal last recorded for
   it, append a reconciled record for the state found, unless the file
   is one the store cannot journal, and name the journal's last state
   in `replaced`.
4. Write the entry file atomically, with the content byte for byte, and
   remove it for a tombstone.
5. Append its record with `entry.hash` the digest of `entry.content`,
   `replaced` the hash of the state the write landed on, `prev` the
   hash it built on, and the terminated-tail rule observed.
6. Only ever append to the journal.

It SHOULD rewrite the scope's `INDEX.md`; the reference rewrites it on
its next write to the scope either way. It MAY leave `.state.json`
alone: the reference catches its cursor up from the offset it holds and
sees the appended records. A program that does anything to the journal
other than append, rewriting it, truncating it, replacing it, MUST
delete `.state.json`, since a cursor whose `head` still matches and
whose offset is still inside the file would otherwise resume at a byte
that is no longer where a record starts.

A program that only reads needs no lock. It MUST apply the rules for a
partial tail and for damaged lines, and SHOULD read entry files as
*Reading* describes.

A person editing by hand is not a writer in this sense: they edit the
Markdown and the reference notices. They MUST NOT edit the journal,
which is the record; a journal line they change is damage.

## Worked example

Two sessions share one store, a person edits a file, and an entry is
forgotten. The clock advances one second per change from
2026-09-20T10:00:00Z.

1. Session `slack-7f3a` creates `user/editor`.
2. It creates `user/style` with a description and a type.
3. Session `telegram-c91e` reads `user/style`, adds a fact and writes
   the entry whole, anchored with `BasedOn` in the hash it read.
4. Session `slack-7f3a`, which read the entry before Telegram wrote,
   adds a different fact and writes whole, anchored in the same hash.
   Its write lands on Telegram's: a fork, and Telegram's fact is gone
   from the file.
5. A person opens `user/style.md`, merges the two facts by hand, and
   saves at 10:00:03.5. `Reconcile` records the file.
6. Session `slack-7f3a` forgets `user/editor`.

The journal afterwards:

```jsonl file=memory/journal.jsonl
{"seq":1,"entry":{"scope":"user","name":"editor","content":"Uses neovim.\n","hash":"sha256:bb849475c4187cc3b3d202a44fac78fd031244a2539675703b5c2415b8b82f85","updated":"2026-09-20T10:00:00Z"},"session":"slack-7f3a","at":"2026-09-20T10:00:00Z"}
{"seq":2,"entry":{"scope":"user","name":"style","content":"Prefers short answers.\n","meta":{"description":"How to answer","type":"feedback"},"hash":"sha256:b9222ae357120af1d8c83927948ddc111cedb70023b6b694d2ad45df97b088ed","updated":"2026-09-20T10:00:01Z"},"session":"slack-7f3a","at":"2026-09-20T10:00:01Z"}
{"seq":3,"entry":{"scope":"user","name":"style","content":"Prefers short answers \u0026 tabs.\n","meta":{"description":"How to answer","type":"feedback"},"hash":"sha256:db7ebdd5506e13726b52c567a6b21f52885cfd20aa6831ad3b9ac08eac644ce4","updated":"2026-09-20T10:00:02Z"},"prev":"sha256:b9222ae357120af1d8c83927948ddc111cedb70023b6b694d2ad45df97b088ed","replaced":"sha256:b9222ae357120af1d8c83927948ddc111cedb70023b6b694d2ad45df97b088ed","session":"telegram-c91e","at":"2026-09-20T10:00:02Z"}
{"seq":4,"entry":{"scope":"user","name":"style","content":"Prefers short answers. Lives in Bristol.\n","meta":{"description":"How to answer","type":"feedback"},"hash":"sha256:4dab640d22b67f852e80d4775e6424c1564d97a293ca5fe79def3c632d55bbc4","updated":"2026-09-20T10:00:03Z"},"prev":"sha256:b9222ae357120af1d8c83927948ddc111cedb70023b6b694d2ad45df97b088ed","replaced":"sha256:db7ebdd5506e13726b52c567a6b21f52885cfd20aa6831ad3b9ac08eac644ce4","session":"slack-7f3a","at":"2026-09-20T10:00:03Z"}
{"seq":5,"entry":{"scope":"user","name":"style","content":"Prefers short answers and tabs. Lives in Bristol.\n","meta":{"description":"How to answer","type":"feedback"},"hash":"sha256:3365f6ed0369b24253840a91206f90bec41fc5b78aa571fdbf1415a707ac9b3d","updated":"2026-09-20T10:00:03.5Z"},"prev":"sha256:4dab640d22b67f852e80d4775e6424c1564d97a293ca5fe79def3c632d55bbc4","replaced":"sha256:4dab640d22b67f852e80d4775e6424c1564d97a293ca5fe79def3c632d55bbc4","source":"reconciled","at":"2026-09-20T10:00:04Z"}
{"seq":6,"entry":{"scope":"user","name":"editor","content":"Uses neovim.\n","hash":"sha256:bb849475c4187cc3b3d202a44fac78fd031244a2539675703b5c2415b8b82f85","updated":"2026-09-20T10:00:05Z","deleted":true},"prev":"sha256:bb849475c4187cc3b3d202a44fac78fd031244a2539675703b5c2415b8b82f85","replaced":"sha256:bb849475c4187cc3b3d202a44fac78fd031244a2539675703b5c2415b8b82f85","session":"slack-7f3a","at":"2026-09-20T10:00:05Z"}
```

Reading it:

- Records 1 and 2 are creates: no `prev`, no `replaced`.
- Record 3 shows the encoding of `&` and an anchored edit, `prev` equal
  to `replaced`.
- Record 4 is the fork. Its `prev` is record 2's hash, the state Slack
  read; its `replaced` is record 3's, the state it landed on. `LostUpdates`
  reports it, with record 3's hash as what was lost.
- Record 5 is the person's merge, `source` `reconciled`, no `session`,
  `entry.updated` the file's time at half a second past the three, and
  `prev` and `replaced` both record 4's hash.
- Record 6 is the tombstone for `user/editor`: `deleted`, the last
  content kept, `prev` and `replaced` both its hash.

The directory afterwards holds `journal.jsonl`, `.state.json` as shown
under *The cursor*, and under `user/` the `INDEX.md` shown under *The
index* and the `style.md` shown under *Entry files*; `editor.md` is
gone.

## Conformance

`filestore/format_test.go` reads this document and checks every fenced
block whose info string carries `file=<path>`: the first path segment
names an example tree (`memory`, `torn`, `torn-after`), the rest the
file in it, and a block marked `noeol` has no final newline. The test:

- replays the worked example against a fresh store with a fixed clock
  and compares every file of the `memory` tree byte for byte;
- writes the `memory` tree to a directory, opens it and checks `Get`,
  `List`, `Journal`, `LostUpdates`, that every journal line decodes and
  encodes back to the same bytes with the hash of its content, that the
  chain holds, that the entry file and the index are what the package
  writes, and that the cursor is what it rebuilds;
- writes the `torn` tree, makes the write described and compares the
  journal with `torn-after`;
- decodes the block marked `lock`, encodes it back, and checks the
  claim name this document gives for it;
- checks that the file names and bounds this document states are the
  package's.
