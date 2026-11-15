# nomira

> **Alpha.** The recipe schema may still change without notice. Recipes
> therefore refer to `ghcr.io/nomira-os/nomira:1-alpha`; once the schema
> is stable this becomes `:1`.

A BuildKit frontend that builds container images from a YAML recipe: in
phases whose groups are **verified to be independent** and are therefore
built in parallel and cached one by one.

The frontend stands on its own. It builds any OCI image, from any base
image, with any BuildKit from v0.10 on. It should therefore work through
`docker buildx` as well; so far only `buildctl` has been tried. It was
created for operating system images with bootc (see
[Why it was created](#why-it-was-created)), but it assumes none of that.

## The idea

In a Dockerfile every step hangs on the previous one: change one, and all
that follow run again. Independent steps can only be built in parallel
through multi-stage detours, and nothing checks whether they really are
independent.

- **Phases** run one after another, in the declared order.
- **Groups** within a phase are declared independent: they fan out from
  the same base state, are built in parallel, cached one by one (DiffOp)
  and merged at the end of the phase (MergeOp).
- The independence is **verified during the build** (the write sets must
  be disjoint, with prefix semantics for whiteouts). A conflict aborts
  the build before anything is merged or exported.

```
S ──Exec(g₁)──→ R₁ ──Diff──→ Δ₁  ┐
S ──Exec(g₂)──→ R₂ ──Diff──→ Δ₂  ├─ parallel, cached one by one, validated
S ──Exec(gₙ)──→ Rₙ ──Diff──→ Δₙ  ┘
S' = Merge(S, Δ₁ … Δₙ)
```

When a group changes, only that group is rebuilt; its neighbours come
from the cache. Motivation and model in detail: `docs/concept.md`
(concept v1; for the recipe schema this README is authoritative).

## Short example

```yaml
# syntax=ghcr.io/nomira-os/nomira:1-alpha
settings:
  name: test
  base: docker.io/library/alpine:3.22
phases:
  - name: content
    groups:
      a: ["run echo a > /etc/a"]
      b: ["run echo b > /etc/b"]
```

If `a` and `b` write the same file, the build aborts:

```text
phase "content": declared-independent groups are not disjoint (policy: strict):
  groups "a" and "b": /etc/a (both write)
```

## Recipe

The full schema in one example, here with an Arch image as the base:

```yaml
# syntax=ghcr.io/nomira-os/nomira:1-alpha
settings:
  name: mysystem
  description: My system                    # becomes the label org.opencontainers.image.description
  base: docker.io/library/archlinux:base     # any image; pin by digest in real recipes
  validation: strict                        # file default: strict | allow-identical
  labels:                                   # labels on the finished image
    org.example.owner: me
  env:                                      # environment for all run commands (build time only)
    LANG: C.UTF-8
  args:
    VERSION: "1.0"                          # ${VERSION} in commands; --build-arg overrides
  platforms: [linux/amd64]                  # information for CLI and CI, see below
  tags: ["${VERSION}", stable]              # information for CLI and CI, see below

prepare:                                    # optional named stages (multi-stage)
  aurbuild:
    from: docker.io/library/archlinux:base-devel
    commands:
      - run --no-cache git clone https://aur.archlinux.org/example.git /src

phases:                                     # list: order matters
  - name: packages
    groups:                                 # mapping: groups are independent, key = name
      system:                               # a single group = the serial case, no validation
        - run --mount=type=cache,target=/usr/lib/sysimage/cache/pacman/pkg pacman -S --needed --noconfirm kmscon
  - name: configs
    validation: allow-identical             # a phase overrides the default
    shared: [/usr/lib/tmpfiles.d/]          # allowlist (suffix "/" = prefix)
    env: {MAKEFLAGS: -j4}                   # environment for this phase
    groups:
      kmscon:
        - env THEME=dark                    # applies to the following commands of the group
        - copy --chmod=0644 files/kmscon/ /etc/kmscon/
      aur:
        - copy --from=aurbuild /out/ /usr/lib/aur/
```

Commands are strings that start with a verb (use a YAML block scalar for
several lines):

| Command | Flags |
|---|---|
| `run <shell>` | `--no-cache`, `--mount=type=cache,target=/path[,id=…][,sharing=shared\|private\|locked]` |
| `copy <src>… <dst>` | `--from=<stage>`, `--chmod=0644`, `--chown=UID:GID` |
| `env NAME=VALUE` | none; everything after the first `=` is the value |

**Args:** `${NAME}` is replaced only for args declared in `settings.args`;
`$repo` and the like are left alone. A changed arg invalidates exactly the
commands that use it.

**Environment:** `settings.env` applies to all `run` commands, `env` on a
phase to its groups, the `env` command to the rest of its group. The more
specific one wins. The variables exist only during the build and do not
end up in the image configuration.

**`run --no-cache`:** The step always runs, even if BuildKit has it in its
cache. Everything that builds on it then runs again: the rest of its group
and all following phases. Such steps therefore belong in a group of their
own as late as possible, or in a `prepare` stage.

**`platforms` and `tags`:** The frontend checks both and reports them
through `inspect`, but does not act on them. Which architecture is built
and which tags are published is up to the caller (CLI or CI).

## Splitting a recipe across files

A recipe has one entry file; everything else can be moved out with
`include`. Paths are relative to the including file and must stay inside
the recipe directory. Included files may include further files; cycles are
an error.

```yaml
prepare:
  aurbuild:
    from: docker.io/library/archlinux:base-devel
    commands: {include: stages/aur.yml}     # file with a list of commands

phases:
  - include: phases/base.yml                # file with one phase or a list of phases
  - name: configs
    groups:
      kmscon: {include: groups/kmscon.yml}  # file with a list of commands
      networkmanager:
        - copy configs/nm/ /etc/NetworkManager/
```

Wherever a list of commands is expected, `include: <file>` works instead;
in the list of phases, `- include: <file>` pulls in whole phases.
`settings` exists only in the entry file. Source paths of `copy` still
refer to the build context, not to the included file.

## Building a recipe

The frontend image is published as `ghcr.io/nomira-os/nomira`, for
`linux/amd64` and `linux/arm64`, signed with cosign:

- **Releases** are cut by hand with the "release" workflow (`beta` or
  `final`) and carry the version number as their tag. Every release also
  moves `1-alpha`, which recipes refer to.
- **Every commit on `main`** is published as the immutable development
  build `sha-<commit>`.

Version and changelog follow from the Conventional Commits, see
[Nomira-OS/ci](https://github.com/Nomira-OS/ci). Building locally works
too:

```sh
podman build -f Dockerfile.frontend -t ghcr.io/nomira-os/nomira:1-alpha .
```

Any vanilla BuildKit then builds the recipe; the `# syntax=` line selects
the frontend:

```sh
buildctl build \
  --frontend dockerfile.v0 \
  --local context=. --local dockerfile=. \
  --opt filename=recipe.yml \
  --opt build-arg:VERSION=1.1 \
  --output type=oci,dest=img.tar
```

Or explicitly, without a `# syntax` line (handy for testing local builds
of the frontend):

```sh
buildctl build \
  --frontend gateway.v0 --opt source=ghcr.io/nomira-os/nomira:1-alpha \
  --local context=. --local dockerfile=. \
  --opt filename=recipe.yml \
  --output type=oci,dest=img.tar
```

The result is an ordinary OCI image, for podman for instance:

```sh
skopeo copy oci-archive:img.tar containers-storage:localhost/mysystem
```

## Why it was created

The frontend is one part of [nomira](https://github.com/Nomira-OS): there
a recipe describes a whole operating system, which is built as a bootc
image and switched to atomically. The other parts are the
[CLI](https://github.com/Nomira-OS/nomira-cli) and a minimal
[Arch base image](https://github.com/Nomira-OS/nomira-image). That is
where the model pays off: a system consists of many parts that have
nothing to do with each other (terminal, network, boot loader), and a
change to one should not rebuild all the others.

So far the frontend has been tried with Alpine and Arch images.

## Helper modes (for thin CLIs)

The same binary serves as a parser oracle, so that a CLI in any language
does not have to reimplement schema validation and the graph:

```sh
nomira-frontend lint  recipe.yml     # parse + schema; exit 1 with errors
nomira-frontend graph recipe.yml     # pipeline as a Mermaid flowchart
nomira-frontend inspect recipe.yml   # name, base, phases and groups as JSON
podman run --rm -i ghcr.io/nomira-os/nomira:1-alpha lint - < recipe.yml
```

## Development

With normal network access this is enough:

```sh
go mod tidy && make test build
```

`hack/gen_offline_deps.py` exists for environments without a Go module
proxy: it generates a `go.offline.mod` that serves all dependencies via
`replace` from a local `moby/buildkit` checkout (its `vendor/` tree); the
build then uses `GOFLAGS="-mod=mod -modfile=go.offline.mod"`. The normal
`go.mod` is not affected.

Tests: `go test ./...` (parser, conflict check) plus
`hack/test_lister.sh`, which checks `internal/compile/lister.sh` against
synthetic trees (add, change, metadata, symlink, file and directory
deletion, paths with spaces).

## Status and known limits

- Collecting the write set hashes the whole upper tree of a group; phases
  with a single group (the pacman phase, for instance) skip diff, merge
  and validation entirely.
- `copy` does not support paths with spaces on the command line (the
  validation itself does); `--chown` is numeric only.
- The lister does not yet record xattrs and hard links as writes of their
  own (concept.md §10).
- The lister needs a toolbox image with busybox tools
  (`settings.toolbox`, default `docker.io/library/busybox:1.36.1`).
- The platform follows the build host; multi-arch is open.
- Built against BuildKit v0.35.0; MergeOp and DiffOp need ≥ 0.10.

## Licence

EUPL-1.2, see `LICENSES/`. Per-file information is in `REUSE.toml`.
