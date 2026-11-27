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
- **Groups** within a phase are declared independent: they are built in
  parallel, cached one by one and merged at the end of the phase
  (MergeOp).
- The independence is **verified during the build** (the write sets must
  be disjoint, with prefix semantics for whiteouts). A conflict aborts
  the build before anything is merged on top of it or exported.
- A group **without `run`** never sees the state before it. It is built
  apart from that state, so neither a new base image nor a change in an
  earlier phase rebuilds it.
- **Collectors** gather what must happen in one step, such as installing
  packages: groups name what they need, one command does it.

```
S ──run(g₁)──→ R₁ ──Diff──→ Δ₁  ┐
S ──run(g₂)──→ R₂ ──Diff──→ Δ₂  ├─ parallel, cached one by one, validated
    copy(g₃) ─────────────→ Δ₃  ┘  (no run: does not depend on S)
S' = Merge(S, Δ₁ … Δₙ)
```

When a group changes, only that group is rebuilt; its neighbours come
from the cache. `docs/concept.md` is the first draft of the model and is
kept as a record; this README is authoritative.

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

A recipe that uses everything, here with an Arch image as the base:

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
  compression: zstd                         # zstd | gzip | uncompressed, see below
  secrets:                                  # secret id: file on the build machine
    db.key: /etc/kernel/secureboot/db.key
  source_date_epoch: 1794268800             # default: creation time of the base image, see below
  record: true                              # true | false | a phase name, see below

prepare:                                    # optional named stages (multi-stage)
  aurbuild:
    from: docker.io/library/archlinux:base-devel
    commands:
      - run --no-cache git clone https://aur.archlinux.org/example.git /src

phases:                                     # list: order matters
  - name: packages                          # a collector, see below
    collect: run --mount=type=cache,target=/usr/lib/sysimage/cache/pacman/pkg pacman -S --needed --noconfirm ${items}

  - name: features
    validation: allow-identical             # a phase overrides the default
    shared: [/usr/lib/tmpfiles.d/]          # allowlist (suffix "/" = prefix)
    env: {MAKEFLAGS: -j4}                   # environment for this phase
    groups:                                 # mapping: groups are independent, key = name
      network:                              # short form: a list of commands
        - copy files/nm/ /etc/NetworkManager/
      terminal:                             # long form
        packages: [kmscon]                  # contribution to the phase "packages"
        services: [kmsconvt@tty1.service]   # contribution to the phase "services"
        expects: [/usr/lib/systemd/system/kmsconvt@.service]
        prepare:                            # stages that only this group uses
          theme:
            from: docker.io/library/alpine:3.22
            commands: [run mkdir /out && echo dark > /out/theme.conf]
        commands:
          - copy --chmod=0644 files/kmscon/ /etc/kmscon/
          - copy --from=theme /out/theme.conf /etc/kmscon/theme.conf
      aur:
        - copy --from=aurbuild /out/ /usr/lib/aur/

  - name: services
    collect: run systemctl enable ${items}
```

Commands are strings that start with a verb (use a YAML block scalar for
several lines):

| Command | Flags |
|---|---|
| `run <shell>` | `--no-cache`, `--mount=type=cache,target=/path[,id=…][,sharing=shared\|private\|locked]`, `--mount=type=tmpfs,target=/path`, `--mount=type=bind,from=NAME,target=/path[,source=/path]` (read-only), `--mount=type=secret,id=ID[,target=/path][,required=true]` |
| `copy <src>… <dst>` | `--from=NAME`, `--chmod=0644`, `--chown=UID:GID` |
| `env NAME=VALUE` | none; everything after the first `=` is the value |

**Args:** `${NAME}` is replaced only for args declared in `settings.args`;
`$repo` and the like are left alone. A changed arg invalidates exactly the
commands that use it.

**Environment:** `settings.env` applies to all `run` commands, `env` on a
phase to its groups, the `env` command to the rest of its group. The more
specific one wins. The variables exist only during the build and do not
end up in the image configuration.

**`from=` and `--from=`** name a prepare stage or a phase that comes
earlier; a phase stands for the system as it is after that phase. A bind
mount is read-only and never ends up in the image.

**A prepare stage can start from a phase** (`from: packages`): it is
built from the system after that phase, is cached as long as that phase
is, and only phases after it can use it. Nothing of it ends up in the
image unless a later phase copies it.

Together they let a recipe end the way bootc builds a sealed image, with
a unified kernel image. The base image keeps the initramfs in
`/usr/lib/modules/<kver>/` up to date whenever a package that ends up in
it changes, so the recipe only takes kernel and initramfs out of the image
and builds the UKI from the system without them:

```yaml
phases:
  # … packages, features, services …
  - name: kernel
    groups:
      out: ["run rm /usr/lib/modules/*/vmlinuz /usr/lib/modules/*/initramfs.img"]
  - name: uki
    groups:
      uki:
        - >-
          run --mount=type=bind,from=kernel,target=/target
          --mount=type=bind,from=services,source=/usr/lib/modules,target=/kernels
          mkdir -p /boot/EFI/Linux &&
          for d in /kernels/*; do
          bootc container ukify --rootfs /target --kernel-dir "$d" --erofs-version v1-only --
          --output "/boot/EFI/Linux/$(basename "$d").efi"; done
```

**Secrets:** `--mount=type=secret,id=ID` makes a file available at
`/run/secrets/ID` for that one step, for a signing key for instance. It
never ends up in a layer or in the cache key; `required=true` fails the
step when the secret is missing. Every secret a recipe mounts must be
declared in `settings.secrets` with the file on the build machine that
provides it (`secrets: {db.key: /etc/kernel/secureboot/db.key}`); the CLI
passes those files to BuildKit (`buildctl --secret`).

**`run --no-cache`:** The step always runs, even if BuildKit has it in its
cache. Everything that builds on it then runs again: the rest of its group
and all following phases. Such steps therefore belong in a `prepare`
stage, or in a group of their own as late as possible.

**`source_date_epoch`:** Every `run` sees it as `SOURCE_DATE_EPOCH`
(dracut, for one, then builds the same initramfs every time), and the
exported image takes it as its creation time. With the exporter option
`rewrite-timestamp=true`, which the nomira CLI always sets, the times of
the files in the recipe's layers are clamped to it as well; the base
image's layers stay as they are. Without the setting it is the creation
time of the base image, so a recipe builds the same image every time
from the same base.

**`record`:** The image carries the recipe it was built from, below
`/usr/share/nomira/recipe`: the entry file, every included file and what
the `copy` commands take from the build context, at their paths relative
to the recipe directory. Being below `/usr` it is part of the read-only,
verified image, and local changes cannot make it lie. The record is added
after the last phase before the first one that reads an earlier phase (a
bind mount or copy from it, or a prepare stage that starts from it), so
that what such a phase derives from the image, a UKI with its digest for
instance, covers it; without such a phase, after the last one.
`record: <phase>` puts it after that phase instead, `record: false`
leaves it out.

**What is loaded:** From the recipe directory the frontend reads only the
entry file and the files it includes, one by one as it meets them, and
from the build context only what `copy` commands name. Anything else in
the directory never leaves the machine, and `.nomira/` (the CLI's build
output) is always left out.

**`platforms`, `tags`, `compression` and `secrets`:** The frontend checks
them and reports them through `inspect`; the caller acts on them, since
BuildKit leaves the platform, the tags, the exporter and the secrets to
the client. The nomira CLI uses `compression` for the exported layers and
passes the `secrets` files on; everything that shapes the image is thus
in the recipe, not in command line flags.

## Groups

A group is written in one of two forms. The short form is a list of
commands. The long form is a mapping; four keys have a fixed meaning:

| Key | Meaning |
|---|---|
| `commands` | The list of commands |
| `prepare` | Stages that belong to this group. Their names are local to it and win over stages of the same name at the top |
| `expects` | Paths that must exist in the state the phase starts from |
| `include` | Read the group from a file; no other key next to it |

Every other key is a **contribution** to the collector phase of that
name. A group may consist of contributions alone.

**`expects`** is checked on every build, without rebuilding the group.
It is for groups that override or extend something: if a new base image
no longer has the unit a drop-in refers to, the build says so instead of
silently shipping a drop-in for nothing.

## Collectors

Some things must happen in one step. Two groups that both install a
package both write the package database, so they can never be
independent. A collector solves this: a phase with `collect` instead of
`groups` runs one command, and `${items}` in it is replaced by what all
groups of the recipe contributed under the name of the phase.

```yaml
- name: packages
  collect: run pacman -S --needed --noconfirm ${items}
```

- The frontend knows nothing about package managers or services. A
  collector is whatever command you give it; a recipe on another
  distribution uses `dnf install` in the same place.
- Contributions come from groups of any phase, also later ones. Where the
  collector stands in the list of phases decides when it runs.
- Items are sorted and de-duplicated, so the command does not depend on
  the order of groups or files. A collector without items is skipped.
- An item is a single word of letters, digits and `@%+=:,./_-`, and does
  not start with `-`. Anything else is an error before the build.

A collector runs as one step: when one item changes, the whole command
runs again. Use it where that is the nature of the operation, or where
the command is quick.

## What a rebuild costs

A group that uses `run` is executed on the state before it. A new base
image, or a change in an earlier phase, therefore runs it again.

A group without `run` only copies files, from the recipe directory or
from a `prepare` stage. The frontend builds it apart from the state
before it and merges the result, the way `COPY --link` works in a
Dockerfile. Its cache then depends only on what it copies.

The goal for a recipe is therefore: **`run` in `prepare` stages and
collectors, copies in groups.** A script that generates a file runs in a
`prepare` stage with its own image; the group copies the result.

Building apart is only done where it gives the same result. On every
build a check compares the group with the state before it and falls back
to building on that state if

- a directory the group copies into is a symbolic link there, or exists
  with another mode or owner, or
- the group puts a file where a directory or a symbolic link is, or the
  other way round.

The fallback is always correct; it only costs the rebuild. Because that
cost is easy to miss, the steps of such a group say so in the output of
every build, also when they come from the cache:

```text
[features/sudo, WARNING: not independent of earlier state: /etc/sudoers.d has mode.uid.gid 750.0.0 there, not 755.0.0] copy files/sudoers-wheel -> /etc/sudoers.d/10-wheel
```

## Splitting a recipe across files

A recipe has one entry file. `include: <file>` can stand in place of a
phase, a group or a list of commands and is replaced by the content of
the file. Paths are relative to the including file and must stay inside
the recipe directory. Included files may include further files; cycles
are an error.

```yaml
phases:
  - include: phases/base.yml                # one phase or a list of phases
  - name: features
    groups:
      terminal: {include: features/terminal.yml}   # a group, short or long form
      aur:
        prepare:
          build:
            from: docker.io/library/archlinux:base-devel
            commands: {include: stages/aur.yml}     # a list of commands
        commands:
          - copy --from=build /out/ /usr/lib/aur/
```

A group file in the long form brings everything the feature needs:

```yaml
# features/terminal.yml
packages: [kmscon]
services: [kmsconvt@tty1.service]
commands:
  - copy --chmod=0644 files/kmscon/ /etc/kmscon/
```

`settings` exists only in the entry file. Source paths of `copy` always
refer to the build context, not to the included file. `examples/` has a
recipe laid out this way.

## Building a recipe

The frontend image is published as `ghcr.io/nomira-os/nomira`, for
`linux/amd64` and `linux/arm64`, signed with cosign:

- **Releases** are cut by hand with the "release" workflow (`beta` or
  `final`) and carry the version number as their tag. Every release also
  moves `1-alpha`, which recipes refer to.
- **Every commit on `main`** is published as the immutable development
  build `sha-<short commit>`, such as `sha-f7728e3`.

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
[Arch base image](https://github.com/Nomira-OS/arch-base). That is
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

Tests come in three layers:

- `go test ./...`: parser, conflict check, shape of the build graph.
- `hack/test_lister.sh` and `hack/test_guard.sh` run the two shell
  scripts the build uses against synthetic trees.
- `hack/e2e.sh` builds small recipes with a real BuildKit and checks the
  results: conflicts between writes and deletions, the policies,
  collectors, includes and group-local stages, `expects`, that a group
  without `run` comes from the cache after a change of the base image,
  and that the fallback keeps symbolic links and directory modes intact.
  CI runs it on every push; the header of the script says what it needs
  to run locally.

## Status and known limits

- Everything in this README is covered by the end-to-end tests against
  BuildKit v0.35.0, except args, `run --no-cache`, labels and `--chown`.
  Larger real systems have hardly been built yet.
- The write set of a group is read from its layer, so only the files it
  wrote are hashed. For a group with `run` the trees before and after
  are additionally walked, without reading files, to find deletions.
  Every file in the layer counts as written, also one that was rewritten
  with the same content: its layer would still cover what a sibling
  writes to that path. `validation: allow-identical` permits that case.
- A phase with a single group that uses `run` skips diff, merge and
  validation entirely.
- The checks of a phase run in parallel and are cached by BuildKit; they
  only run again when the group or the state before it changed.
- `copy` does not support paths with spaces on the command line (the
  validation itself does); `--chown` is numeric only.
- `copy --from` inside a `prepare` stage is not supported yet.
- The lister does not yet record xattrs and hard links as writes of their
  own.
- The checks need a toolbox image with busybox tools
  (`settings.toolbox`, default `docker.io/library/busybox:1.36.1`).
- The platform follows the build host; multi-arch is open.
- Built against BuildKit v0.35.0; MergeOp and DiffOp need ≥ 0.10.

## Licence

EUPL-1.2, see `LICENSES/`. Per-file information is in `REUSE.toml`.
