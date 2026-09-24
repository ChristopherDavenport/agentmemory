# Contributing

Issues and pull requests are welcome.

## Before you start

This library is the memory contract, its reference stores and the
tools through which a model reaches them. It never decides what is
worth remembering; that is instructions to the model, and a product
writes them. It never imports the agent loop; a product wires the
rendered block and the tools into its `agentturn.Config`. `Put` is the
store's only write and the journal's unit; an edit is the
`memory_patch` tool's, applied by the tool and stored whole. Read
`docs/plans/memory-layer.md` before a change of any size, and
`docs/feedback.md` for the findings the design studies raised against
it.

For anything larger than a bug fix, open an issue first so the shape of
the change can be discussed before you spend time on it.

## Development

Go 1.25 or later is required. The full local check is:

```sh
make check        # gofmt, tidy, vet, deps, replaces, staticcheck, govulncheck, race tests, every module
```

The root module depends on `openresponses`, `agenttool` and the
standard library only, and `filestore` and `storetest` on the root
package and the standard library only; `make deps` and a test fail if
anything else creeps in. `agentturn` is a test dependency of the
tools' integration test and never a build dependency. `sqlite` is a
nested module listed under `SUBMODULES` in the Makefile; a bare `go
test ./...` at the root does not cover it, the Makefile targets do.

Every `Store` passes `storetest.Run`; a new store adds a test that
runs it. `Render` has golden fixtures under `testdata/render/`;
regenerate them with `go test . -update` and review the diff.

## Pull requests

- Keep the change focused; unrelated cleanups belong in their own PR.
- Add or update tests. Tests are table-driven and run offline.
- Run `make check` before pushing. CI runs the same steps on the minimum
  and current Go versions.
- Note user-visible changes under *Unreleased* in `CHANGELOG.md`.

## Releases

Every module in the repository shares one version and is tagged at one
commit. With the changelog's *Unreleased* section written:

```sh
make release VERSION=v0.1.0
```

sets the root requirement in `sqlite` to the version, dates the
changelog, runs `make check`, commits, tags `v0.1.0` and
`sqlite/v0.1.0` with the changelog section as the message, and pushes.
The nested `go.mod` requires a released root next to a `replace`
directive to the tree, so consumers fetch the version and the checkout
builds against the working tree. The release workflow publishes a
GitHub release per tag, and the Go module proxy picks the versions up.
Before v1.0.0 the API may change between minor versions; the changelog
records every break.


`make release-guard TAG=<tag>` is what stands between a mistake and a
permanent one, and `make release` runs it for every tag it writes. It
refuses a dirty tree, a tag that already exists locally or on origin, a
version that sorts below the current root release or does not move its
module forward, a first-party require that does not name that version, a
root tag that is not this commit, and a module that will not build with
`GOWORK=off`. The root is guarded and tagged first, because a nested
module's guard needs the root tag to exist. Nothing is public until the
push, so a refusal costs a `git reset --hard HEAD~1` and a `git tag -d`.

`make replaces`, part of `check`, refuses a first-party require that
lacks a matching `replace`. There is no `go.work` here, so the replaces
are the only thing building the tree against itself — and losing one
would make the next release resolve that module from the proxy, where
the version being released does not exist yet.

`go mod tidy` can move a requirement that the release just set, so the
requires are read back and asserted before anything is tagged.
