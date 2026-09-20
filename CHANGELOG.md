# Changelog

All user-visible changes to this library. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
uses [Semantic Versioning](https://semver.org/); before v1.0.0 minor
versions may break the API.

## Unreleased

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
