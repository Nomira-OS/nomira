# AGENTS.md

Guidance for coding agents (and humans) working in this repository.

## What this is

`nomira` is a BuildKit gateway frontend. It compiles a YAML recipe into an
LLB graph: phases run serially, the groups of a phase fan out on the same
base state, are diffed, checked for write-set disjointness, and merged.
Recipes select it with `# syntax=ghcr.io/nomira-os/nomira:1`.

Sibling repositories in the Nomira-OS organization:

- `nomira-cli`: command line wrapper (build, validate, `bootc switch`). It
  knows this frontend only as a container image and must never import
  code from here.
- `nomira-image`: the minimal Arch Linux bootc base image.
- `nomira-web`: the reference OS recipe and the website.

## Layout

- `internal/recipe`: schema, parser, `include` resolution. No BuildKit imports.
- `internal/conflict`: disjointness check. Pure, no BuildKit imports.
- `internal/compile`: recipe to LLB; `lister.sh` collects a group's write set.
- `internal/frontend`: gateway entrypoint, image config.
- `cmd/frontend`: the binary; also the `lint`, `graph` and `inspect` modes
  that the CLI calls through the image.
- `docs/concept.md`: the original design. Historical; the README is authoritative.

## Commands

```sh
gofmt -l .                                   # must print nothing
go vet ./...
go test ./...
sh hack/test_lister.sh                       # tests internal/compile/lister.sh
go run ./cmd/frontend lint examples/recipe.yml
```

CI runs exactly these, then builds and pushes the frontend image.

## Rules

- **Schema changes** touch four places together: the parser, its tests,
  the README and `examples/recipe.yml`. YAML decoding is strict; unknown
  keys are errors, also in included files.
- **Error messages** speak in recipe terms (phase, group, path), never in
  LLB vertex ids.
- **`lister.sh`** must run with busybox tools only and is covered by
  `hack/test_lister.sh`. Extend the test when you change the script.
- **Build-time environment** is passed per `run`, never stored in the LLB
  state, so it cannot leak into other groups or the image config.
- **Versions are pinned.** Never use `latest` or another moving tag for
  images, runners or actions. GitHub Actions are pinned to a commit SHA
  with the version as a trailing comment. Renovate keeps pins current.
- **Languages:** README and user-facing docs are German. Code, comments,
  commit messages and this file are English.
- **Be explicit about what was not verified.** Unit tests do not start
  BuildKit. If a change affects a real build and you could not run one,
  say so in the commit or pull request.

## Building without a Go module proxy

`hack/gen_offline_deps.py` writes a `go.offline.mod` that serves all
dependencies from a local `moby/buildkit` checkout. It is only for
sandboxes without network access to the module proxy; `go.offline.mod` is
git-ignored and `go.mod` stays the source of truth.
