# Roadmap

What is planned for nomira across its repositories, and what still has
to be tried on a real machine. Nothing here is a promise of a date.

## Planned

### Imports from OCI artifacts (frontend)

Imports by URL work (see the README). Still open: an OCI artifact,
pinned by digest (`oci://registry/name@sha256:…`), and how Renovate
updates the pins of both. Open as well: files that such an import
copies, which today can only come from the recipe directory (the
import's own context next to the recipe's).

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

### More on imports (frontend, CLI)

Imports merge as JSON Merge Patch (see the README). Still open:

- Changing one entry of a list (one command of a group, one package)
  without restating the list, in the spirit of kustomize's strategic
  merge patch. Today a list is replaced as a whole.
- `nomira recipe diff` that shows changes per phase and group rather
  than as lines.

### Collectors that build on themselves (frontend)

A collector runs one command with all items, so adding one package
installs all of them again: from the package cache, but still unpacked,
with all hooks, a new initramfs included. Splitting the packages over
several collectors in phases one after the other (things that rarely
change first) avoids that by hand. Better: the collector runs once per
group, in a fixed order, each run on top of the previous one, so a new
item only reruns the groups after it. Open: the order (by group name, or by phase), and
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
