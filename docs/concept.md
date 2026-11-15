# Concept: a phase-based, verified-parallel OCI builder for bootc systems

**Name:** `nomira`
**Status:** draft v1 · 2026-11-14, kept as a historical record. The recipe
schema (`# syntax`, `args`, `prepare`, string commands) and the roadmap have
moved on since; the README is authoritative.

## 1. Goal

A recipe-driven build tool for bootc system images (primarily Arch Linux) that
organises build steps into **phases** and **groups**:

- Phases run **serially** in the declared order.
- Groups within a phase are **declared independent**: they fan out from the
  same base state, are built in parallel, cached one by one and merged at the
  end of the phase.
- The independence is **verified during the build** (disjoint write sets). A
  conflict aborts the build before anything is merged or exported.

A change to one group therefore invalidates only its own layer plus the cheap
merge and the following phases, not its sibling groups. This brings the
granularity of Nix derivations to the Containerfile model without giving up
its flexibility (arbitrary shell, Arch packages).

## 2. Scope and non-goals

**In scope**

- Recipe format (YAML) → LLB graph → OCI image via BuildKit
- Conflict validation with a configurable policy
- Delivery into podman's containers-storage and hand-over to `bootc switch`
- Pinning for reproducibility through the Arch Linux Archive (snapshot date)
- Post-build gates (hooks, bcvk boot test) in the `switch` workflow

**Non-goals**

- Not a BlueBuild fork and no compatibility with its recipes or modules. The
  only common ground is bootc; the build layer is an independent sibling
  project.
- No Fedora- or distribution-specific modules in v1. The recipe is
  distribution-agnostic; Arch is the reference target.
- No package manager of its own, no configuration merging like Nix modules.
- v1 is not a `# syntax=` frontend (see roadmap).

## 3. Build model

### 3.1 Structure

```
Pipeline  = ordered list of phases              (list → order matters)
Phase     = set of independent groups           (mapping → order irrelevant)
Group     = serial list of commands             (list → order matters)
```

Design principle: **what is ordered is a list, what is independent is a
mapping.** The syntax mirrors the semantics; a phase with exactly one group is
the serial case (`base`, `finish`) without special treatment.

### 3.2 Mapping to LLB

For a phase with input state `S` and groups `g₁…gₙ`:

```
S ──Exec/File(g₁)──→ R₁ ──DiffOp(S,R₁)──→ Δ₁   ┐
S ──Exec/File(g₂)──→ R₂ ──DiffOp(S,R₂)──→ Δ₂   ├─ parallel, cached one by one
S ──Exec/File(gₙ)──→ Rₙ ──DiffOp(S,Rₙ)──→ Δₙ   ┘
S' = MergeOp(S, Δ₁, …, Δₙ)        → input of the next phase
```

- Every group runs **on `S`**, never on the result of a sibling group. Read
  dependencies between groups are thereby ruled out by construction (they fail
  deterministically instead of silently working).
- Commands within a group chain serially (ExecOp→ExecOp…); `copy` becomes a
  FileOp.
- Export: every Δᵢ becomes **an image layer of its own with a stable digest**.
  The diffs are not a by-product: the merge of the diffs *is* the image.
  Validation data (write lists) never ends up in the export.

### 3.3 Cache semantics

- A change in group g → rebuilt: Δ_g, the MergeOp of the phase, all following
  phases.
- Unchanged: all sibling Δ (from the cache, stable digests).
- MergeOp is lazy and metadata-based (overlay-like), nothing is copied.

## 4. Recipe schema v1

### 4.1 Example

```yaml
version: 1

settings:
  name: mysystem
  base-image: ghcr.io/…/arch-bootc@sha256:…   # pinned by digest
  arch-snapshot: "2026/11/10"                 # Arch Linux Archive, see §7
  validation: strict                          # file default

phases:
  - name: base
    groups:
      system:
        - run: pacman -Syu --needed --noconfirm kmscon networkmanager apparmor …

  - name: configs
    validation: allow-identical               # a phase overrides the default
    shared:                                   # allowlist of deliberately shared paths
      - /usr/lib/tmpfiles.d/common.conf
    groups:
      kmscon:
        - run: ./scripts/install-kmscon.sh
        - copy: [files/kmscon/, /etc/kmscon/]
      networkmanager:
        - copy: [configs/nm/, /etc/NetworkManager/]
      boot:
        - run: |
            python3 ./scripts/gen-boot-params.py \
              --out /usr/lib/bootc/kargs.d/

  - name: finish
    groups:
      enable:
        - run: systemctl enable kmscon NetworkManager apparmor
```

### 4.2 Fields

| Field | Level | Meaning |
|---|---|---|
| `version` | file | schema version, required |
| `settings.name` | file | image name (`localhost/<name>`) |
| `settings.base-image` | file | base; pinning by digest recommended |
| `settings.arch-snapshot` | file | archive date; empty = unpinned (warning) |
| `settings.validation` | file | default policy for all phases |
| `phases[].name` | phase | required, unique; appears in logs and errors |
| `phases[].validation` | phase | overrides the default |
| `phases[].shared` | phase | path allowlist, exempt from the conflict check |
| `phases[].groups.<name>` | group | key = group name; value = list of commands |
| `- run:` | command | shell (`/bin/sh -c`), block scalar for several lines |
| `- copy: [src, dst]` | command | from the build context into the image (FileOp) |

Deliberately **not** provided: validation at group level. Disjointness is a
property of *pairs* of groups within a phase; a per-group policy would be
ambiguous when two groups conflict.

Schema validation (structure, unknown keys, empty phases, duplicate group
names) runs completely **before** the first build step.

## 5. Conflict validation

### 5.1 Collecting the write set

A write/delete list is collected per group. Mechanism: an internal ExecOp gets
`S` and `Rᵢ` mounted side by side and emits the list as a file, which the
frontend reads through the gateway API. (Merely walking the diff with
`ReadDir` does not show deletions reliably, hence the explicit comparison
step.) The lists exist only as build references and are never exported.

### 5.2 Rules

- **Shared directories are never an error.** A conflict exists only at file
  level (the same path as a file in two or more groups).
- **A type conflict is an error:** group A creates the file `/etc/foo`, group
  B the directory `/etc/foo/`.
- **Whiteouts have prefix semantics:** if a group deletes `/etc/foo/`
  (directory whiteout), this collides with *every* path below `/etc/foo/` in
  every other group, not only with identical paths. Deleting a file collides
  with writing the same path.
- Metadata changes (chmod or chown without a content change) count as writing
  the path.

### 5.3 Policies

| Policy | Behaviour |
|---|---|
| `strict` | every file conflict is an error |
| `allow-identical` | the same path is allowed if the content hash is identical (metadata must match as well) |
| + `shared:` | allowlist per phase, exempt from the check (applies to both policies) |

### 5.4 Failure behaviour

Conflict ⇒ **the build fails**, before MergeOp and export. The error message
uses the terms of the recipe:

```
error: phase "configs": groups "kmscon" and "boot" both write
  /etc/systemd/system/getty@tty2.service.d/override.conf
  (policy: strict; add to phases[].shared to allow intentionally)
```

An expected and intended effect: two groups that both run `pacman -S` both
write `/var/lib/pacman/local/…` and correctly fail. Package installations are
never independent because of the shared pacman database and belong in a serial
phase (or a single group).

## 6. Architecture

### 6.1 Language

**Go.** BuildKit's `client/llb` DSL and the gateway API are first-class Go
libraries (including MergeOp/DiffOp, progress UI, solver client). Rust would
mean rebuilding the LLB protobuf and the gateway gRPC; the existing crates are
abandoned and predate MergeOp/DiffOp.

### 6.2 Package layout

```
cmd/nomira/            CLI (cobra or similar)
internal/recipe/       YAML parsing, schema validation, types
internal/compile/      recipe → LLB (phases/groups → Exec/File/Diff/Merge)
internal/conflict/     write-set collection + disjointness check + policies
internal/engine/       buildkitd client: solve, progress, export (oci-archive)
internal/deliver/      containers-storage import, bootc switch call
internal/gates/        post-build: hook runner (validation.d/), bcvk gate
```

### 6.3 CLI

```
nomira build    [recipe.yml]   # ensure buildkitd → compile →
                               # solve (incl. conflict check) → oci-archive
nomira validate [image]        # gates: validation.d/ hooks, bcvk test-basic
nomira switch   [recipe.yml]   # build + validate + import + bootc switch
nomira graph    [recipe.yml]   # DAG as a tree/Mermaid (shows parallelism)
```

Principles: error messages only in the terms of the recipe (phase, group,
path, never LLB vertex IDs); schema errors before the first build step;
BuildKit's progress UI is passed through, so parallel groups show up as
parallel lines. buildctl, skopeo and the daemon stay invisible.

## 7. Runtime environment and delivery

- **Engine:** podman (the only runtime on the host).
- **Builder:** rootless `buildkitd`, run as a podman container or a systemd
  user service; `nomira build` starts and checks it.
- **Delivery chain:**
  1. BuildKit export as `oci-archive`
  2. `skopeo copy oci-archive:… containers-storage:localhost/<name>`
  3. `bootc switch --transport containers-storage localhost/<name>`
- **Pinning:** `settings.arch-snapshot` points the mirrorlist in the build to
  `archive.archlinux.org/repos/<date>`. The date is the one versioned place
  that determines the package set (a poor man's `flake.lock`): update = bump
  the date and rebuild, roll back the package set = git revert. `base-image`
  is pinned by digest.
- **Generations:** keep dated oci-archives; optionally a local registry (zot,
  for instance) for a digest history of any depth.

## 8. Post-build gates (outside the image)

Checks run against the finished image, never as a RUN layer:

```
validation.d/10-lint.sh      podman run --rm $IMG bootc container lint --fatal-warnings
validation.d/20-units.sh     podman run --rm $IMG systemd-analyze verify …
validation.d/30-apps.sh      podman run --rm $IMG sway --validate -c …
(built in)                   bcvk ephemeral test-basic $IMG   # boot smoke test
```

`nomira switch` runs build → gates → import → bootc switch; a failed gate
stops before the import. Gates are shell hooks and can be extended with a
single line.

## 9. Roadmap

- **v1: CLI against buildkitd** (this document): `client/llb` directly, no
  frontend image needed, `# syntax=` not involved.
- **v2: gateway frontend:** the same Go program built as a frontend image and
  pushed to any reachable OCI registry (ghcr.io, Docker Hub, a localhost
  registry; there is no privileged list and no approval process). After that
  any vanilla buildx builds recipes with
  `# syntax=ghcr.io/nomira-os/nomira:1-alpha`; testable without the syntax
  line through `buildctl build --frontend gateway.v0 --opt source=<image>`.
- **Later/open:** finer `allow` rules (per-path policy), `env:`/`workdir:` per
  group, remote cache.

## 10. Open points

- **buildkitd lifecycle:** autostart strategy (podman container vs. systemd
  user unit), version requirement (MergeOp/DiffOp ⇒ BuildKit ≥ 0.10, in
  practice a current version).
- **The Arch bootc base** remains experimental (bootcrew/mono); the project
  does not depend on it, but the system as a whole inherits its maturity.
- **Hard links and xattrs in the diff:** the collection step must record
  SELinux/security xattrs and hard-link pairs correctly as writes (test cases
  needed).
- **Hash definition for `allow-identical`:** content + mode + owner + xattrs,
  or content alone? (Proposal: everything, strict.)
