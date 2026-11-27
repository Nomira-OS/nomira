# Roadmap

What is planned for nomira across its repositories, and what still has
to be tried on a real machine. Nothing here is a promise of a date.

## Planned

### Includes from outside the recipe (frontend)

Groups that many recipes need, network setup or a desktop for instance,
should be shareable without copying them around. `include:` should
therefore also accept:

- an HTTPS URL, pinned by its sha256;
- an OCI artifact, pinned by digest (`oci://registry/name@sha256:…`).

Open questions: how files that such an include copies are fetched (the
include's own context next to the recipe's), how Renovate updates the
pins, and whether included groups may bring prepare stages.

### Secure Boot (frontend, CLI, base image)

- Sign the unified kernel image and systemd-boot in the recipe, with
  the keys passed as BuildKit secrets (`--mount=type=secret`, already
  supported; `nomira build --secret`).
- Passphrase-protected keys: the CLI asks for the passphrase before the
  build and passes the decrypted key from memory, since nothing can
  prompt inside BuildKit.
- A signed systemd-boot next to the unsigned one
  (`/usr/lib/systemd/boot/efi/systemd-bootx64.efi.signed`), so that
  `systemd-boot-update.service` installs the signed one.
- Key enrollment: document sbctl, and bootc's automatic enrollment from
  `/usr/lib/bootc/install/secureboot-keys/` in the recipe.

### Image version (frontend or recipes)

`bootc status` shows the version label of the image, today the one
inherited from the Arch Linux image. Recipes can set
`org.opencontainers.image.version` in `settings.labels`; a sensible
default is still open.

### Overrides and layered recipes (frontend)

Today an include brings a group, a phase or a list of phases, and the
including file cannot change anything in it. Wanted, in the spirit of
kustomize:

- **Top-level imports:** a recipe imports other recipes (a shared
  base recipe, a machine-specific one on top). Their contents are merged
  by key, the last file wins: settings key by key, prepare stages and
  groups by name, phases by name (keeping the order of the first file
  that defines a phase; new phases are placed with `before:` or
  `after:`).
- **Overrides of single values:** change one setting, one group's
  package list or one phase's env without restating the rest, and remove
  something (a group, a package) explicitly rather than by omission.
- Lists stay lists where order matters. kustomize's strategic merge
  patch merges lists of named items by their `name`, which fits phases
  as they are; turning phases into a mapping keyed by name (say under
  `build:`) would make that merge simpler to write, but YAML gives a
  mapping no order, so the order would then need its own key. To be
  decided together with the merge rules.
- `nomira graph` and `inspect` show the merged recipe and where each
  value came from.

### Collectors that build on themselves (frontend)

A collector runs one command with all items, so adding one package
installs all of them again: from the package cache, but still unpacked,
with all hooks, a new initramfs included. Splitting the packages over
several collector phases (things that rarely change first) avoids that
by hand. Better: the collector runs once per group, in a fixed order,
each run on top of the previous one, so a new item only reruns the
groups after it. Open: the order (by group name, or by phase), and
removals, which still rebuild from the first changed group.

## To be tried on a real machine

- `bootc upgrade` taking new images from the directory `nomira build`
  publishes to (composefs backend, `oci` transport).
- `bootc install` with `--target-transport oci --target-imgref`.
- Booting the unified kernel image: root partition found by its GPT type,
  fs-verity enforced, `bootctl list` showing the UKI.
- `--block-setup tpm2-luks` with a UKI.
- The first user from `systemd-homed-firstboot` ending up in `wheel`.
- Soft reboots between images that share kernel and initramfs: after
  installing a package that adds nothing to the initramfs (nano, say),
  the pacman hook leaves the initramfs alone, and where it does rebuild
  it from the same inputs the result must have the same checksum, so
  that `bootc status --verbose` shows the new image as soft-rebootable.
- The initramfs pacman hook of arch-base: triggered by firmware, kernel
  and dracut module packages exactly like Arch's own, and nothing else.
- Builds as a normal user (podman-managed buildkitd) on a development
  machine.
