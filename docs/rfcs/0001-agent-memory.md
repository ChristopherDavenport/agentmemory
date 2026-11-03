# RFC 0001: Agent Memory

Status: draft 0.1
Author: Christopher Davenport
Discussion: to be opened against this repository. The Go module at its
root is the reference binding; `filestore` is the reference store,
`storetest` the conformance suite for stores, and `testdata/render` the
golden fixtures for the rendered block.

## Summary

Memory is what an agent keeps between sessions about the user, the
project and its own past work. This document states the contract a
memory has, in terms a program in any language can implement:

- an **entry**, one remembered thing, named within a **scope**, with
  Markdown content, one-line metadata and a content hash;
- a **store** that holds entries, changes them only by whole-entry
  writes and tombstones, and keeps a **journal**: an append-only,
  numbered record of every state, chained per entry, from which a
  reader finds a write that lost another;
- the **tools** through which a model reads and changes a store, so
  every change is a function call in the transcript;
- the **rendered block**, the part of memory the model always sees: a
  Markdown section of the instructions, in parts with stable IDs,
  bounded in bytes, with a **manifest** of what it held and left out;
- the **session records** that join a session to the store: the
  manifest under `agentmemory:render`, folded along the session's path,
  and the journal record of each write under `agentmemory:write`, both
  as `custom` entries of an agentsession session.

The on-disk format of the reference store is Annex A, kept in
[`docs/filestore-format.md`](../filestore-format.md).

Draft 0.1 states the contract as the code at v0.0.10 holds it. Four
vetted issues change it (#46, #47, #48, #49); each place they touch
carries a **Changes** note, and [Changes the open issues
make](#15-changes-the-open-issues-make) lists them, so the fixes implement
this document rather than the other way round. Where the code and its
documents disagree, [Appendix B](#appendix-b-inconsistencies-found)
lists the disagreement; nothing here settles one silently.

## Motivation

The contract lived in Go doc comments, in the README, in a plan, and
in the filestore format document. Code outside Go now reads it: a
viewer of the store directory, a session reader that wants to know
what memory a model was shown, an importer, a port of the tools. Each
had to read Go to learn what a manifest delta is, which hash a record's
`prev` names, or what the summary line says, and the Go comments did
not always agree with the documents (Appendix B).

The session format already has a home for memory: a memory block is a
run of instruction parts, a manifest is a `custom` entry, a write is a
call with a record beside it. What it lacked was a document saying
what those records hold and how a reader folds them. agentsession's
RFC names `agentmemory` as an instruction part's source; this document
is what that name points at.

## Goals

- **Language-neutral.** Every rule is stated over JSON values, byte
  strings and hashes. A Go name appears only in [Bindings](#11-bindings).
- **The journal is the record.** Every change is a numbered, hashed,
  full state, so "what did memory hold at sequence N" and "which write
  lost which" are answerable from the journal alone.
- **What the model saw is recorded.** The rendered block is
  deterministic in the store's state and the bounds, and its manifest
  names every entry it held and every one it left out, with the reason.
- **Cheap to record.** A write moves one entry, and the session records
  pay for that entry, not for the whole memory again.
- **Many stores, one contract.** An in-memory store, a directory of
  Markdown files and a SQLite database pass one suite.

## Non-goals

- Embeddings, vector search or any retrieval beyond the search
  operation and its options. A store may rank as it likes.
- Deciding what is worth remembering. That is the product's
  instructions to the model.
- Conversation history. That is the session; searching past sessions
  is agentsession's store.
- Access control between users. A store is opened by a product for the
  scopes it chooses; who may open it is the product's.
- Authentication, transport or a wire protocol for a remote store.
- Compaction of the journal. The journal is unbounded in this draft
  (see [Open questions](#16-open-questions)).

## 1. Terminology

The key words MUST, MUST NOT, SHOULD, SHOULD NOT and MAY are to be
interpreted as in RFC 2119.

- **Name**: a string in the kebab-case grammar of
  [section 2.1](#21-names).
- **Scope**: a name that says whose memory an entry is. Products define
  scopes; `user`, `project` and `session` are the conventional three.
- **Entry**: one remembered thing: a scope, a name unique within it,
  content, metadata, a hash, and the time it was last written.
- **Live entry**: an entry a store currently holds, which reads return.
- **Hash**: `sha256:` followed by the 64 lowercase hexadecimal digits
  of the SHA-256 digest of a byte string. An entry's hash is the hash
  of its content's UTF-8 bytes, and of nothing else.
- **Store**: the holder of entries and their journal.
- **Record**: one line of the journal: an entry's state after a
  change, with what it was built on and what it replaced.
- **Tombstone**: a record that removes an entry and keeps its last
  content.
- **Base**: the hash of the state a writer built a write on, as the
  writer claims it.
- **Writer**: whatever calls a store's put or forget: a tool, a
  product, a sync program, a person's edit noticed by the store.
- **Product**: the program that opens a store, offers the tools, puts
  the rendered block into its instructions and records the session.
- **Block**: the rendered memory section of the instructions.
- **Part**: one piece of the block with a stable ID.
- **Manifest**: the list of entries a block held and left out.
- **Session**: an agentsession session (agentsession RFC 0001); its
  **path** and **`custom` entry** are that document's.

## 2. Entries

### 2.1 Names

A name is lowercase ASCII letters and digits in groups joined by single
hyphens, between 1 and 64 bytes:

```
name  = group *("-" group)
group = 1*(%x61-7A / %x30-39)      ; a-z, 0-9
```

Scopes, entry names and metadata keys all follow it, so a name is a
directory name, a file name and a frontmatter key without escaping,
and no two of `<scope>/<name>`, `memory/<scope>` and similar
compositions collide.

### 2.2 Content

Content is Markdown, as a string of valid UTF-8. It MUST NOT be empty:
an entry is removed by a tombstone, not emptied. A store has an
**entry bound** in bytes of content, 4096 by default, which it reports
to callers; the product chooses it and nothing records it. Content
over the bound is refused (section 3.3). Content is stored and returned
byte for byte: no normalisation of line endings, white space or a
final newline.

### 2.3 Metadata

Metadata is a map from names to strings, beside the content:

- A key MUST be a name and MUST NOT be `name` or `updated`, which are
  reserved because the reference store writes them in its frontmatter.
- A value MUST be valid UTF-8 on one line, with no carriage return or
  line feed, and no leading or trailing white space. It MAY be empty.
- Keys and values together MUST NOT exceed 1024 bytes.
- An empty map and no map are the same metadata, and a store returns
  the second.

The key `description` is the one this document gives a meaning: the
one-line description the rendered block, the search result and the
reference store's index show beside the entry.

The hash covers the content alone. Two states that differ only in
metadata have one hash (see [Open questions](#16-open-questions)).

### 2.4 The entry value

Where an entry is written as JSON, in a journal record or a session
record, it is an object with these members:

| member | type | presence | meaning |
|---|---|---|---|
| `scope` | string, a name | required | the entry's scope |
| `name` | string, a name | required | the entry's name |
| `content` | string | required | the content; for a tombstone, the last content the entry held |
| `meta` | object of string to string | omitted when empty | the metadata |
| `hash` | string, a hash | required | the hash of `content` |
| `updated` | RFC 3339 time | required | when the store last wrote the entry |
| `deleted` | `true` | omitted when false | the record is a tombstone |

## 3. The store

A store offers seven operations. A binding names them as it likes; the
rules are about what each does.

### 3.1 Reads

- **Get** (scope, name) returns the live entry, with its hash and
  updated time filled, or fails with *not found*.
- **List** (scope) returns the live entries of one scope in byte order
  of their names. A scope with no entries lists nothing, and so does a
  scope the store has never seen.
- **Search** (scopes, query, limit): [section 3.5](#35-search).
- **Journal** (after): [section 4](#4-the-journal).
- **Entry bound**: the content bound put enforces, a positive integer.

A read never returns a tombstone, and returns the caller's own copy: a
caller that changes what it was given changes nothing in the store.

A store MAY refuse a scope or name that is not a name with *invalid*,
and the reference directory and SQLite stores do, so that nothing
outside the store is reached through a path; the in-memory store reads
it as unknown instead (Appendix B, item 4).

### 3.2 Put

**Put** (entry, options) creates the entry or replaces it whole,
content and metadata together, and appends the new state to the
journal. It is the store's only write besides forget, so the journal
is a list of full states.

- A store MUST validate the entry against section 2 and refuse a
  violation with *invalid*, and content over its bound with *too
  large*, before anything is written.
- The caller does not set the hash or the updated time; the store
  computes the hash of the content and stamps the time, and ignores
  what the caller put there. A put always makes a live entry.
- A successful put appends exactly one record for the write, and
  returns that record, as the journal holds it. A store that notices,
  under the same write, that the entry was changed outside it MAY
  append a reconciled record first (section 4.6).
- A failed put writes nothing and returns no record.
- A put that stores the same content and metadata the entry already
  holds is still a write and still appends a record.

Two options, both optional, say what the write was built on:

- **If-hash** (h) makes the put conditional: it fails with *conflict*
  unless the live entry's hash is h, where the empty string means the
  entry must not be live. A conditional put whose condition fails
  writes nothing. A create with the empty string never overwrites.
- **Based-on** (h) names the base without making it a condition: the
  put lands on whatever the store holds, and the record's `prev` is h.

The record's `prev` is the base: the if-hash when the put is
conditional, else the based-on hash when one was given, else the hash
of the state the write landed on, which is empty for a create. Both
options given, the if-hash wins.

A writer that composes a whole entry from a read SHOULD name the hash
it read, with either option. A write that names no base is recorded as
built on whatever it landed on, and a write it lost is invisible
(section 4.7).

### 3.3 Errors

A store reports failure in one of these kinds; a binding MAY add detail
but MUST let a caller tell the kinds apart:

| kind | when | what it carries |
|---|---|---|
| *not found* | get or forget of an entry that is not live | the scope and name |
| *invalid* | an entry, scope or name that breaks section 2 | which rule, in words |
| *too large* | put content over the bound | the scope and name, the attempted size, the bound, and the size of the live entry or "none" |
| *conflict* | a conditional put whose condition fails | the scope and name, the hash expected and the hash held, empty for no entry on either side |

The *too large* error carries the three numbers because its reader is
the model, which trims or splits by arithmetic rather than guessing.
A store MAY fail with any other error (I/O, a lock it could not take);
those are not part of the contract.

### 3.4 Forget

**Forget** (scope, name) removes a live entry and appends a tombstone:
the entry's last state with `deleted` set, `updated` the time of the
removal, and `prev` and `replaced` both the hash it held. It returns
the tombstone. Forgetting an entry that is not live fails with *not
found* and writes nothing. The next put of the name is a create.

### 3.5 Search

**Search** (scopes, query, limit) returns live entries of the given
scopes that match the query, at most `limit` of them when `limit` is
positive and all of them otherwise. A scope the store has never seen
contributes nothing.

What matches, and in what order, is the store's. The **reference
match**, which the in-memory and directory stores use, is:

- the query is split at white space into words, and lowercased;
- an entry matches when every word occurs, as a substring, in the
  lowercased text made of its name, each of its metadata values and its
  content; metadata keys are not searched;
- an empty query, or one of white space alone, matches every entry;
- results come in the order the scopes were given, then by name.

The SQLite store matches every word as a token prefix in a full-text
index of the name, the metadata values and the content, ranks by
relevance and breaks ties by scope name and entry name (Appendix B,
item 3).

**Changes (#47).** A store MAY support search options beside the
query: `any`, an entry matching any word of the query rather than every
word, ranked by the store; `meta`, entries whose metadata holds every
given key with the given value; and `updated_after` and
`updated_before`, bounds on the entry's `updated` time, exclusive. A
store MUST refuse an option it does not support rather than ignore it.
These are an optional capability (section 5.1).

### 3.6 Attribution

Every record names the **session** that wrote it, an opaque string the
writer supplies with each write, or the empty string for a person or
an unattributed writer. One store serves many sessions at once, so the
session travels with the write, not with the store. This document
gives the string no structure (see [Open questions](#16-open-questions)).

### 3.7 Concurrency

A store MUST serialise writes so that each record is appended whole,
each takes its own sequence number, and the chain of section 4.4
holds, whatever number of writers in however many processes share the
store. A conditional put's check and its write MUST be one step with
respect to other writers. Reads MAY run beside writes and see a state
before or after each write, never a part of one.

## 4. The journal

### 4.1 Records

A record is a JSON object:

| member | type | presence | meaning |
|---|---|---|---|
| `seq` | integer ≥ 1 | required | the record's position in the store's journal |
| `entry` | entry value (2.4) | required | the entry after the change |
| `prev` | string, a hash | omitted when empty | the base: the hash of the state the write was built on, as its writer claims |
| `replaced` | string, a hash | omitted when empty | the hash the journal held for the entry when the change landed; empty for a create |
| `session` | string | omitted when empty | the session that wrote it (3.6) |
| `source` | string | omitted when empty | where a change the store did not write came from; `reconciled` is the one value defined (4.6) |
| `at` | RFC 3339 time | required | when the change was made; for display only, it never orders records |

Readers MUST accept the members in any order, MUST accept RFC 3339
times with any number of fractional digits and any offset, and MUST
ignore members they do not know. A writer SHOULD write times in UTC.
Members may be added in a later draft, each omitted when empty; the
meaning of a member listed here will not change.

A record is a **create** when `replaced` is empty: the entry was not
live. Otherwise it is a **replace**, or a tombstone when
`entry.deleted` is set.

### 4.2 Reading

**Journal** (after) yields the records whose `seq` is greater than
`after`, in increasing `seq` order; `after` of 0 reads the whole
journal. A reader that stops early resumes with the last `seq` it saw.
Each record yielded is the caller's own copy.

A store that holds a record it cannot read, a damaged line in a file,
an undecodable row, MUST yield an error in its place, naming where it
is, and SHOULD go on to the records after it, as the reference stores
do. A reader MUST NOT treat the error as the journal's end.

### 4.3 Sequence

A store MUST allocate `seq` as one more than the `seq` of the last
record its journal holds, or 1 for an empty journal. Along the journal
`seq` MUST strictly increase. A fresh store's journal is therefore
numbered 1, 2, 3 and on, and the conformance suite checks it, but a
reader MUST NOT assume a journal is contiguous: a damaged record, or a
journal edited behind the store, leaves gaps.

`seq` orders records within one store. It promises nothing across
stores, and nothing across a journal replaced by another.

### 4.4 The chain

Records for one entry chain through `replaced`: a record's `replaced`
MUST be the `entry.hash` of the previous record for the same scope and
name, or empty when there is none or the previous one is a tombstone.
A store keeps the chain under every write; a change made outside the
store is put on the chain by a reconciled record (4.6) before the
write that replaces it.

### 4.5 The base and forks

`prev` is the writer's claim about the state it built the write on
(section 3.2). Read beside `replaced`:

- `prev` equal to `replaced` is an ordinary edit, built on the state
  it replaced.
- `prev` different from `replaced` is a **fork**: a write built on a
  state another writer had already replaced, or on a state the journal
  never held. The write landed and the store is consistent, but what it
  wrote over survives only in the journal, as the `entry` of the record
  whose hash is this record's `replaced`.

A tombstone has `prev` equal to `replaced`. A create with a declared
base, a put based on an entry that has since been forgotten, is a fork
whose `replaced` is empty.

### 4.6 Reconciled records

A store whose entries can change outside it, as the reference
directory's files can under a person's editor, records what it finds
as a **reconciled** record: `source` set to `reconciled`, `session`
empty, `prev` and `replaced` both the hash the journal last held for
the name, and `entry` the state found. A state the store could not
have stored (empty content, content over the bound, metadata that
breaks section 2) is not recorded; it stays outside the journal until
someone fixes it. Annex A says when the reference store notices.

### 4.7 Lost updates

A **lost update** is a record that was not built on the state it
replaced. An auditor lists them to say what a session may have
discarded.

As the reference reader does it today: it reads the journal from a
cursor and tracks, per entry, the hash of the last state the window
holds, empty after a tombstone. A record whose `prev` differs from the
tracked state is reported, with that state as what it was over. The
first record of an entry in a window that does not start at 0 is not
judged, since nothing was tracked for it. The reader stops at the first
error the journal yields and returns what it has with the error.

**Changes (#46, #49).** Both of the reader's limits contradict section
4.2 and the reason forks are recorded: damage hides every fork after
it, and a fork right after the cursor is never reported. A fork is a
property of the record alone, so the rule becomes:

> A reader listing lost updates MUST NOT stop at a record it cannot
> read. It reports the lost updates on both sides of it and the damage
> beside them. A record whose `prev` differs from its `replaced` is a
> lost update wherever it lies. A record whose `replaced` differs from
> the last state the reader holds for its entry is a break in the
> chain, reported as one, and a break after damage is attributed to the
> damage. A reader resuming lost updates after a cursor MUST report
> every record after it whose `prev` differs from its `replaced`. It
> reports a break in the chain only for an entry whose earlier record
> the window holds.

Under the rule the conformance suite's "nothing from cursor 2" case
inverts: the fork at sequence 3 is reported from cursor 2.

### 4.8 The last sequence number

**Changes (#49).** A store SHOULD answer the journal's last sequence
number without the reader reading the journal; a reader MAY fall back
to reading to the end. This is an optional capability (section 5.2).
The reference stores have it cheaply: the directory store's cursor
file, SQLite's largest `seq`, the in-memory store's length. Until #49
lands, a reader reads to the end.

## 5. Optional store capabilities

A capability is something a store MAY offer beyond section 3. A store
that offers one MUST make its presence discoverable without calling it
(in the Go binding, a second interface the store implements), and a
caller MUST NOT assume a capability the store does not declare. Draft
0.1 has no capability in the code; both below are the vetted changes.

### 5.1 Search options (#47)

The options of section 3.5: `any`, `meta`, `updated_after`,
`updated_before`. A store declares which of them it supports. It MUST
refuse a search naming an option it does not support, rather than
ignore it, and `memory_search` MUST offer only the options its store
supports (section 6.6). How `any` ranks is the store's.

### 5.2 Last sequence number (#49)

Returns the `seq` of the last record the journal holds, 0 for an empty
journal, by the rule of section 4.3: damaged records are not records.

## 6. The tools

The model reaches a store through tools in the agenttool contract
(agenttool RFC 0001), so every read beyond the block and every write is
a function call the session records. A product offers them over a list
of **writable scopes**, one or more, and optionally **read scopes**.

### 6.1 Common rules

- The tools are named `memory_save`, `memory_patch`, `memory_forget`
  and `memory_search`. **Changes (#48)** adds `memory_insert`.
- Writable scopes MUST be distinct names, and there MUST be at least
  one. Read scopes MUST be distinct names and none of them writable.
  A tool set built otherwise is a programming error, not a call error.
- Each writing tool's parameter schema has a `scope` property whose
  `enum` lists the writable scopes. With more than one, `scope` is
  required; with one, it MAY be left out and means that one.
  `memory_search`'s `scopes` items have an `enum` of the writable and
  read scopes together.
- The tool checks every call against its own schema before it runs,
  so a required property or an enum is enforced, not advisory: a
  `memory_patch` that omits `new_text` is an error, not a deletion.
- A writing tool given a read scope fails with the message `scope
  <scope> is read-only`, before the schema check, and its description
  says which scopes are read-only and that message.
- A scope outside the lists fails with an error naming the choices.
- Failures reach the model by agenttool's error convention. The text of
  a success is for the model and is not normative; what a reader needs
  is in the write record (6.7).
- Every content size a tool reports is in bytes, beside the store's
  bound.

Annotations and replay, as agenttool defines them:

| tool | title | read-only | destructive | open-world | replay |
|---|---|---|---|---|---|
| `memory_save` | Save memory entry | no | yes | no | unknown |
| `memory_patch` | Edit memory entry | no | no | no | unknown |
| `memory_forget` | Forget memory entry | no | yes | no | unknown |
| `memory_search` | Search memory | yes | no | no | safe |

No writer is safe to run again: a save run again is a second write and,
anchored in the block, reports the session's own first run as lost; a
patch run again fails or edits twice; a forget run again removes what
another session saved in between.

### 6.2 `memory_save`

Arguments: `scope`, `name` (required), `content` (required), `meta`
(optional object).

It reads the live entry, then puts the whole entry:

- `meta` given replaces the metadata, and `{}` clears it. `meta` left
  out keeps the metadata the live entry has, since a model rewriting
  the content is not saying to drop the description.
- The put is anchored with based-on (3.2): in the hash the block showed
  for the entry when the product says what the block showed (6.8) and
  the block held the entry; otherwise in the hash it read; and for a
  create, in nothing.
- The result says whether it created or replaced, the size it replaced,
  what became of the metadata, and, when the product says what the
  block showed, which base the write took, and that the entry changed
  after the block was rendered when it did.

### 6.3 `memory_patch`

Arguments: `scope`, `name`, `old_text`, `new_text`, all required except
`scope` under its rule.

It replaces one exact occurrence of `old_text` in the live content with
`new_text`, keeping the metadata:

- An empty `old_text` fails. `new_text` MAY be empty, which removes
  `old_text`.
- `old_text` absent from the content fails, saying so; present more
  than once fails, saying how many times.
- A missing entry fails with *not found* and a pointer to
  `memory_save`.
- The put is conditional with if-hash of the state read. On *conflict*
  the tool reads again and applies the edit to the new content, up to
  four attempts in all, then fails. Two sessions patching one entry
  keep both edits; an edit whose anchor another session removed fails
  out loud.

### 6.4 `memory_forget`

Arguments: `scope`, `name`. It forgets the entry (3.4).

### 6.5 Fixed names and insert (#48)

**Changes (#48).** Not in the code at v0.0.10:

> A product MAY fix a scope's entry names. `memory_save` MUST then
> refuse any other name in that scope, and `memory_forget` MUST refuse
> the fixed names, each with an error naming the fixed entries. A
> product MAY offer an insert into a scope. The tool names the entry
> itself, a valid name unique in the scope, built from the time of the
> write and the content's hash, plus a suffix when that name is taken.
> It creates the entry with the must-not-exist precondition, and its
> result names the entry.

`memory_insert` takes `scope`, `content` and `meta`. Its write is a put
with if-hash of the empty string, so an insert never overwrites an
entry, and two inserts of one content in one second get two names. The
exact form of the generated name is an [open
question](#16-open-questions).

### 6.6 `memory_search`

Arguments: `query` (required), `scopes` (optional array), `limit`
(optional integer).

- A query that is empty or white space alone fails.
- `scopes` left out or empty searches every writable and read scope,
  in that order.
- `limit` left out, or under one, is the product's default, 5 unless
  it sets another.
- The result lists each match with its scope, name, size, description
  and whole content, while the content shown stays within the product's
  result bound, 16384 bytes unless it sets another. A match whose
  content would pass the bound is listed by scope, name and size
  instead, and later, smaller matches are still shown. The result is
  in the transcript for the rest of the session, which is why both
  defaults are low.
- A search writes nothing and carries no record.

**Changes (#47).** `memory_search` adds an argument for each search
option its store supports, and none for one it does not.

### 6.7 The write record

Each successful write's tool result carries, out of the model's sight,
a **write record**: a JSON object with `tool`, the tool's name, and
`change`, the journal record (4.1) the store returned. The namespace is
`agentmemory:write` (section 8.3). A tool that wrote nothing carries
none.

### 6.8 What the block showed

A product that re-renders the block before each model call MAY tell the
tools the manifest (7.6) of the block the model is reading now.
`memory_save` then bases a save of an entry the manifest holds on the
hash the manifest names (6.2), since the model composed its content
from the block and not from the tool's read: a write another session
made after the render, which this save discards, becomes a fork the
journal shows. The manifest MUST be the one of the block the model is
reading; one from earlier in the turn makes a second save of an entry
read as built on the block rather than on the model's own first save,
and reported as lost.

### 6.9 Usage paragraph

The reference binding offers one paragraph telling the model what the
block is and which tool makes which change, for the product to append
to its instructions when it offers the tools. It asks the model to use
headings of level four or none inside an entry, since levels one to
three read as the block's own structure. Its text is not normative.

## 7. The rendered block

### 7.1 Inputs

The block is rendered from a store, an ordered list of scopes, a
**block bound** in bytes, and optionally a **cap** per scope.

- The scopes MUST be distinct names; a renderer refuses a scope given
  twice or not a name with *invalid*, since its parts would collide.
- The block bound defaults to 32768. A bound under 1 MUST be refused
  with a **budget** error and no block, not read as the default, so a
  product dividing one budget among instruction layers leaves memory
  out when its share reaches zero.
- A cap is a non-negative byte count; a negative one is a budget
  error. A cap on a scope not rendered does nothing; the last cap given
  for a scope holds.

### 7.2 Parts

The block is a list of parts, each an `id` and a `text`, joined by one
blank line, the two characters `\n\n`, which is agentsession's rule for
joining instructions parts. In order:

| part | `id` | `text` |
|---|---|---|
| title | `memory` | `# Memory` |
| per scope, its heading | `memory/<scope>` | `## <scope>`; for a scope that lists no entries, `## <scope>`, a blank line and `No entries.` |
| per entry shown, in list order | `memory/<scope>/<name>` | the entry part, below |
| per scope that left entries out, after its entries | `memory/<scope>:omitted` | the omission line, below |
| summary | `memory:summary` | the summary line, below |

An **entry part** is `### <name> (<size> of <bound> bytes)`, then, when
the entry has a non-empty description, ` — ` (space, U+2014, space) and
the description, then a blank line, then the content with one trailing
`\n` removed if it has one. `<size>` is the content's byte length and
`<bound>` the store's entry bound. The content is otherwise verbatim.

An **omission line** is `Not shown, over the block budget; fetch with
memory_search: ` followed by `<name> (<size> bytes)` for each entry it
names, joined by `, `, and `, and <n> more` when it names fewer than
the scope left out. When there is room for no name it is `Not shown,
over the block budget: <n> entries.`, and when there is room for
neither, the scope has no omission line.

The **summary** is

```
Entries: <shown> shown, <omitted> omitted. Block: <size> of <bound> bytes (<free> free). Entry limit: <limit> bytes.
```

where `<size>` is the joined block's own length in bytes, summary
included, `<free>` is the block bound less `<size>`, right-aligned with
spaces to the width of the bound's decimal digits, and `<limit>` is the
store's entry bound. Padding `<free>` makes the summary's width depend
on `<size>` alone, so the size it states can be its own.

The IDs are distinct within a block, because scopes and names are
names. Every ID is `memory` or begins `memory/` or `memory:`; a product
that records parts of its own beside these gives them IDs outside that
space (Appendix B, item 6).

### 7.3 Budget and order

- The joined block MUST NOT exceed the block bound. The title, the
  summary at its widest and one heading per scope are written whatever
  the block shows; a bound too small for them is a budget error.
- Entries appear in list order (byte order of names), scope by scope in
  the order given. Order is never changed to fit more.
- An entry whose part does not fit in what is left, of the bound or of
  its scope's cap, is skipped, and the next entry is still considered,
  so one large entry cannot hide the small ones after it.
- Every byte sent counts: headings, descriptions, omission lines,
  separators and the summary. A scope's cap counts its entry parts and
  its omission line with their separators, not its heading.
- Scopes share one bound in order, so a full scope starves the scopes
  after it; a cap on an early scope keeps room for the later ones.
- The summary is last because every write changes it, and the
  instructions are the prefix a provider caches: a write keeps every
  part before the entry it touched in the cached prefix.

### 7.4 Determinism

The block, its parts and its manifest MUST be a function of the store's
live entries, the entry bound, the scope list, the block bound and the
caps, and of nothing else: no time, no randomness, no state of an
earlier render. An unchanged store renders the same bytes.

### 7.5 The reference packing

A renderer whose blocks are to be byte-identical to the reference's,
which a session's instructions parts and a prompt cache both reward,
packs as follows; `cost(t)` is `len(t) + 2`, a part with its separator.

1. `fixed` is `len("# Memory")`, plus `cost` of the summary with
   `<shown>` and `<omitted>` both the total number of entries listed,
   `<size>` and `<free>` both the bound, plus `cost` of each scope
   heading. If `fixed` exceeds the bound, fail with a budget error.
2. For each scope in order, with `spent` the bytes the scopes before it
   placed: `room` is the bound less `fixed` less `spent`, or the
   scope's cap when that is smaller. Place entries in list order while
   `used + cost(part) ≤ room`, skipping those that do not fit. If any
   was skipped, place again from the start with `room` less a reserve
   for the omission line: `cost` of its head, plus the longest
   `<name> (<size> bytes)` of the scope's entries, plus the length of
   `, and <n> more` with `<n>` the number of entries the scope lists.
3. Build the omission line within `room - used`: name skipped entries
   in order while `cost(head) + named so far + next item + the length
   of the ", and <r> more" tail for the rest` fits; add the tail for the
   rest; fall back to the count line if no name fits and it does, and
   to nothing otherwise. Add what the line costs to `used`, and `used`
   to `spent`.
4. The summary: start with `size = fixed + spent`; up to 8 times,
   write the summary with `size` and `bound - size`, and set `size` to
   the length of the block it completes; stop when it no longer moves.

The fixtures under `testdata/render` are the conformance corpus for a
byte-identical renderer: for each, the joined block, the parts with
their lengths, and the manifest.

### 7.6 The manifest

A render returns a **manifest**: `entries`, the entries the block
holds in block order, and `omitted`, the entries the bound or a cap
left out, in the order they would have appeared. Each element is:

| member | type | meaning |
|---|---|---|
| `scope` | string | the entry's scope |
| `name` | string | the entry's name |
| `hash` | string | the hash of the content shown or left out |
| `bytes` | integer | the content's size |
| `reason` | string, omitted when empty | why it was left out; empty for an entry the block holds |

Reasons:

- `budget`: the entry's part did not fit in what was left of the bound
  or of its scope's cap. A renderer writes it.
- `block`: the product left the whole block out, having no room for
  memory on this call. A renderer never writes it; a product records a
  manifest with every entry under `omitted` with this reason and none
  under `entries`, so a reader tells a block dropped whole from a
  block that held nothing because the store held nothing.

A reader MUST accept other reason strings and keep them.

The **manifest hash** is the hash of the string made of one line per
element, `entries` first and then `omitted`, each in order:

```
+ <scope>/<name> <hash> <bytes>\n            for an element of entries
- <scope>/<name> <hash> <bytes> <reason>\n   for an element of omitted
```

with `<bytes>` in decimal. The empty manifest's hash is the hash of the
empty string. Two renders that showed the model the same memories hash
the same, so a product records the manifest only when its hash moves.

## 8. Session records

### 8.1 Where memory sits in a session

A product that records its sessions in the agentsession format
(agentsession RFC 0001) records memory in entries that format already
has, and adds no entry type:

- The block is part of the instructions, so it is in the session's
  `config` entries. A product that records instructions as parts
  records each part of section 7.2 as an instructions part with its
  `id` and `text`, and a write then costs the session the entry part
  it touched and the summary, the rest named by hash or by a run of
  `keep`.
- Each entry the manifest left out is an `instructions_omitted`
  element whose `id` is that entry's part ID, `memory/<scope>/<name>`,
  and whose `reason` is the manifest's reason.
- The manifest is a `custom` entry under `agentmemory:render` (8.2).
- A write is a function call, and its write record is a `custom`
  entry under `agentmemory:write` beside the call (8.3).

A part or omitted element that comes from the memory block SHOULD carry
`source` `agentmemory`, the well-known instruction source value for
this layer, which agentsession is adding to its RFC; a reader
recognises the memory block by that value without knowing the product.
In the reference binding a part carries no source, and the product
sets it (agentkit does).

### 8.2 Namespaces

The namespaces this document defines are `agentmemory:render` and
`agentmemory:write`. Every `custom` namespace beginning `agentmemory:`
is reserved to this document: a writer MUST NOT use another one, and a
reader MUST keep one it does not know, as agentsession requires of any
`custom` entry. The `data` of an entry under either namespace has no
top-level `usage` member, which agentsession reserves for a paid model
call.

### 8.3 `agentmemory:write`

```json
{"type":"custom","ns":"agentmemory:write","call_id":"call_…",
 "data":{"tool":"memory_save","change":{"seq":12,"entry":{…},"prev":"sha256:…","replaced":"sha256:…","session":"…","at":"…"}}}
```

`data` is the write record of section 6.7. `call_id` names the call
whose result carried it; a recorder that writes it beside the call puts
it between the call's dispatch and its output, which is what the
agenttool record rule and agentturn's recorder do. It joins the session
to the store's journal by `seq` and hash without reading a journal that
may since have moved. It is not folded; each stands alone.

### 8.4 `agentmemory:render`

`data` is a **render record**, one of two forms told apart by the
presence of the member `base`:

- A **whole record** is the manifest itself: `{"entries":[…],
  "omitted":[…]}`, `omitted` left out when empty. A reader MUST read an
  absent or `null` `entries` as empty (Appendix B, item 9).
- A **delta** is `{"base":…, "hash":…, "entries":[…], "omitted":[…]}`:
  `base` is the hash of the manifest it is written on, `hash` the hash
  of the manifest it gives, and each list, left out when empty, is a
  list of elements, each either a manifest element written whole or
  `{"keep":n}`, a positive integer and no other member.

A delta's list is read against the same list of its base manifest with
a cursor that starts at the base list's first element:

- `{"keep":n}` stands for the next n elements of the base list from
  the cursor, unchanged, and moves the cursor past them. A keep that
  runs past the end of the base list is malformed.
- A whole element stands for itself. When the base list holds an
  element with its scope and name, at position j, the cursor moves to
  j + 1; otherwise it stays.
- An element that is both a keep and an entry, or neither, or a whole
  element with an empty scope or name, is malformed.
- An element of the base list that no keep takes and no whole element
  names is gone.

The manifest the delta gives MUST hash to its `hash`; one that does not
is malformed. This is the cursor rule agentsession uses for
`instructions_parts`, so one reader serves both.

A writer:

- records a session's first manifest whole;
- after it, records a manifest only when its hash differs from the last
  one it recorded;
- MAY record a delta, writing a keep for each run of elements that
  stand unchanged, all five members alike, at the cursor in the base,
  and every other element whole;
- MUST record whole when either list of either manifest names one
  entry twice, since a keep could not say which it means;
- SHOULD record whole when the delta is not smaller than the whole
  record, as the reference does.

### 8.5 The fold

The **manifest in force** at a point on a session's path is what folding
every `agentmemory:render` entry on the path before it gives, in path
order, starting from the empty manifest. The manifest in force at a
`response` entry is what the model of that call was shown, provided the
product recorded the manifest before the response, as the reference
wiring does from the hook that runs before each model call.

A reader folds with a window of the last **8** distinct manifests in
force, most recent first:

- A whole record becomes the manifest in force.
- A delta is resolved against the manifest in the window whose hash is
  its `base`; before anything is folded, a base equal to the empty
  manifest's hash resolves to the empty manifest. A delta whose base is
  none of these is refused as **unresolved**.
- The result goes to the front of the window, moved there if the
  window already holds a manifest of that hash, and the oldest past 8
  drops off.
- A refused record, unresolved or malformed, leaves the fold as it was.
  A reader SHOULD report it.

The window exists because two agents with their own memories can take
turns in one session: at a hand-back the manifest in force is the
other agent's, which shares nothing with this one's, and a delta on it
would repeat every entry. A writer MAY therefore write a delta on any
of the last 8 distinct manifests in force on the path, and SHOULD pick
the smallest record among the whole and those deltas, preferring the
manifest in force on a tie. A writer MUST NOT write a delta on a
manifest that is not among them.

A reader before v0.0.9 resolved a delta only against the manifest in
force. A writer that writes a delta on another manifest does so only
once its session's readers fold with the window.

## 9. Annex A: the filestore format

The reference store keeps a directory: one directory per scope, one
Markdown file per entry with a frontmatter, an index per scope, an
append-only `journal.jsonl` of records in the shape of section 4.1, a
cursor file, and a lock. A second program that reads or writes that
directory, a viewer, a sync daemon, an agent in another language, MUST
follow [`docs/filestore-format.md`](../filestore-format.md), which is
this document's Annex A and normative.

The annex stays where it is rather than moving under `docs/rfcs/`:
`filestore/format_test.go` reads it by path and checks the package
against its examples, so moving it would touch the test, and the
history of the format is in that file. A change to the annex is a
change to this RFC and raises its draft.

The SQLite store keeps the same record, encoded as the reference
encodes it, in a `line` column of its journal table, so a database can
be dumped to a `journal.jsonl` without conversion. Its schema is not
part of this document.

## 10. Security considerations

- **Memory is instructions.** The block is part of what the model
  obeys, and its content was written by a model, which may have read
  text an attacker wrote. An injected instruction saved to memory
  persists across sessions. Read scopes give a product scopes the model
  cannot write; whose text reaches a writable scope is the product's
  question.
- **Secrets.** Nothing stops a model saving a secret it saw, and the
  journal keeps every state, tombstones included, forever. A product
  that may see secrets redacts before the store or keeps the store
  where the secret could be.
- **Paths.** The name grammar admits no separator, dot or upper case,
  so a scope and a name are always one path segment under the store's
  root, and a store that refuses a non-name before using it as a path
  reaches nothing outside it.
- **Bounds.** The entry bound, the metadata bound, the block bound and
  the search result bound keep a model from filling its own context
  through memory.

## 11. Bindings

### Go

The root module is the reference binding.

| Contract | Go |
| --- | --- |
| name rule | `ValidName`, `ValidScope`, `MaxNameBytes` |
| entry, entry value | `Entry{Scope, Name, Content, Meta, Hash, Updated, Deleted}`, `Scope`, `MetaDescription`, `Entry.Description`, `Entry.Size` |
| entry rules | `CheckEntry`, `MaxMetaBytes`, `DefaultMaxEntryBytes` |
| hash | `Hash` |
| store | `Store`: `Get`, `List`, `Put`, `Forget`, `Search`, `Journal`, `MaxEntryBytes` |
| if-hash, based-on | `IfHash(h)`, `BasedOn(h)`; `PutOption`, `PutOptions`, `ResolvePutOptions`, `PutOptions.BaseFor`, `PutOptions.Check` for a store's own use |
| error kinds | `ErrNotFound`, `ErrInvalid`, `ErrTooLarge` with `SizeError`, `ErrConflict` with `ConflictError` |
| reference match | `Match` |
| attribution | `WithSession(ctx, id)`, `SessionFrom(ctx)` |
| record | `Change{Seq, Entry, Prev, Replaced, Session, Source, At}`, `SourceReconciled` |
| lost updates | `LostUpdates(ctx, store, after)`, `LostUpdate{Change, Over}` |
| search options (#47) | an optional `Searcher` interface with `SearchOptions`, to come |
| last sequence number (#49) | an optional `LastSeqer` interface and `LastSeq(ctx, s)` falling back to a read, to come |
| tools | `Tools(store, scopes, opts...)`; `SaveTool`, `PatchTool`, `ForgetTool`, `SearchTool` |
| tool options | `WithReadScopes`, `WithRendered` (6.8), `WithSearchLimit`, `WithSearchBytes`, `DefaultSearchLimit`, `DefaultSearchBytes`; `WithFixedNames` and `WithInsert` to come (#48) |
| write record | `WriteRecord{Tool, Change}`, `WriteNS`, as an `agenttool.Recordable` |
| usage paragraph | `Usage()` |
| render | `Render`, `RenderParts`, `JoinParts`, `Part{ID, Text}`, `PartSeparator`, `TitlePartID`, `SummaryPartID`, `PartID` |
| render options | `WithMaxTotalBytes`, `WithScopeMaxBytes`, `DefaultMaxTotalBytes`, `ErrBudget` |
| manifest | `Manifest{Entries, Omitted}`, `ManifestEntry{Scope, Name, Hash, Bytes, Reason}`, `OmitBudget`, `OmitBlock`, `Manifest.Hash` |
| render record | `ManifestNS`; `Manifest.Record` (whole), `Manifest.RecordSince` (delta on a base) |
| fold | `ManifestFold` (`Apply`, `Manifest`, `Record`), `ManifestFoldDepth`; `ApplyManifestRecord` is the single-base reader; `ErrManifestBase` is *unresolved* |
| stores | `NewMemStore` (in memory), `filestore.Open` (Annex A, with `Reconcile`, `LockHolder`, `BreakLock`, `ErrLocked`), `sqlite.Open` (nested module) |
| store conformance | `storetest.Run` |

The module never imports the loop or the session format. A product
hands `Manifest.Record`'s namespace and bytes to its session recorder,
and a recorder that reads `agenttool.Recordable` writes the
`WriteRecord` beside the call without knowing its type.

## 12. Conformance

**A conforming store** validates every write against section 2,
refuses with the error kinds of section 3.3, writes whole entries
atomically, appends one record per write and returns it, keeps the
chain, numbers records by section 4.3, yields its journal in order and
reports a record it cannot read in place, serialises writers, and
declares every optional capability it offers and refuses an option it
does not. `storetest` is the suite; every store in this repository
passes it, and a store elsewhere can run it.

**A conforming tool set** names and schemas its tools as section 6
says, enforces its own schema, refuses read scopes and unlisted scopes,
anchors every save and patch as sections 6.2 and 6.3 say, and carries
the write record of every write it makes.

**A conforming renderer** meets sections 7.1 to 7.4 and returns the
manifest of 7.6. A **byte-identical renderer** also reproduces every
fixture under `testdata/render`.

**A conforming session writer** records the block, its omissions, its
manifest and its writes as section 8 says, under the reserved
namespaces only.

**A conforming session reader** folds `agentmemory:render` by section
8.5 with a window of 8, refuses an unresolved or malformed record
without changing its fold, verifies every delta against its `hash`,
and keeps `agentmemory:` namespaces it does not know.

**A conforming writer of the filestore directory** meets Annex A.

## 13. Versioning

This document is versioned with the Go module. The draft number rises
when a rule is added or changed, the annex included. Before the
module's v1.0.0, a draft MAY change any rule; a change that makes a
conforming store, tool set, renderer or reader non-conforming is listed
in the changelog as breaking.

The records in a session and in the journal carry no version member.
Members are added only as optional and omitted when empty, so a reader
that ignores what it does not know reads a later writer's records; the
meaning of an existing member does not change. A change that would
change one is a new namespace, not a new meaning under the old one.

## 14. Prior art

- **Letta memory blocks**: bounded text in the window that the agent
  edits through tools, with archival memory behind a search tool and
  `archival_memory_insert` for unnamed passages. This document takes
  the bound, rendered to the model, the edit by exact replacement, and,
  with #48, the insert and fixed block names.
- **Claude Code's memory directory**: one fact per Markdown file with
  an index loaded each session. This document takes the file as the
  unit, in the reference store, and the index.
- **agentsession RFC 0001**: the session format. This document defines
  no entry type; it uses `config` with `instructions_parts` and
  `instructions_omitted`, and `custom`, and borrows the `keep` cursor
  and the hash notation.
- **agenttool RFC 0001**: the tool contract, its annotations, replay
  and records.
- **Git**: a content-addressed history of whole states, where a merge
  that lost a change is visible only by reading the history. The
  journal is that per entry, with the base recorded so the loss is
  visible without a three-way diff.

## 15. Changes the open issues make

| issue | sections | the change | today |
|---|---|---|---|
| #46 | 4.2, 4.7 | Lost updates are judged by a record's own `prev` against its `replaced`; a reader does not stop at damage, and reports chain breaks, attributing one after damage to the damage. | The reader compares `prev` with the state it tracked and stops at the first error. |
| #47 | 3.5, 5.1, 6.6 | Optional search options `any`, `meta`, `updated_after`, `updated_before`; refused when unsupported; offered by `memory_search` only when supported. | Query, scopes and limit only. |
| #48 | 6.1, 6.5 | Fixed entry names per scope, refused by save and forget; `memory_insert` naming the entry itself. | Any valid name; no insert. |
| #49 | 4.7, 4.8, 5.2 | A fork is reported from any cursor; chain breaks only where the window holds a predecessor; an optional last-sequence-number capability. | The first record of an entry after a cursor is not judged; a reader reads to the end. |

The fix for #46 also corrects the `Store.Journal` doc comment and the
annex's paragraph on lost updates, which describe today's reader.

## 16. Open questions

- **Metadata in the hash.** The hash covers content alone, so a
  conditional put does not see a concurrent metadata change, and a
  write that drops another session's new description is no fork. A
  second hash over the metadata, as the reference store's cursor
  already keeps, would make it visible; it would also be a new member
  in every record.
- **Reads of a non-name.** The in-memory store reads an invalid scope
  or name as unknown, the other two refuse it as *invalid* (Appendix B,
  item 4). Whether to require one behaviour, and which, is open; the
  conformance suite pins neither.
- **The session string.** A record's `session` is opaque. Requiring it
  to be the agentsession session ID would let a reader join a journal
  to sessions without a product's help; products that do not record
  sessions in that format would then have nothing to put there.
- **The inserted name's form (#48).** The vetting fixes what goes into
  the name, the time and the content's hash, and that a taken name gets
  a suffix, but not the form. Fixing it would let two implementations
  agree; leaving it free costs nothing a reader depends on.
- **`size` of an omitted memory part.** agentsession defines an
  `instructions_omitted` element's `size` as the bytes the part would
  have added; the one writer of memory omissions, agentkit, writes the
  entry's content size (Appendix B, item 7). Which this document
  requires is open.
- **The manifest of a block dropped whole.** A product records it with
  every entry under `omitted` as `block`, and agentkit writes
  `"entries":null` for it (Appendix B, item 9). Whether a writer MUST
  write `[]` is open; readers accept both here.
- **Journal compaction.** The journal is unbounded. With `seq` and the
  chain it is tractable to drop everything before a sequence number
  that has a full state at it, but who may do it, and how a reader of
  an `agentmemory:write` record whose `seq` was compacted away learns
  so, is open.
- **The venue.** Whether this document stays here or joins
  agentsession's RFCs as a companion profile.

## Appendix B: Inconsistencies found

Places where the code and the documents, or two documents, disagree at
v0.0.10. Each is a finding; this draft states the code's behaviour
where it states one, and the fix is a separate change.

1. **What lost updates compare.** The README ("A record whose `Prev` is
   not its `Replaced` … `LostUpdates` lists them"), `Change.Replaced`'s
   doc comment, the plan and the annex all say a lost update is a
   record whose `prev` differs from its `replaced`. `LostUpdates`
   compares `prev` with the state it tracked in its window, which is
   `replaced` only when the window holds the chain. (#46, #49)
2. **Lost updates and damage.** The annex says a reader MUST NOT stop
   at a damaged line and a damaged line loses that one change; the
   reference reader stops at the first error. The annex admits it
   ("reports the forks before the damage"). (#46)
3. **What search matches.** `Store.Search`'s doc says "the reference
   stores use `Match` and list by scope then name"; SQLite, a
   reference store, matches token prefixes rather than substrings, so
   `eovi` finds `Neovim` in one and not the other, and orders by rank,
   then by scope name rather than the order the scopes were given.
   `memory_search`'s description, and its `query` argument's, say a
   word must appear in the entry's "name, description or content"; all
   three stores search every metadata value, not only the description.
4. **Reads of a non-name.** `Store.List`'s doc says "An unknown scope
   lists nothing". The directory and SQLite stores refuse an invalid
   scope in `List` and an invalid scope or name in `Get` and `Forget`
   with `ErrInvalid`; the in-memory store lists nothing and answers
   `ErrNotFound`. `storetest` checks neither.
5. **The plan's account of the store.** `docs/plans/memory-layer.md`
   says the journal keeps both writes "with `Prev` naming what each
   replaced"; `replaced` names that, and `prev` the base. It twice says
   "the store applies the patch"; `memory_patch` applies it and the
   store only puts. It puts an entry's description "under" its heading;
   the block puts it on the heading line. Its milestones list "the
   three tools"; there are four.
6. **Part IDs a product adds.** `TitlePartID`'s doc says every ID of
   the block starts `memory`, so a product's own parts take another
   prefix. agentkit names the usage paragraph `memory:usage`, inside
   the block's ID space; no block part has that ID today, but the
   reservation does not hold.
7. **`size` of an omitted part.** agentsession RFC 0001 defines an
   `instructions_omitted` element's `size` as the bytes the part would
   have added; agentkit writes the manifest's `bytes`, the content's
   size, which is less than the part (heading and separator). The
   README says only that the manifest's omissions "are its omitted
   parts".
8. **Where the manifest goes.** `AGENTS.md` says "the manifest lands
   in an `env` or `custom` entry"; the plan explains why `env` cannot
   carry it, and the code and README use `custom` alone.
9. **An empty `entries`.** `Manifest`'s doc and `OmitBlock`'s describe
   a manifest with "none under Entries"; `Render` writes
   `"entries":[]`, while a product that builds the dropped-block
   manifest from a nil list, as agentkit does, records
   `"entries":null`. The Go reader accepts both; a reader in another
   language that expects an array does not.
10. **A cap of zero.** `WithScopeMaxBytes`'s doc says a cap of zero
    "shows none of the scope's entries and names them, room
    permitting". The omission line is counted inside the cap, so at a
    cap of zero there is never room and the scope's entries are not
    named in the block; the manifest still lists them.
11. **`Change.Replaced`'s chain.** Its doc comment says each record's
    `Replaced` names the hash of the record before it for that entry,
    and leaves out the tombstone case, after which it is empty; the
    annex and `LostUpdates` have it.
