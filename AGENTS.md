# AGENTS.md

Guidance for coding agents (and humans) working in this repository.

## What this is

`nomira` is a BuildKit gateway frontend. It compiles a YAML recipe into an
LLB graph: phases run serially, the groups of a phase fan out on the same
base state, are diffed, checked for conflicts, and merged.
Recipes select it with `# syntax=ghcr.io/nomira-os/nomira:1-alpha`.

The project is in alpha: the schema may still change, but every change
must keep README, example and tests in step. The image tag stays
`1-alpha` until the schema is declared stable.

Sibling repositories in the Nomira-OS organization:

- `nomira-cli`: command line (build, check, publish for bootc). It
  knows this frontend only as a container image and must never import
  code from here.
- `arch-base`: the minimal Arch Linux bootc base image.
- `nomira-web`: the reference OS recipe and the website.

## Layout

- `internal/recipe`: schema, parser, merging of `imports`. No BuildKit imports.
- `internal/conflict`: the conflict check. Pure, no BuildKit imports.
- `internal/compile`: recipe to LLB; `fanout.go` the collectors that fan
  out (buckets, fetch, each, then); `lister.sh` lists a group's writes and
  deletions (pass 1), `hash.sh` hashes the paths several groups write
  (pass 2), `guard.sh` decides whether a group without `run` may be built
  apart from the state before it.
- `internal/frontend`: gateway entrypoint, image config, the modes lint
  and resolve (build option `mode`), imports by URL.
- `cmd/frontend`: the binary.
- `schema/recipe.schema.json`: JSON Schema of the recipe, for editors.
- `docs/concept.md`: the original design. Historical; the README is authoritative.

## Commands

```sh
gofmt -l .                                   # must print nothing
go vet ./...
go test ./...
sh hack/test_lister.sh                       # tests lister.sh and hash.sh
sh hack/test_guard.sh                        # tests internal/compile/guard.sh
sh hack/e2e.sh                               # real builds; needs BuildKit, see its header
```

CI runs exactly these through the shared workflows in `Nomira-OS/ci`,
then publishes the frontend image as `sha-<short commit>`. Change CI behaviour
there, not here.

## Rules

- **Schema changes** touch five places together: the parser, its tests,
  the README, `schema/recipe.schema.json` and `examples/`. YAML decoding
  is strict; unknown keys are errors, also in imported files, and name
  the file and line. While in alpha, removed keys are simply unknown.
- **Client options stay out of the recipe.** What only a BuildKit client
  can set (exporter options, names, platform, where secrets come from) is
  never a recipe key.
- **Error messages** speak in recipe terms (phase, group, path), never in
  LLB vertex ids.
- **`lister.sh`, `hash.sh` and `guard.sh`** must run with busybox tools
  only and are covered by `hack/test_lister.sh` and `hack/test_guard.sh`. Extend the test
  when you change a script.
- **Behaviour in BuildKit needs an end-to-end test.** What a diff contains,
  what a merge does to links and modes, what is cached: none of that shows
  in unit tests. Add a case to `hack/e2e.sh` for every change of that kind,
  and check that the case fails without the change.
- **The guard must stay conservative.** A group without `run` is built on an
  empty state only if merging provably gives the same tree as building on
  the state before it. When in doubt the guard reports a finding and the
  group falls back; never the other way round.
- **The frontend knows no package manager or service manager.** Collectors
  are generic: a command and a list of words.
- **Build-time environment** is passed per `run`, never stored in the LLB
  state, so it cannot leak into other groups or the image config.
- **Versions are pinned.** Never consume `latest` or another moving tag for
  images, runners or actions. Renovate keeps pins current.
- **Conventional Commits** for every commit (`feat:`, `fix:`, `docs:` …,
  `!` for breaking changes, which includes any incompatible schema
  change). Version and changelog are derived from them. Releases are cut
  by hand with the `release` workflow (beta or final); never push a tag
  yourself.
- **Licence:** EUPL-1.2, declared through `REUSE.toml`; `reuse lint` must
  pass.
- **Language:** everything is English: README, docs, code, comments and
  commit messages.
- **Be explicit about what was not verified.** Unit tests do not start
  BuildKit. If a change affects a real build and you could not run one,
  say so in the commit or pull request.

## Building without a Go module proxy

`hack/gen_offline_deps.py` writes a `go.offline.mod` that serves all
dependencies from a local `moby/buildkit` checkout. It is only for
sandboxes without network access to the module proxy; `go.offline.mod` is
git-ignored and `go.mod` stays the source of truth.
