# agentmemory

Memory for Go agents over Open Responses: what the agent keeps between
sessions, in a scoped store with an append-only journal, changed only
through tools and rendered into `agentturn.Config.Instructions` with a
manifest for the session's provenance. The design is in
`docs/plans/memory-layer.md`; read it before writing code. `docs/feedback.md` holds the design studies' findings against that plan; apply them to the plan before building the piece they touch.

## Module

- Module path: `github.com/ChristopherDavenport/agentmemory`.
- Go 1.25 is the floor. The root package name is `agentmemory`.
- The root module depends on
  `github.com/ChristopherDavenport/openresponses`,
  `github.com/ChristopherDavenport/agenttool` and the standard library.
  Nothing else. `make deps` and a test enforce it.
- `filestore` and `storetest` are packages of the root module. Each
  imports the root package and the standard library alone; a test
  enforces it.
- `sqlite` is a nested module on `modernc.org/sqlite`.
- `agentturn` is never imported except as a test dependency of the
  tools' integration test.

## Siblings

- `../open-responses`: the wire package. Copy its conventions.
- `../agenttool`: the tool contract, for the memory tools.
- `../agentturn`: the loop. A product wires the rendered block and the
  tools into its `Config`; the loop never imports this module.
- `../agentsession`: the session format. The rendered block lands in
  config deltas; the manifest lands in an `env` or `custom` entry.
  `storetest` mirrors its `storetest`.
- `../agentskill`: read-only sources people write. Same manifest
  shape; different lifecycle, hence a separate module.

## Conventions

Mirror `../agenttool`: a `Makefile` with `build`, `deps`, `test`,
`vet`, `fmt`, `tidy`, `tidy-check`, `lint`, `vuln`, `check` and
`release` targets, a `SUBMODULES` list (`sqlite`), the same CI shape,
a `CHANGELOG.md` in Keep a Changelog form, annotated `v*` tags. `make
check` must pass before any commit.

Tests are table-driven and offline. Every `Store` passes `storetest`.
`Render` has golden fixtures under `testdata/render/`; regenerate with
`go test . -update` and review the diff.
