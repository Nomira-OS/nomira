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
- The independence is **verified during the build**, see
  [The conflict check](#the-conflict-check). A conflict aborts the build
  before anything is merged on top of it or exported.
- A group **without `run`** never sees the state before it. It is built
  apart from that state, so neither a new base image nor a change in an
  earlier phase rebuilds it.
- **Collectors** gather what must happen in one step, such as installing
  packages: groups name what they need, one command does it.

```
S ──run(g₁)──→ R₁ ──Diff──→ Δ₁  ┐
S ──run(g₂)──→ R₂ ──Diff──→ Δ₂  ├─ parallel, cached one by one, checked
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
  base: docker.io/library/alpine:3.22
build:
  content:
    a: ["run echo a > /etc/a"]
    b: ["run echo b > /etc/b"]
```

`build` lists the phases by name, here one, `content`; under a phase stand
its groups. If `a` and `b` write different things to the same file, the
build aborts:

```text
phase "content": its groups are not independent:
  groups "a" and "b": /etc/a (both write, with different content, mode or owner)
  fix: make it one group, or move one of them into a phase of its own
```

## Recipe

A recipe that uses everything, here with an Arch image as the base:

```yaml
# syntax=ghcr.io/nomira-os/nomira:1-alpha
imports: [features/terminal.yml]            # other recipe files, see Imports

settings:
  base: docker.io/library/archlinux:base    # any image; pin by digest in real recipes
  labels:                                   # labels on the finished image
    org.opencontainers.image.description: My system
  env:                                      # environment for every run (build time only)
    LANG: C.UTF-8
  mounts:                                   # mounts for every run, key = target
    /var/cache/pacman/pkg: type=cache,sharing=locked
    /var/log: type=tmpfs
  source_date_epoch: 1794268800             # seconds | true (default: the base image's time) | false
  record: true                              # the resolved recipe as the label org.nomira.recipe

stages:                                     # named stages with an image of their own
  aurbuild:
    from: docker.io/library/archlinux:base-devel
    commands:
      - run --no-cache git clone https://aur.archlinux.org/example.git /src

build:                                      # the phases, by name; their order matters
  packages:                                 # a phase that is one collector
    collect: run pacman -S --needed --noconfirm ${items}

  features:
    env: {MAKEFLAGS: -j4}                   # environment for this phase
    network:                                # a group, short form: a list of commands
      - copy files/nm/ /etc/NetworkManager/
    aur:                                    # key = group name; groups are independent
      - copy --from=aurbuild /out/ /usr/lib/aur/
    locales:                                # a collector among the groups
      collect: run locale-gen ${items}
    german:                                 # long form
      locales: [de_DE.UTF-8]                # contribution to the collector "locales"
      commands:
        - env LC=de_DE.UTF-8
        - run echo "LANG=$LC" > /etc/locale.conf

  services:
    collect: run systemctl enable ${items}
```

Commands are strings that start with a verb (use a YAML block scalar for
several lines):

| Command | Flags |
|---|---|
| `run <shell>` | `--no-cache`, `--mount=type=cache,target=/path[,id=…][,sharing=shared\|private\|locked]`, `--mount=type=tmpfs,target=/path`, `--mount=type=bind,from=NAME,target=/path[,source=/path]` (read-only), `--mount=type=secret,id=ID[,target=/path][,required=true]` |
| `copy <src>… <dst>` | `--from=NAME`, `--chmod=0644`, `--chown=UID:GID` |
| `env NAME=VALUE` | none; everything after the first `=` is the value |

A JSON Schema of the recipe is in
[`schema/recipe.schema.json`](schema/recipe.schema.json), for editors
that complete and check YAML with it; recipes stay YAML. The frontend
checks more than the schema can (names that `from` refers to, items,
the order of phases), and its errors name the file and line.

**Environment:** `settings.env` applies to every `run`, `env` on a phase
to its groups, the `env` command to the commands after it in its group or
stage. The more specific one wins. The variables exist only during the
build and do not end up in the image configuration. A changed value
reruns what it reaches: a change in `settings.env` every run of the
recipe, an `env` command only the commands after it. `${…}` is left to
the shell; the frontend replaces nothing.

**`from=` and `--from=`** name a stage, or a phase that comes earlier; a
phase stands for the system as it is after that phase. A bind mount is
read-only and never ends up in the image. A stage always starts from an
image.

Together they let a recipe end the way bootc builds a sealed image, with
a unified kernel image. The base image keeps the initramfs in
`/usr/lib/modules/<kver>/` up to date whenever a package that ends up in
it changes, so the recipe only takes kernel and initramfs out of the image
and builds the UKI from the system without them:

```yaml
build:
  # … packages, features, services …
  kernel:
    out: ["run rm /usr/lib/modules/*/vmlinuz /usr/lib/modules/*/initramfs.img"]
  uki:
    uki:
      - >-
        run --mount=type=bind,from=kernel,target=/target
        --mount=type=bind,from=services,source=/usr/lib/modules,target=/kernels
        mkdir -p /boot/EFI/Linux &&
        for d in /kernels/*; do
        bootc container ukify --rootfs /target --kernel-dir "$d" --erofs-version v1-only --
        --output "/boot/EFI/Linux/$(basename "$d").efi"; done
```

**`settings.mounts`** are added to every `run`, in stages too, unless the
run mounts something at the same target itself. The value is what
`--mount=` takes, without the target: `type=tmpfs` keeps logs and
temporary files out of the image, `type=cache,…` keeps downloads between
builds. Only these two types apply to every run. Without the setting
nothing is mounted. On a bootc system, `/run` and `/tmp` are tmpfs at
runtime, and bootc takes `/var` from an image only at the first install
(`bootc container lint` warns about logs and leftovers there), so a
recipe for one has reason to mount tmpfs there.

**Secrets:** `--mount=type=secret,id=ID` makes a file available at
`/run/secrets/ID` for that one step, for a signing key for instance. It
never ends up in a layer or in the cache key; `required=true` fails the
step when the secret is missing. Where the secret comes from is the
client's business (`buildctl --secret id=ID,src=FILE`; the nomira CLI
takes it from `--secret` or `nomira.conf`).

**`run --no-cache`:** The step always runs, even if BuildKit has it in its
cache. Everything that builds on it then runs again: the rest of its group
and all following phases. Such steps therefore belong in a stage, or in
a group of their own as late as possible.

**`source_date_epoch`:** Every `run` sees it as `SOURCE_DATE_EPOCH`
(dracut, for one, then builds the same initramfs every time), and the
exported image takes it as its creation time. With the exporter option
`rewrite-timestamp=true` (a client option; the nomira CLI always sets it), the times of
the files in the recipe's layers are clamped to it as well; the base
image's layers stay as they are. Without the setting it is the creation
time of the base image, so a recipe builds the same image every time
from the same base. `source_date_epoch: false` turns all of that off:
no variable, the build time as creation time, file times as they are.

**`record`:** The image carries the recipe it was built from in its label
`org.nomira.recipe`: resolved, with all imports merged, so that it
describes the build by itself and later changes to imported files cannot
change it. Comments name the file each setting, stage, phase and group
came from. The label is part of the image's config and with it of the
image digest and its signature; it is no layer, so changing only the
recipe rebuilds no step. `nomira recipe export` writes it to a file,
`nomira recipe diff` compares two recipes. Files that `copy` takes from
the recipe directory are not in the label; they are in the image where
they were copied to. `record: false` leaves the label out. The label
`org.nomira.frontend.version` names the frontend that built the image.

**What is loaded:** From the recipe directory the frontend reads only the
entry file and the files it imports, one by one as it meets them, and
from the build context only what `copy` commands name. Anything else in
the directory never leaves the machine, and `.nomira/` (the CLI's build
output) is always left out.

**What is not in a recipe:** whatever only the BuildKit client can set.
The platform, image names, the exporter and its options (format, layer
compression, `rewrite-timestamp`) and the files behind secrets are
options of `buildctl`; a frontend cannot set them, so a recipe does not
name them either. The rule: what has to be a client option does not
belong in the recipe. The frontend builds for the platform BuildKit asks
for.

## Groups

A group is written in one of two forms. The short form is a list of
commands. The long form is a mapping; three keys have a fixed meaning:

| Key | Meaning |
|---|---|
| `commands` | The list of commands |
| `collect` | Makes the group a collector, see below; then it has no other key |
| `env` | Not allowed: environment is set on the phase or with the `env` command |

Every other key is a **contribution** to the collector of that name. A
group may consist of contributions alone.

Under a phase, `env` is the phase's environment and every other key is a
group. A phase with `collect` directly under it is the short form of a
phase with one collector named after the phase.

## Collectors

Some things must happen in one step. Two groups that both install a
package both write the package database, so they can never be
independent. A collector solves this: a group with `collect` runs one
command, and `${items}` in it is replaced by what all groups of the
recipe contributed under the collector's name.

```yaml
build:
  packages:                     # a phase that is one collector, "packages"
    collect: run pacman -S --needed --noconfirm ${items}
  setup:
    services:                   # two collectors in one phase
      collect: run systemctl enable ${items}
    locales:
      collect: run locale-gen ${items}
    hostname: ["copy files/hostname /etc/hostname"]
```

- Collector names are unique in the recipe, since contributions refer to
  them by name. Several collectors in one phase are groups like any
  other: they run side by side and the conflict check covers them.
- The frontend knows nothing about package managers or services. A
  collector is whatever command you give it; a recipe on another
  distribution uses `dnf install` in the same place.
- Contributions come from groups of any phase, also later ones. Where the
  collector stands among the phases decides when it runs.
- Items are sorted and de-duplicated, so the command does not depend on
  the order of groups or files. A collector without items is skipped.
- An item is a single word of letters, digits and `@%+=:,./_-`, and does
  not start with `-`. Anything else is an error before the build.

A collector runs as one step: when one item changes, the whole command
runs again. Use it where that is the nature of the operation, or where
the command is quick.

### Collectors that fan out

Installing packages is the case where one step costs most: one new
package, and all of them are unpacked again into one new, large layer.
A collector can instead fan out, in three commands:

```yaml
build:
  packages:
    collect:
      resolve: run pacman -Sp --needed --print-format '%n %l sha256:%h %e' ${items}
      each: run pacman --root /nomira/out --dbpath /tmp/db -U --noconfirm --nodeps --noscriptlet /nomira/in/*
      then: run pacman -U --noconfirm --overwrite '*' … /nomira/in/*
```

```
S ─ resolve ─→ list of files
       fetch ×n (BuildKit, by checksum, parallel)
S ─ each(bucket 1) ─→ /nomira/out ┐
S ─ each(bucket 2) ─→ /nomira/out ├─ parallel, cached one by one
S ─ each(bucket k) ─→ /nomira/out ┘   (check: no file in two buckets)
Merge(S, buckets…) ─ then ─→ result of the collector
```

- **resolve** runs on the state the phase starts from, with the items,
  and prints one line per file to fetch: name, URL, `sha256:<hex>`, and
  optionally a key (default: the name). The frontend reads the list, so
  everything after it waits for it.
- **fetch:** BuildKit downloads every file itself, checked against its
  checksum and cached by it, shared by every build.
- **each** runs once per bucket on the same state, with the bucket's
  files in `/nomira/in`; what it writes to `/nomira/out` is the bucket's
  layer. A file's bucket depends on its key alone (rendezvous hashing
  over 64 buckets): adding or removing a file changes only its own
  bucket, and an unchanged bucket is neither run nor exported again. A
  key shared by files that change together, a source package for one,
  keeps them in one bucket.
- **Buckets must not overlap:** a file in two buckets is a conflict,
  even when it is the same, and stops the build.
- **then** runs on the merged result with all files in `/nomira/in`, to
  finish: a package database, install scripts, hooks. Optional. It may
  use `${items}` too, to tell what was asked for from what came along
  as a dependency; `each` may not, since every bucket would then run
  again when one item changes.

The frontend knows no package manager: what the three commands do is up
to the recipe, and a base image can offer them as a part to import.

## The conflict check

Merging the layers of a phase's groups must give what running them one
after the other would, in any order. The frontend checks that on every
build, for every phase with more than one group, before anything is
merged on top of it.

**What counts.** A group's layer is what the merge lays on top: every
regular file and symbolic link in it is a *write*, also one that was
rewritten with the same content, because the layer would still cover
what a sibling writes there. For a group with `run` the trees before and
after it are walked as well, to find what it *deleted*: a file, or a
whole directory. A group without `run` cannot delete anything.

**What is allowed** is exactly what is the same in every order:

- two groups write a path with the same content, mode and owner;
- two groups delete the same file, or the same directory;
- a group deletes something below a directory another one deletes.

**What is a conflict:** different writes to one path; a write and a
deletion of one path; a write below a directory another group deletes; a
file in one group where the other deletes a directory of that name. The
build stops with every conflict listed by group and path. The fix is
always one of two: make the groups one, or put one of them into a phase
of its own. There is no allowlist; two groups that really write the same
file differently depend on their order, and that order belongs in the
recipe.

**How it runs**, in two passes, each a small step in a busybox image that
mounts the trees read-only:

1. **List** (`internal/compile/lister.sh`): one `find` per tree, no file
   is read. Each group yields its writes and deletions.
2. **Hash** (`internal/compile/hash.sh`): only the paths that more than
   one group writes, usually none or a few, get a sha256 of their content
   (of the target for a symbolic link) plus mode and owner, and only in
   the groups that write them.

The comparison itself is done by the frontend (`internal/conflict`). The
steps are cached like any other: they run again only when the group or
the state before it changed. A phase with a single group that uses `run`
has nothing to check and skips diff and merge as well.

**Limits:** xattrs and hard links are not recorded as writes of their
own; paths that contain a newline are not supported.

## What a rebuild costs

A group that uses `run` is executed on the state before it. A new base
image, or a change in an earlier phase, therefore runs it again.

A group without `run` only copies files, from the recipe directory or
from a stage. The frontend builds it apart from the state
before it and merges the result, the way `COPY --link` works in a
Dockerfile. Its cache then depends only on what it copies.

The goal for a recipe is therefore: **`run` in stages and collectors,
copies in groups.** A script that generates a file runs in a stage with
its own image; the group copies the result.

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
[2/3 features/sudo, WARNING: not independent of earlier state: /etc/sudoers.d has mode.uid.gid 750.0.0 there, not 755.0.0] copy files/sudoers-wheel /etc/sudoers.d/10-wheel
```

## Imports

A recipe can be split across files, and recipes can share parts.
`imports:` at the top of a file lists further recipe files. Each is a
recipe or a part of one and may import further files; a cycle is an
error.

- A **path** is relative to the importing file and stays inside the
  recipe directory.
- A **URL** is fetched by BuildKit. `#sha256=<hex>` at its end pins the
  content: a file that does not match fails the build, and a pinned file
  is cached by its pin. An `https` URL may go without a pin; an `http`
  URL may not, since nothing else vouches for what it serves. A file
  read from a URL imports relative to that URL, so a set of parts on a
  web server can import each other.

```yaml
# recipe.yml
imports:
  - https://raw.githubusercontent.com/Nomira-OS/arch-base/<commit>/recipes/base.yml
  - features/terminal.yml  # one feature
build:
  features:
    network: null          # base.yml has it; this machine does not
```

```yaml
# features/terminal.yml: a feature is a part of a recipe
build:
  features:
    terminal:
      packages: [kmscon]
      services: [kmsconvt@tty1.service]
      commands:
        - copy --chmod=0644 files/kmscon/ /etc/kmscon/
```

The files are merged into one recipe: the imports in the order listed,
each after its own imports, and the importing file last. The rules are
those of JSON Merge Patch (RFC 7386):

- **Mappings are merged** key by key: settings, labels, env, mounts,
  stages, phases, groups.
- **Everything else is replaced** by the later file: strings, numbers,
  and lists (a group's commands or packages).
- **`key: null` removes** a key: a group, a stage, a phase, a label, a
  mount.

The first file that names a key decides its place; keys new in a later
file come after it. Phases are the exception, since their order matters:
a new phase goes after the phase that comes before it in its own file, or
before the next one it names if it is the first there, so an import can
add `firmware` in front of `packages`. Two files that order the same
phases differently are an error.

Source paths of `copy` always refer to the build context, the recipe
directory, not to the importing file; an imported file from a URL can
copy only what the recipe directory has. `buildctl … --opt mode=resolve`
(and `nomira recipe export` on an image) shows the merged result.
`examples/` has a recipe laid out this way.

## Building a recipe

The frontend image is published as `ghcr.io/nomira-os/nomira`, for
`linux/amd64` and `linux/arm64`, signed with cosign:

- **Releases** are cut by hand with the "release" workflow (`beta` or
  `final`) and carry the version number as their tag. Every release also
  moves `1-alpha`, which recipes refer to.
- **Every commit on `main`** is published as the immutable development
  build `sha-<short commit>`, such as `sha-f7728e3`, and moves `latest`.
  `latest` is for trying things out; recipes that should build the same
  tomorrow pin `sha-…` or a release. BuildKit resolves the tag in
  `# syntax=` on every build.

The image reports its version, the release version or `sha-<short
commit>` for builds of `main`: in the label `org.nomira.frontend.version`
of every image it builds, in the file `version` of lint and resolve, and
with `nomira-frontend version`.

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

Options the frontend takes (`--opt`):

| Option | Meaning |
|---|---|
| `filename` | The recipe in the `dockerfile` local; default `recipe.yml` |
| `mode` | `build` (default); `lint` only parses the recipe and its imports; `resolve` outputs the recipe with all imports merged as `recipe.yml`. lint and resolve also output `version`; use them with `--output type=local,dest=DIR` |
| `image-resolve-mode` | `default`, `pull` or `local`, as the Dockerfile frontend takes it, for the base image and stages |
| `toolbox` | The busybox image the checks run in; default `docker.io/library/busybox:1.38.0` |
| `check` | `on` (default) or `off`: the conflict checks between groups and between buckets. Off is faster and merges overlapping groups in a fixed order without a word |

Caches that buildctl imports (`--import-cache`) are passed on to every
solve of the frontend, so steps whose inputs are unchanged come from
there: from an image's own inline cache (`--export-cache type=inline`,
also with the `oci` exporter and `rewrite-timestamp=true`), a registry
cache or a local directory. A collector that fans out then takes every
unchanged bucket from the import; `then`, which needs all files, runs
and downloads them.

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

- `go test ./...`: parser, conflict check, shape of the build graph, and
  that `examples/` parses.
- `hack/test_lister.sh` and `hack/test_guard.sh` run the shell scripts
  the build uses against synthetic trees.
- `hack/e2e.sh` builds small recipes with a real BuildKit and checks the
  results: conflicts between writes and deletions, identical writes,
  collectors, imports by path and URL, mounts, lint and resolve,
  that a group without `run` comes from the cache after a
  change of the base image, and that the fallback keeps symbolic links
  and directory modes intact. CI runs it on every push; the header of
  the script says what it needs to run locally.

## Status and known limits

- Everything in this README is covered by the end-to-end tests against
  BuildKit v0.35, except `run --no-cache`, labels and `--chown`.
  Larger real systems have hardly been built yet.
- `copy` does not support paths with spaces on the command line (the
  conflict check itself does); `--chown` is numeric only.
- `copy --from` inside a stage is not supported yet.
- The platform follows the build host; multi-arch is open.
- Built against BuildKit v0.35; MergeOp and DiffOp need ≥ 0.10.

## Licence

EUPL-1.2, see `LICENSES/`. Per-file information is in `REUSE.toml`.
