# RFC 0001: Agent Memory

Status: draft 0.2
Author: Christopher Davenport
Discussion: to be opened against this repository. The Go module at its
root is the reference binding; `filestore` is the reference store,
`storetest` the conformance suite for stores, and the JSON fixtures
under `testdata/` the conformance corpus for everything else
([section 12](#12-conformance)).

## Summary

Memory is what an agent keeps between sessions about the user, the
project and its own past work. This document states the contract a
memory has, in terms a program in any language can implement:

- an **entry**, one remembered thing, named within a **scope**, with
  Markdown content, one-line metadata, a content hash and a metadata
  hash;
- a **store** that holds entries, changes them only by whole-entry
  writes and tombstones, and keeps a **journal**: an append-only,
  numbered record of every state, chained per entry, from which a
  reader finds a write that lost another;
- the **tools** through which a model reads and changes a store, so
  every change is a function call in the transcript;
- the **rendered block**, the part of memory the model always sees: a
  Markdown section of the instructions, in parts with stable IDs and
  trust labels, bounded in bytes, with a **manifest** of what it held
  and left out;
- the **session records** that join a session to the store: the
  manifest under `agentmemory:render`, folded along the session's path,
  and the journal record of each write under `agentmemory:write`, both
  as `custom` entries of an agentsession session.

The on-disk format of the reference store is Annex A, kept in
[`docs/filestore-format.md`](../filestore-format.md).

Draft 0.2 states the contract as the code holds it after the fixes for
#46, #47, #48 and #49, the security fixes of the same batch, and the
answers to draft 0.1's open questions. [Changes from draft
0.1](#15-changes-from-draft-01) lists what moved;
[Appendix B](#appendix-b-inconsistencies-draft-01-found) says how each
inconsistency draft 0.1 found was resolved.

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
is what that name points at, and stays in this repository
([section 16](#16-decisions)).

## Goals

- **Language-neutral.** Every rule is stated over JSON values, byte
  strings and hashes, and every computed output has a JSON fixture. A
  Go name appears only in [Bindings](#11-bindings).
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
- Compaction of the journal. Draft 0.2 defines no way to compact, and
  says what a reader does with a journal whose start is gone and a
  session record from before it ([section 4.9](#49-a-journal-without-its-start)).

## 1. Terminology

The key words MUST, MUST NOT, SHOULD, SHOULD NOT and MAY are to be
interpreted as in RFC 2119.

- **Name**: a string in the kebab-case grammar of
  [section 2.1](#21-names).
- **Scope**: a name that says whose memory an entry is. Products define
  scopes; `user`, `project` and `session` are the conventional three.
- **Entry**: one remembered thing: a scope, a name unique within it,
  content, metadata, a hash, a metadata hash, and the time it was last
  written.
- **Live entry**: an entry a store currently holds, which reads return.
- **Hash**: `sha256:` followed by the 64 lowercase hexadecimal digits
  of the SHA-256 digest of a byte string. An entry's hash is the hash
  of its content's UTF-8 bytes, and of nothing else.
- **Metadata hash**: the hash of an entry's metadata in the form of
  [section 2.3](#23-metadata), or the empty string for none.
- **White space**: the code points with the Unicode `White_Space`
  property: U+0009 to U+000D, U+0020, U+0085, U+00A0, U+1680, U+2000
  to U+200A, U+2028, U+2029, U+202F, U+205F and U+3000. The list is
  closed: a later Unicode version does not change it here.
- **Lowercased**: each code point replaced by its Unicode *simple*
  lowercase mapping (field 13 of `UnicodeData.txt`), one code point
  for one, with no special casing and no context: U+0130 becomes
  U+0069, and a final sigma becomes U+03C3. A code point without a
  mapping is unchanged. The reference uses Unicode 15.0; a store on
  another version may differ on the code points the versions map
  differently, and nothing but search matching depends on it.
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
  **path**, **`custom` entry** and **trust** are that document's.

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
- A value MUST be valid UTF-8 with no U+000D or U+000A, and no white
  space (section 1) at its start or its end. It MAY be empty.
- Keys and values together MUST NOT exceed 1024 bytes.
- An empty map and no map are the same metadata, and a store returns
  the second.

The key `description` is the one this document gives a meaning: the
one-line description the rendered block, the search result and the
reference store's index show beside the entry.

The content hash covers the content alone. The **metadata hash** covers
the metadata: it is the hash of the string made of one line per key, in
byte order of the keys, each the key, `=`, the value and U+000A:

```
description=How to answer\n
type=feedback\n
```

and the empty string for no metadata. A key is a name and a value holds
no line feed, so the form is unambiguous. A conditional write names
both hashes when it keeps metadata it read (section 3.2).

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
| `meta_hash` | string, a hash | omitted when `meta` is empty | the metadata hash of `meta` |
| `updated` | RFC 3339 time | required | when the store last wrote the entry |
| `deleted` | `true` | omitted when false | the record is a tombstone |

A writer SHOULD write `meta_hash` whenever `meta` is not empty. A record
written before draft 0.2 has none, so a reader that needs it computes it
from `meta`, and a reader MAY check a present one against `meta`. A
reader reads `meta` absent or `null` as no metadata.

## 3. The store

A store offers seven operations. A binding names them as it likes; the
rules are about what each does.

### 3.1 Reads

- **Get** (scope, name) returns the live entry, with its hash, its
  metadata hash and its updated time filled, or fails with *not found*.
- **List** (scope) returns the live entries of one scope in byte order
  of their names. A scope with no entries lists nothing, and so does a
  scope the store has never seen.
- **Search** (scopes, query, limit): [section 3.5](#35-search).
- **Journal** (after): [section 4](#4-the-journal).
- **Entry bound**: the content bound put enforces, a positive integer.

A read never returns a tombstone, and returns the caller's own copy: a
caller that changes what it was given changes nothing in the store.

A store MUST refuse a scope or a name that is not a name with
*invalid*, in get, list, search and forget alike, before it looks
anything up, so that nothing is reached through one, whatever the store
maps names onto, and every store answers it the same. The conformance
suite pins it.

**Reads do not validate.** A store whose entries can change outside it,
as the reference directory's files can under a person's editor, returns
an entry as it finds it: empty, over the entry bound, or with metadata a
put would refuse. Its hash and metadata hash are those of what it
returns. A renderer and a tool MUST NOT assume a read entry meets
section 2; the renderer renders it as it is (section 7), and such an
entry is never journaled until a write replaces it (section 4.6).

### 3.2 Put

**Put** (entry, options) creates the entry or replaces it whole,
content and metadata together, and appends the new state to the
journal. It is the store's only write besides forget, so the journal
is a list of full states.

- A store MUST validate the entry against section 2 and refuse a
  violation with *invalid*, and content over its bound with *too
  large*, before anything is written.
- The caller does not set the hash, the metadata hash or the updated
  time; the store computes both hashes and stamps the time, and ignores
  what the caller put there. A put always makes a live entry.
- A successful put appends exactly one record for the write, and
  returns that record, as the journal holds it. A store that notices,
  under the same write, that the entry was changed outside it MAY
  append a reconciled record first (section 4.6).
- A failed put writes nothing and returns no record.
- A put that stores the same content and metadata the entry already
  holds is still a write and still appends a record.

Three options, each optional, say what the write was built on:

- **If-hash** (h) makes the put conditional on the content: it fails
  with *conflict* unless the live entry's hash is h, where the empty
  string means the entry must not be live. A create with the empty
  string never overwrites.
- **If-meta-hash** (m) makes the put conditional on the metadata: it
  fails with *conflict* unless the live entry's metadata hash is m,
  where a missing entry's is the empty string.
- **Based-on** (h) names the base without making it a condition: the
  put lands on whatever the store holds, and the record's `prev` is h.

A conditional put whose condition fails writes nothing. Its check and
its write are one step with respect to other writers (section 3.7).

The record's `prev` is the base: the if-hash when the put has one, else
the based-on hash when one was given, else the hash of the state the
write landed on, which is empty for a create. Both given, the if-hash
wins. If-meta-hash never changes `prev`.

A writer that composes a whole entry from a read SHOULD name the hash
it read, with if-hash or based-on, and a writer that writes back
metadata it read without changing it SHOULD name its metadata hash with
if-meta-hash, since the content hash does not move when another writer
changes only the metadata. A write that names no base is recorded as
built on whatever it landed on, and a write it lost is invisible
(section 4.7).

The state a write "lands on", which `replaced` names (section 4.1), is
the journal's last state for the entry. In a store whose entries change
outside it, the live entry may be a state the store cannot journal (an
empty or over-bound file a person wrote); then `replaced` and a base
taken from that state are as Annex A says, so that the chain never
names a hash no record holds.

### 3.3 Errors

A store reports failure in one of these kinds; a binding MAY add detail
but MUST let a caller tell the kinds apart:

| kind | when | what it carries |
|---|---|---|
| *not found* | get or forget of an entry that is not live | the scope and name |
| *invalid* | an entry, scope, name or metadata key that breaks section 2 | which rule, in words |
| *too large* | put content over the bound | the scope and name, the attempted size, the bound, and the size of the live entry or "none" |
| *conflict* | a conditional put whose condition fails | the scope and name, which condition, content or metadata, and the hash expected and the hash held, empty for none on either side |
| *unsupported* | a search with an option the store does not support (section 3.5) | the option |

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

**Search** (scopes, query, limit, options) returns live entries of the
given scopes that match the query, at most `limit` of them when `limit`
is positive and all of them otherwise. A scope the store has never seen
contributes nothing; a scope that is not a name is *invalid*.

What matches, and in what order, is the store's. The **reference
match**, which the in-memory and directory stores use, is:

- the query is lowercased (section 1) and split at white space
  (section 1) into words, each distinct word once;
- the **text** of an entry is its name, then each of its metadata values
  in any order, then its content, joined by U+000A, lowercased;
  metadata keys are not searched;
- an entry matches when every word occurs in its text as a substring of
  code points;
- an empty query, or one of white space alone, matches every entry;
- results come in the order the scopes were given, then by name.

The SQLite store matches every word as a token prefix in a full-text
index of the name, the metadata values and the content, so `eovi`
finds `Neovim` there and not in the reference match, and ranks by
relevance, ties broken by scope name and entry name. A caller MUST NOT
assume one store's matching of another.

**Options.** A store MAY support search options beside the query:

| option | value | keeps |
|---|---|---|
| `any` | boolean | an entry whose text holds any word of the query rather than every word, ranked by the store |
| `meta` | object of name to string | an entry whose metadata holds every given key with exactly the given value |
| `updated_after` | RFC 3339 time | an entry whose `updated` is after the time, exclusive |
| `updated_before` | RFC 3339 time | an entry whose `updated` is before the time, exclusive |

Options combine: an entry is kept when it passes every option given and
the query. A store that supports options declares which (section 5.1).
A store MUST refuse a search naming an option it does not support with
*unsupported*, rather than ignore it, and MUST refuse a `meta` key that
is not a name with *invalid*. `limit` applies after the options.

Under `any`, the reference ranks an entry by how many distinct words of
the query its text holds, most first, ties in the order a plain search
gives; the SQLite store uses its index's relevance (bm25). Without
`any`, an option filters and does not reorder.

### 3.6 Attribution

Every record names the **session** that wrote it, a string the writer
supplies with each write, or the empty string for a person or an
unattributed writer. One store serves many sessions at once, so the
session travels with the write, not with the store. A product that
records its sessions in the agentsession format SHOULD give the
agentsession session ID, so a reader joins the journal to sessions
without the product's help; a product that does not MAY give any
string. Nothing authenticates it (section 10).

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
do. A reader MUST NOT treat the error as the journal's end. A store
cannot tell whether a record it cannot read is after the cursor, so it
MAY yield the error to a reader whose cursor is past it; the reference
directory store does.

### 4.3 Sequence

A store MUST give each record a `seq` greater than that of every record
in its journal it can read, and SHOULD give it one more than the
greatest `seq` it knows the journal to hold, or 1 for an empty journal.
Along the journal `seq` MUST strictly increase. A fresh store's journal
is therefore numbered 1, 2, 3 and on, and the conformance suite checks
it.

The reference stores take one more than the greatest they know: the
in-memory store its length; the SQLite store the largest `seq` in its
table, an undecodable row's included; the directory store the greater
of the last record read from the file's tail and the last its cursor
saw (Annex A), which are the same unless the journal was edited behind
it.

A reader MUST NOT assume a journal is contiguous: a journal edited
behind the store, or one whose start was removed (4.9), leaves gaps. A
damaged record carries no number a reader can trust, so the record
written after it MAY take the number the damaged record had; a reader
MUST NOT take a damaged record's number as taken.

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
replaced, or that does not continue the chain a reader holds for its
entry. An auditor lists them to say what a session may have discarded.
A reader reads the journal from a cursor and holds, per entry, the
hash of the last state its window held for it, empty after a
tombstone.

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

A window read from 0 over a journal whose first record is 1 holds every
entry's start, which is no live entry, so an entry's first record there
whose `replaced` is not empty is a break. A window from a later cursor,
or over a journal whose start is gone (4.9), holds no start, and judges
an entry's first record in it as a fork alone.

For each record reported, a reader gives:

- **over**: the last state the window holds for the entry, empty for no
  live entry, or the record's `replaced` when the window holds nothing
  earlier for the entry;
- whether it is a **fork** (`prev` differs from `replaced`) and whether
  it is a **break** (`replaced` differs from over);
- for a break, whether the reader met a record it could not read after
  the entry's earlier record, or after the start of the window, and
  before this one: the break is then **after damage**, and may be that
  damage, since the missing record may be the one the chain runs
  through.

One record may be both a fork and a break. A reader returns the errors
for the records it could not read beside the list.

### 4.8 The last sequence number

A store SHOULD answer the journal's **last sequence number** without
the reader reading the journal: a number after which the journal yields
no record now, 0 for an empty journal, which is the `seq` of the last
record the store can read or a greater number the store holds for a
record it cannot (SQLite's undecodable row). The next record appended
takes a greater one. A reader MAY fall back to reading the journal to
its end, counting only the records it can read. This is an optional
capability (section 5.2); a background pass resuming from a cursor
needs it without reading an unbounded journal.

### 4.9 A journal without its start

Draft 0.2 defines no compaction, and a store MUST NOT remove records
from its journal. A journal may still lose its start: restored from a
partial backup, or truncated by hand, or compacted by a later draft.
Such a journal's first record has a `seq` greater than 1.

- A reader MUST NOT read the absence of records before the first as
  damage, nor take the first record of an entry there for a create.
- A lost-update reader reads such a journal from 0 as it reads from a
  cursor just before its first record (4.7).
- A reader holding an `agentmemory:write` record (section 8.3) whose
  `seq` is before the journal's first record, or whose `seq` the
  journal holds for another entry or another hash, MUST take the
  record's own `change` as the state it names at that `seq`, MUST NOT
  report the record or the journal as damaged for it, and SHOULD say
  that the journal no longer holds it. The write record stands alone
  for exactly this reason.

## 5. Optional store capabilities

A capability is something a store MAY offer beyond section 3. A store
that offers one MUST make its presence discoverable without calling it
(in the Go binding, a second interface the store implements), and a
caller MUST NOT assume a capability the store does not declare. Every
store in this repository offers both.

### 5.1 Search options

The options of section 3.5: `any`, `meta`, `updated_after`,
`updated_before`. A store declares the names of those it supports. It
MUST refuse a search naming an option it does not support with
*unsupported*, and a caller that searches a store declaring no options
with any option set MUST be refused the same way. `memory_search` MUST
offer only the options its store supports (section 6.6). How `any`
ranks is the store's.

### 5.2 Last sequence number

Section 4.8.

## 6. The tools

The model reaches a store through tools in the agenttool contract
(agenttool RFC 0001), so every read beyond the block and every write is
a function call the session records. A product offers them over a list
of **writable scopes**, one or more, and optionally **read scopes**.

### 6.1 Common rules

- The tools are named `memory_save`, `memory_patch`, `memory_forget`,
  `memory_search`, and, when the product offers an insert (6.5),
  `memory_insert`.
- Writable scopes MUST be distinct names, and there MUST be at least
  one. Read scopes MUST be distinct names and none of them writable.
  A tool set built otherwise is a programming error, not a call error.
- Each writing tool's parameter schema has a `scope` property whose
  `enum` lists the scopes it writes. With more than one, `scope` is
  required; with one, it MAY be left out and means that one.
  `memory_search`'s `scopes` items have an `enum` of the writable and
  read scopes together. Section 6.10 gives every schema.
- The tool checks every call against its own schema before it runs,
  so a required property, a type or an enum is enforced, not advisory:
  a `memory_patch` that omits `new_text` is an error, not a deletion,
  and `"meta": null` is an error, not "keep the metadata".
- A writing tool given a read scope fails with the message `scope
  <scope> is read-only`, before the schema check, and its description
  says which scopes are read-only and that message.
- A scope outside the lists fails with an error naming the choices.
- Failures reach the model by agenttool's error convention. The text of
  a success is for the model and is not normative; what a reader needs
  is in the write record (6.7).
- Every content size a tool reports is in bytes, beside the store's
  bound.

Annotations, flow and replay, as agenttool defines them:

| tool | title | read-only | destructive | idempotent | open-world | replay | reads |
|---|---|---|---|---|---|---|---|
| `memory_save` | Save memory entry | no | yes | no | no | unknown | not stated |
| `memory_patch` | Edit memory entry | no | no | no | no | unknown | not stated |
| `memory_forget` | Forget memory entry | no | yes | no | no | unknown | not stated |
| `memory_search` | Search memory | yes | no | (n/a) | no | safe | `untrusted`, unless the product says otherwise |
| `memory_insert` | Insert memory entry | no | no | no | no | unknown | not stated |

No writer is idempotent or safe to run again: a save run again is a
second write and, anchored in the block, reports the session's own
first run as lost; a patch run again fails or edits twice; a forget run
again removes what another session saved in between; an insert run
again inserts again. `memory_search`'s flow declares that its output
reads `untrusted` content, since it returns memory a model wrote
(section 10), and sends nothing; a product whose searchable scopes only
people write MAY narrow it, and SHOULD declare the union of the labels
it gives the scopes in the block (7.2).

### 6.2 `memory_save`

Arguments: `scope`, `name` (required), `content` (required), `meta`
(optional object).

It reads the live entry, then puts the whole entry:

- When the product fixed the scope's names (6.5), a name outside them
  fails before anything is read.
- `meta` given replaces the metadata, and `{}` clears it. `meta` left
  out keeps the metadata the live entry has, since a model rewriting
  the content is not saying to drop the description; the put is then
  conditional on that metadata, with if-meta-hash, and on *conflict*
  the tool reads the metadata again and puts again, up to four attempts
  in all, so a description another session changed in between is kept.
- The put is anchored with based-on (3.2): in the hash the block showed
  for the entry when the product says what the block showed (6.8) and
  the block held the entry; otherwise in the hash of its first read;
  and when the read found no entry, in the empty string, with
  if-meta-hash of the empty string too, so another session's create
  between the read and the put is a fork the journal shows, and one
  with metadata is refused with *conflict* rather than written over. A
  metadata retry, which only a save that keeps metadata makes, keeps
  the base, so a content change in between is still the fork the
  journal shows.
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
- The put is conditional with if-hash and if-meta-hash of the state
  read. On *conflict*, of either, the tool reads again and applies the
  edit to the new content, up to four attempts in all, then fails. Two
  sessions patching one entry keep both edits; a description another
  session changed is kept; an edit whose anchor another session
  removed fails out loud.

### 6.4 `memory_forget`

Arguments: `scope`, `name`. It forgets the entry (3.4). When the
product fixed the scope's names, a fixed name fails (6.5).

### 6.5 Fixed names and insert

> A product MAY fix a scope's entry names. `memory_save` MUST then
> refuse any other name in that scope, and `memory_forget` MUST refuse
> the fixed names, each with an error naming the fixed entries. A
> product MAY offer an insert into a scope. The tool names the entry
> itself, a valid name unique in the scope, built from the time of the
> write and the content's hash, plus a suffix when that name is taken.
> It creates the entry with the must-not-exist precondition, and its
> result names the entry.

Fixed names are for a scope of a few named blocks the model rewrites
and never adds to or removes, as Letta's `persona` and `human` are: a
model told to rewrite one otherwise saves a new name beside it. The
writers' descriptions name the fixed entries. `memory_patch` edits a
fixed entry as any other. A scope with fixed names takes no insert.

`memory_insert` takes `scope` (under the scope rule, over the scopes
the product offers inserts into), `content` (required) and `meta`
(optional). Its n-th attempt names the entry

```
YYYYMMDD-HHMMSS-hhhhhhhh        for n = 1
YYYYMMDD-HHMMSS-hhhhhhhh-n      for n ≥ 2
```

with the date and time the tool begins the write, in UTC, to the
second, and `hhhhhhhh` the first eight hexadecimal digits of the
content's hash, after `sha256:`. Each attempt is a put with if-hash of
the empty string; on *conflict* the tool tries the next n, up to 100.
An insert never overwrites, and two inserts of one content in one
second get `…-1a2b3c4d` and `…-1a2b3c4d-2`. Names sort by time, and the
block renders in name order (7.3), so under the budget a scope of
inserts shows its oldest and leaves out its newest, which
`memory_search` reaches.

### 6.6 `memory_search`

Arguments: `query` (required), `scopes` (optional array), `limit`
(optional integer), and one argument for each search option its store
supports (3.5): `any` (boolean), `meta` (object of string to string),
`updated_after` and `updated_before` (strings, RFC 3339). An option the
store does not support has no argument, and the description says which
it offers.

- A query that is empty or white space alone fails.
- `scopes` left out or empty searches every writable and read scope,
  in that order.
- `limit` left out, or under one, is the product's default, 5 unless
  it sets another.
- A bound that is not an RFC 3339 time fails, naming the argument.
- The result lists each match with its scope, name, size, description
  and whole content, while the content shown stays within the product's
  result bound, 16384 bytes unless it sets another. A match whose
  content would pass the bound is listed by scope, name and size
  instead, and later, smaller matches are still shown. The result is
  in the transcript for the rest of the session, which is why both
  defaults are low.
- A search writes nothing and carries no record.

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
headings of level four or none inside an entry; the block writes levels
one to three at level four anyway (7.2). Its text is not normative.

### 6.10 Parameter schemas

The schemas below are for writable scopes `user` and `project`, read
scope `rules`, inserts into `user`, and a store that supports every
search option. A tool set over other scopes writes its own in the
`enum`s; with one scope, `scope` leaves `required`. The descriptions
are the reference's and not normative; the types, the `enum`s and
`required` are. No schema sets `additionalProperties` at its top level,
so a member it does not list is ignored, except that a search option a store does not
support is refused by the store (5.1).

`memory_save`:

```json
{"type":"object","properties":{
  "scope":{"type":"string","enum":["user","project"],"description":"Which memory the entry belongs to; name one of the values"},
  "name":{"type":"string","description":"Kebab-case name, unique within the scope: lowercase letters, digits and hyphens"},
  "content":{"type":"string","description":"The whole content of the entry, Markdown"},
  "meta":{"type":"object","additionalProperties":{"type":"string"},"description":"Metadata beside the content: a description is shown in the block and the index; keys are kebab-case, values one line. Omit it to keep the metadata the entry has; give {} to clear it"}},
 "required":["scope","name","content"]}
```

`memory_patch`:

```json
{"type":"object","properties":{
  "scope":{"type":"string","enum":["user","project"],"description":"Which memory the entry belongs to; name one of the values"},
  "name":{"type":"string","description":"The entry to edit"},
  "old_text":{"type":"string","description":"Text that appears exactly once in the entry, copied exactly"},
  "new_text":{"type":"string","description":"What replaces it; empty removes it"}},
 "required":["scope","name","old_text","new_text"]}
```

`memory_forget`:

```json
{"type":"object","properties":{
  "scope":{"type":"string","enum":["user","project"],"description":"Which memory the entry belongs to; name one of the values"},
  "name":{"type":"string","description":"The entry to remove"}},
 "required":["scope","name"]}
```

`memory_search`:

```json
{"type":"object","properties":{
  "query":{"type":"string","description":"Words that must all appear in an entry's name, metadata or content, in any case"},
  "scopes":{"type":"array","items":{"type":"string","enum":["user","project","rules"]},"description":"Which memories to search; all of them when omitted"},
  "limit":{"type":"integer","description":"At most this many entries; keep it low, results stay in the conversation"},
  "any":{"type":"boolean","description":"Match entries holding any of the words rather than all of them, best matches first"},
  "meta":{"type":"object","additionalProperties":{"type":"string"},"description":"Only entries whose metadata has each of these keys with exactly this value"},
  "updated_after":{"type":"string","description":"Only entries last written after this time, RFC 3339"},
  "updated_before":{"type":"string","description":"Only entries last written before this time, RFC 3339"}},
 "required":["query"]}
```

`memory_insert`, with one insert scope, so `scope` is optional:

```json
{"type":"object","properties":{
  "scope":{"type":"string","enum":["user"],"description":"Which memory the passage belongs to"},
  "content":{"type":"string","description":"The passage to keep, Markdown"},
  "meta":{"type":"object","additionalProperties":{"type":"string"},"description":"Metadata beside the content: a description is shown in the block and the index; keys are kebab-case, values one line"}},
 "required":["content"]}
```

## 7. The rendered block

### 7.1 Inputs

The block is rendered from a store, an ordered list of scopes, a
**block bound** in bytes, and optionally a **cap** per scope and
**trust labels** per scope.

- The scopes MUST be distinct names; a renderer refuses a scope given
  twice or not a name with *invalid*, since its parts would collide.
- The block bound defaults to 32768. A bound under 1 MUST be refused
  with a **budget** error and no block, not read as the default, so a
  product dividing one budget among instruction layers leaves memory
  out when its share reaches zero.
- A cap is a non-negative byte count; a negative one is a budget
  error. A cap on a scope not rendered does nothing; the last cap given
  for a scope holds. A cap of zero shows none of the scope's entries
  and, since the omission line counts inside the cap, names none of
  them in the block either; the manifest lists every one.
- A scope's trust labels are agentsession's (`untrusted`, `private`,
  or none); a scope without them is `untrusted`. They change no byte.

The fixtures under `testdata/render` give each input as JSON
(`<case>.input.json`: `scopes`, `block_bound`, `entry_bound`, `caps`
and the `entries` put into an empty store) beside its outputs.

### 7.2 Parts

The block is a list of parts, each an `id`, a `text` and a `trust`,
joined by one blank line, the two characters `\n\n`, which is
agentsession's rule for joining instructions parts. In order:

| part | `id` | `text` | `trust` |
|---|---|---|---|
| title | `memory` | `# Memory` | `{"reads":[]}` |
| per scope, its heading | `memory/<scope>` | `## <scope>`; for a scope that lists no entries, `## <scope>`, a blank line and `No entries.` | `{"reads":[]}` |
| per entry shown, in list order | `memory/<scope>/<name>` | the entry part, below | the scope's labels |
| per scope that left entries out, after its entries | `memory/<scope>:omitted` | the omission line, below | the scope's labels |
| summary | `memory:summary` | the summary line, below | `{"reads":[]}` |

An **entry part** is `### <name> (<size> of <bound> bytes)`, then, when
the entry has a non-empty description, ` — ` (space, U+2014, space) and
the description with each U+000D and U+000A in it replaced by U+0020,
then a blank line, then the content with one trailing `\n` removed if
it has one and neutralised, below. `<size>` is the content's byte length
and `<bound>` the store's entry bound. A description from a read that
does not validate (3.1) can hold a line end; on the heading line it
would start a line of its own.

**Neutralised content.** The block uses heading levels one to three
for its own structure, so content MUST NOT open one, and MUST NOT
leave open anything that runs into the parts after it. Split the
content into lines at each `\r\n`, `\n` and lone `\r`, as CommonMark
ends a line; a line's body is the line without its end; its indent is
its run of leading U+0020, and its rest what follows. First, on every
line, inside a code fence or not:

- A line whose indent is at most three and whose rest is one to three
  `#` followed by U+0020, U+0009 or the end of the body, an ATX
  heading, has `#` inserted after its indent until the run is four.
- A line whose indent is at most three and whose rest is one or more
  `=`, or one or more `-`, then only U+0020 and U+0009, a setext
  underline, directly after a line that is neither blank nor an ATX
  heading of any level (one to six `#` then U+0020, U+0009 or the end,
  indent at most three), has `\` inserted after its indent, which makes
  it text. A blank line's body is only U+0020 and U+0009, or empty.

Then, reading the lines in order for code fences and HTML comments:

- A fence opens, outside a fence and a comment, at a line whose indent
  is at most three and whose rest begins with three or more `` ` `` or
  `~`, the backtick form with no `` ` `` after the run; it closes at a
  line whose indent is at most three and whose rest is a run of the
  same character at least as long, then only U+0020 and U+0009.
- A comment opens, outside a fence and a comment, at a line whose
  indent is at most three and whose rest begins with `<!--` and holds
  no `-->` after it; it closes at the next line holding `-->`.
- When a fence or a comment is open after the last line, `\` is
  inserted after the indent of the line that opened it, which makes it
  text, and the lines are read again, until nothing is open.

Nothing else changes: a heading of level four or more, a heading inside
a block quote or a list item, an indented line and the bytes of every
other line stay as written, and line ends are kept. Applying the rule
to its own output changes nothing. `<size>` remains the content's size.
The fixture `testdata/neutralise.json` lists cases of the rule, each an
`in` and its `out`.

The model reads the neutralised text, so an `old_text` copied from the
block may not occur in the stored content (`####` where the entry holds
`##`); `memory_search` returns the stored text, and `memory_patch`'s
description says to copy from it.

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

**Trust.** A part's `trust` is agentsession's member: `reads`, a list
of labels. The renderer's own parts read nothing. An entry part and an
omission line carry their scope's labels, `untrusted` unless the
product gives the scope others; a product gives none only to a scope
people alone write. A renderer MUST set it on every part.

The IDs are distinct within a block, because scopes and names are
names. Every ID is `memory` or begins `memory/` or `memory:`; that
space is reserved to this document, and a product that records parts
of its own beside these gives them IDs outside it.

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
live entries, the entry bound, the scope list, the block bound, the
caps and the trust labels, and of nothing else: no time, no randomness,
no state of an earlier render. An unchanged store renders the same
bytes.

### 7.5 The reference packing

A renderer whose blocks are to be byte-identical to the reference's,
which a session's instructions parts and a prompt cache both reward,
packs as follows; `cost(t)` is `len(t) + 2`, a part with its separator,
and an entry part's text is the neutralised one of 7.2.

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
byte-identical renderer: for each, its input, the joined block, the
parts with their lengths, and the manifest.

### 7.6 The manifest

A render returns a **manifest**: `entries`, the entries the block
holds in block order, and `omitted`, the entries the bound or a cap
left out, in the order they would have appeared. Each element is:

| member | type | presence | meaning |
|---|---|---|---|
| `scope` | string | required | the entry's scope |
| `name` | string | required | the entry's name |
| `hash` | string | required | the hash of the content shown or left out |
| `bytes` | integer | required | the content's size |
| `reason` | string | omitted when empty | why it was left out; empty for an entry the block holds |

A writer MUST write every required member. A reader reads a missing
`hash` as the empty string and a missing `bytes` as 0, so it can still
hash what it was given; the hash then tells it the record is not the
one a writer meant.

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
The manifest names content, not metadata: a change to an entry's
description changes its part and not the manifest (section 16).

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
  whose `reason` is the manifest's reason, and whose `size` is what
  agentsession means by it, the bytes the part would have added: the
  length of the entry's part text (7.2) plus 2 for its separator. It is
  not the manifest's `bytes`, which is the content's size.
- The manifest is a `custom` entry under `agentmemory:render` (8.4).
- A write is a function call, and its write record is a `custom`
  entry under `agentmemory:write` beside the call (8.3).

A part or omitted element that comes from the memory block SHOULD carry
`source` `agentmemory`, the well-known instruction source value for
this layer, which agentsession is adding to its RFC (0.17, in review);
a reader recognises the memory block by that value without knowing the
product. In the reference binding a part carries no source, and the
product sets it.

A part's `trust` (7.2) is a record member agentsession defines on
items, compactions and branch summaries; agentsession 0.16 does not
define it on an instructions part. Where the session format defines it
there, a writer MUST carry each part's labels. Until it does, a reader
that decides anything by trust MUST read every part whose `source` is
`agentmemory` and whose ID names an entry or an omission line as
`untrusted`, and a `memory_search` output carries its flow's labels as
agentsession's `trust` on the output item, as any tool's does.

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
may since have moved. It is not folded; each stands alone, and a reader
whose journal no longer holds it reads it as section 4.9 says.

### 8.4 `agentmemory:render`

`data` is a **render record**, one of two forms told apart by the
member `base`: a record with `base` a string is a delta, and a record
without it, or with `base` `null`, which agentsession reads as an
absent member, is whole.

- A **whole record** is the manifest itself: `{"entries":[…],
  "omitted":[…]}`, `omitted` left out when empty. A writer MUST write
  `entries`, as `[]` when it is empty. A reader MUST read an absent or
  `null` `entries` as empty, since writers before draft 0.2 wrote
  `null`.
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

The fixtures under `testdata/manifest` are records on a path, with the
hash of the manifest in force after each, or the refusal: two agents
taking turns, and every malformed and unresolved form above, a `base`
of `null` and an `entries` of `null` among them.

### 8.6 Redaction

agentsession lets a redactor rewrite a `custom` entry's `data`
(agentsession RFC 0001, Redaction). The records here hold hashes that
cover the text a redaction removes, so a redactor of a session that
holds them:

- that changes any element of an `agentmemory:render` record MUST write
  that record whole, with the elements it wrote, never as a delta; and
  every later delta on the path whose `base` is a manifest the
  redaction changed MUST be re-based: written whole, or as a delta on
  the redacted manifest with `base` and `hash` computed over the
  manifests the redacted session holds. A delta left as it was names a
  base no reader of the redacted session holds, and is refused as
  unresolved;
- that changes an entry's content or metadata in an
  `agentmemory:write` record MUST compute `entry.hash` and
  `entry.meta_hash` over what it wrote, and MAY replace `prev` and
  `replaced`, which name states of the original journal. A reader of a
  redacted session MUST NOT expect a write record to join the journal.

A content hash, a metadata hash and a manifest element's `hash` are
unsalted SHA-256 of the text. A holder of a redacted session who can
guess a short secret, a PIN, a short password, a phone number, confirms
the guess by hashing it. A redaction that removes such a value from an
entry MUST also replace every hash of a text that held it: `entry.hash`
and `entry.meta_hash`, the `prev` and `replaced` that name it, and the
manifest elements that name it, re-basing as above; otherwise it hides
nothing.

## 9. Annex A: the filestore format

The reference store keeps a directory: one directory per scope, one
Markdown file per entry with a frontmatter, an index per scope, an
append-only `journal.jsonl` of records in the shape of section 4.1, a
cursor file, and a lock. A second program that reads or writes that
directory, a viewer, a sync daemon, an agent in another language, MUST
follow [`docs/filestore-format.md`](../filestore-format.md), which is
this document's Annex A and normative. It says, among the rest, that
nothing reading or writing the directory follows a link (*Links*), how
the next `seq` is taken (*Sequence*), and what `replaced` and `prev`
are for a write over a file the store cannot journal (*Chains and
forks*).

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

- **Memory is untrusted input.** The block is part of what the model
  obeys, and its content was written by a model, which may have read
  text an attacker wrote. An injected instruction saved to memory
  persists across sessions and reaches every later session's
  instructions. This document therefore labels every entry part and
  omission line `untrusted` unless the product says a scope is written
  by people alone (7.2), declares `memory_search`'s output `untrusted`
  (6.1), and has a session reader treat memory parts as `untrusted`
  where the session format cannot yet carry the label (8.1). Read
  scopes give a product scopes the model cannot write.
- **Forged structure.** An entry's content is placed inside the block,
  among headings the model reads as the block's structure. Every
  heading of level one to three in content is written at level four and
  every setext underline is escaped, inside code fences too, with a
  lone `\r` read as the line end CommonMark reads it; a description's
  line ends are spaces; and a fence or HTML comment the content leaves
  open has its opener escaped (7.2). So an entry cannot open a scope
  such as `## project` or an entry part with a size it does not have,
  in any context, and cannot turn the parts after it into code or a
  comment. The rule models fences and comments, not CommonMark's other
  HTML blocks (`<div>`, `<pre>` and the rest), so a renderer may still
  show more or less of an entry as code or HTML than this rule reads;
  no heading is left at levels one to three either way. A line of plain
  text such as one that looks like the summary is still possible; the
  summary is always the block's last part, and the labels say the rest
  is untrusted.
- **Links.** The name grammar admits no separator, dot or upper case,
  so a scope and a name are always one path segment under the store's
  root, and every store refuses a non-name before it reads (3.1). That
  is not enough for a directory a person commits: a link inside it,
  `project/x.md` pointing at `~/.ssh/id_ed25519`, would be read,
  rendered into the instructions and copied into the journal. A store
  over a file system MUST NOT follow a link inside its directory, nor
  reach anything outside it through any path, and MUST refuse a scope
  directory that is a link with an error rather than read it as empty;
  Annex A's *Links* says what the reference does with each kind and
  what it does not cover.
- **Lost metadata.** A conditional write on the content alone does not
  see a concurrent change to the metadata, so a write that writes back
  metadata it read undoes another session's description without a
  trace. Writers that keep metadata name its hash (3.2, 6.2, 6.3).
- **Secrets.** Nothing stops a model saving a secret it saw, and the
  journal keeps every state, tombstones included, forever. A product
  that may see secrets redacts before the store or keeps the store
  where the secret could be. The hashes in records and manifests
  confirm a guessed short secret (8.6).
- **Attribution is a claim.** A record's `session` is whatever the
  writer supplied, and `source: reconciled` whatever a store wrote;
  nothing authenticates either, and a journal a person can edit holds
  whatever they wrote. An auditor treats both as the store's account,
  not as proof.
- **Scope isolation.** The tools write only the scopes in their `enum`
  and refuse read scopes before their schema check (6.1); the scope a
  call names is checked at call time as well as in the schema.
- **Bounds.** The entry bound, the metadata bound, the block bound and
  the search result bound keep a model from filling its own context
  through memory.

## 11. Bindings

### Go

The root module is the reference binding.

| Contract | Go |
| --- | --- |
| name rule | `ValidName`, `ValidScope`, `MaxNameBytes`, `CheckScope`, `CheckRef` |
| entry, entry value | `Entry{Scope, Name, Content, Meta, Hash, MetaHash, Updated, Deleted}`, `Scope`, `MetaDescription`, `Entry.Description`, `Entry.Size` |
| entry rules | `CheckEntry`, `MaxMetaBytes`, `DefaultMaxEntryBytes` |
| hash, metadata hash | `Hash`, `MetaHash` |
| store | `Store`: `Get`, `List`, `Put`, `Forget`, `Search`, `Journal`, `MaxEntryBytes` |
| put options | `IfHash(h)`, `IfMetaHash(m)`, `BasedOn(h)`; `PutOption`, `PutOptions`, `ResolvePutOptions`, `PutOptions.BaseFor`, `PutOptions.Check` for a store's own use |
| error kinds | `ErrNotFound`, `ErrInvalid`, `ErrTooLarge` with `SizeError`, `ErrConflict` with `ConflictError{…, Meta}`, `ErrUnsupported` |
| reference match | `Match`, `SearchOptions.Filter` |
| search options | `Searcher` (`SearchOptions`, `SearchWith`), `SearchOptions{Any, Meta, UpdatedAfter, UpdatedBefore}`, `SearchAny`, `SearchMeta`, `SearchUpdatedAfter`, `SearchUpdatedBefore`, `AllSearchOptions`, `SearchSupport`, `SearchWith` |
| attribution | `WithSession(ctx, id)`, `SessionFrom(ctx)` |
| record | `Change{Seq, Entry, Prev, Replaced, Session, Source, At}`, `SourceReconciled` |
| lost updates | `LostUpdates(ctx, store, after)`, `LostUpdate{Change, Over, Break, AfterDamage}` |
| last sequence number | `LastSeqer`, `LastSeq(ctx, s)` |
| tools | `Tools(store, scopes, opts...)`; `SaveTool`, `PatchTool`, `ForgetTool`, `SearchTool`, `InsertTool`, `InsertName` |
| tool options | `WithReadScopes`, `WithRendered` (6.8), `WithFixedNames`, `WithInsert`, `WithSearchLimit`, `WithSearchBytes`, `WithSearchReads`, `DefaultSearchLimit`, `DefaultSearchBytes` |
| write record | `WriteRecord{Tool, Change}`, `WriteNS`, as an `agenttool.Recordable` |
| usage paragraph | `Usage()` |
| render | `Render`, `RenderParts`, `JoinParts`, `Part{ID, Text, Trust}`, `Trust{Reads}`, `EntryPart`, `PartSeparator`, `TitlePartID`, `SummaryPartID`, `PartID` |
| render options | `WithMaxTotalBytes`, `WithScopeMaxBytes`, `WithScopeTrust`, `DefaultMaxTotalBytes`, `ErrBudget` |
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
refuses a non-name in every read, refuses with the error kinds of
section 3.3, writes whole entries atomically, appends one record per
write and returns it, fills both hashes, honours if-hash, if-meta-hash
and based-on, keeps the chain, numbers records by section 4.3, yields
its journal in order and reports a record it cannot read in place,
serialises writers, declares every optional capability it offers and
refuses an option it does not, and, over a file system, follows no
link. `storetest` is the suite; every store in this repository passes
it, and a store elsewhere can run it.

**A conforming tool set** names and schemas its tools as section 6
says, enforces its own schema, refuses read scopes and unlisted scopes,
anchors every save and patch as sections 6.2 and 6.3 say, keeps fixed
names and names inserts as 6.5 says, offers only the search options its
store supports, declares `memory_search`'s flow, and carries the write
record of every write it makes.

**A conforming renderer** meets sections 7.1 to 7.4, neutralises
headings and labels every part as 7.2 says, and returns the manifest of
7.6. A **byte-identical renderer** also reproduces every fixture under
`testdata/render` from its input.

**A conforming lost-update reader** meets section 4.7 and reproduces
every fixture under `testdata/lostupdates`: a journal of lines, where a
line that does not decode as a record with a positive `seq`, an entry
and a hash is a record the reader cannot read, and for each cursor the
records reported, each with `over`, `fork`, `break` and `after_damage`,
and the number of records it could not read.

**A conforming session writer** records the block, its omissions with
their sizes, its manifest and its writes as section 8 says, under the
reserved namespaces only, and redacts them as 8.6 says.

**A conforming session reader** folds `agentmemory:render` by section
8.5 with a window of 8, refuses an unresolved or malformed record
without changing its fold, verifies every delta against its `hash`,
reproduces every fixture under `testdata/manifest`, and keeps
`agentmemory:` namespaces it does not know.

**A conforming writer of the filestore directory** meets Annex A.

## 13. Versioning

This document is versioned with the Go module. The draft number rises
when a rule is added or changed, the annex included. Before the
module's v1.0.0, a draft MAY change any rule; a change that makes a
conforming store, tool set, renderer or reader non-conforming is listed
in the changelog as breaking.

The records in a session and in the journal carry no version member.
Members are added only as optional and omitted when empty, as
`meta_hash` is in draft 0.2, so a reader that ignores what it does not
know reads a later writer's records; the meaning of an existing member
does not change. A change that would change one is a new namespace,
not a new meaning under the old one.

## 14. Prior art

- **Letta memory blocks**: bounded text in the window that the agent
  edits through tools, with archival memory behind a search tool and
  `archival_memory_insert` for unnamed passages. This document takes
  the bound, rendered to the model, the edit by exact replacement, the
  insert and fixed block names.
- **Claude Code's memory directory**: one fact per Markdown file with
  an index loaded each session. This document takes the file as the
  unit, in the reference store, and the index.
- **agentsession RFC 0001**: the session format. This document defines
  no entry type; it uses `config` with `instructions_parts` and
  `instructions_omitted`, and `custom`, and borrows the `keep` cursor,
  the hash notation and the trust labels.
- **agenttool RFC 0001**: the tool contract, its annotations, flow,
  replay and records.
- **Git**: a content-addressed history of whole states, where a merge
  that lost a change is visible only by reading the history. The
  journal is that per entry, with the base recorded so the loss is
  visible without a three-way diff.

## 15. Changes from draft 0.1

| what | sections | the change |
|---|---|---|
| #46 | 4.2, 4.7 | Lost updates are judged by a record's own `prev` against its `replaced`; a reader does not stop at damage, and reports chain breaks, attributing one after damage to the damage. |
| #47 | 3.3, 3.5, 5.1, 6.6, 6.10 | Optional search options `any`, `meta`, `updated_after`, `updated_before`; refused as *unsupported* when a store lacks them; offered by `memory_search` only when supported. |
| #48 | 6.1, 6.2, 6.4, 6.5 | Fixed entry names per scope, refused by save and forget; `memory_insert` naming the entry `YYYYMMDD-HHMMSS-<8 hex>[-n]`. |
| #49 | 4.7, 4.8, 5.2 | A fork is reported from any cursor; chain breaks only where the window holds a predecessor; the last sequence number as a capability. |
| question 1 | 1, 2.3, 2.4, 3.2, 3.3, 6.2, 6.3 | The metadata hash, `meta_hash` in the entry value, if-meta-hash; patch, and save that keeps metadata, are conditional on it. |
| question 2 | 3.1 | Every store refuses a non-name in every read as *invalid*. |
| question 3 | 3.6 | `session` SHOULD be the agentsession session ID. |
| question 4 | 6.5 | The inserted name's form. |
| question 5 | 8.1 | An omitted memory part's `size` is agentsession's: the part's bytes and separator. |
| question 6 | 8.4 | Writers MUST write `"entries":[]`; readers accept `null`. |
| question 7 | 4.9 | Compaction deferred; a journal without its start, and a write record before it. |
| question 8 | Motivation, 16 | This document stays here. |
| security | 3.1, 7.2, 8.1, 10, Annex A | Stores follow no link; parts carry trust labels and content headings are neutralised; `memory_search` reads untrusted. |
| review | 1, 3.1, 3.2, 3.5, 4.3, 6.1, 6.10, 7.6, 8.4, 8.6 | White space and lowercase defined; reads do not validate; the unjournalable file deferred to Annex A; one sequence rule; full schemas, `meta: null`, idempotence; element presence; `base: null`; redaction; fixtures. |

## 16. Decisions

Draft 0.1's open questions, as answered for draft 0.2:

1. **Metadata in the hash.** Not in the content hash; beside it, as the
   optional `meta_hash`, with if-meta-hash, which patch and a
   metadata-keeping save name.
2. **Reads of a non-name.** *Invalid*, in every store; `storetest`
   pins it.
3. **The session string.** SHOULD be the agentsession session ID when
   the product records in that format.
4. **The inserted name's form.** Time first, `YYYYMMDD-HHMMSS-<8
   hex>[-n]`, since name order is render order and the form decides
   which inserts a full block leaves out: the newest.
5. **`size` of an omitted memory part.** agentsession's meaning, the
   bytes the part would add; a profile does not redefine the host's
   members.
6. **`"entries":null`.** Writers MUST write `[]`; readers accept both.
7. **Journal compaction.** Deferred; section 4.9 defines a journal
   without its start and a write record from before it.
8. **The venue.** This document stays in this repository;
   agentsession points to it for `agentmemory`.

Open in draft 0.2:

- **Trust on instructions parts.** agentsession defines `trust` on
  items, not on instructions parts, so a session cannot carry a memory
  part's label (8.1). It needs a change to agentsession.
- **Metadata in the manifest.** The manifest hash covers content
  hashes, not metadata, so a description edit changes the block's part
  but not the manifest, and a save based on the block (6.8) does not
  see a description changed after the render. Adding the metadata hash
  to the manifest element changes the manifest hash, which section 13
  allows only under a new namespace.
- **Compaction** itself: who may compact, and how a compacted journal
  says what it dropped.

## Appendix B: Inconsistencies draft 0.1 found

Draft 0.1 listed eleven places the code and the documents, or two
documents, disagreed at v0.0.10. Each is resolved in draft 0.2, in the
code or the document that was wrong, or left to agentkit as a
follow-up:

1. **What lost updates compare.** Code: `LostUpdates` judges a record's
   `prev` against its `replaced` and reports chain breaks separately
   (#46, #49).
2. **Lost updates and damage.** Code: `LostUpdates` reads past damage
   and returns the errors joined (#46); the annex no longer admits a gap.
3. **What search matches.** Documents: `Store.Search`'s comment names
   which stores use the reference match, and `memory_search`'s
   description and `query` say every metadata value is searched. The
   SQLite store's token-prefix matching and rank order stay, as section
   3.5 allows.
4. **Reads of a non-name.** Code: the in-memory store refuses them as
   the others do, and `storetest` pins it; `Store.List`'s comment says
   so.
5. **The plan's account of the store.** Document: the plan says
   `replaced` names what a write replaced and `prev` its base, that the
   tool applies a patch, that the description is on the heading line,
   and lists the tools.
6. **Part IDs a product adds.** agentkit follow-up: its usage paragraph
   is `memory:usage`, inside the space section 7.2 reserves.
7. **`size` of an omitted part.** Document: section 8.1 requires
   agentsession's meaning, and the binding offers `EntryPart` to compute
   it. agentkit follow-up: it writes the content's size.
8. **Where the manifest goes.** Document: `AGENTS.md` says `custom`.
9. **An empty `entries`.** Code: `Manifest` marshals `entries` as `[]`
   when it has none, so a product that records through
   `Manifest.Record` writes `[]`. agentkit follow-up: confirm once it
   requires this release.
10. **A cap of zero.** Document: `WithScopeMaxBytes`'s comment says the
    scope's entries are not named in the block at a cap of zero.
11. **`Change.Replaced`'s chain.** Document: its comment includes the
    tombstone case.

agentkit follow-ups, for agentkit's own change: rename the usage part
out of the `memory` ID space (6); write an omitted memory part's `size`
as `len(EntryPart(e, bound).Text) + 2` (7); require this release so its
dropped-block manifest records `"entries":[]` (9); carry `Part.Trust`
into the session once agentsession defines trust on a part, and pass
`WithScopeTrust` and `WithSearchReads` for its read scopes; and add
`memory_insert` to the writes it withholds when it drops the block.
