# Roadmap

What is planned for nomira across its repositories, and what still has
to be tried on a real machine. Nothing here is a promise of a date.

## Planned

### Faster package installs (frontend, base image)

The biggest cost in a rebuild today: a collector runs one command with
all items, so adding one package installs all of them again, from the
package cache but still unpacked, with every hook and a new initramfs.
Splitting packages over several collectors in phases one after the other
helps by hand. Options, after a look at how Fedora Atomic and Arkane
Linux handle it, are being weighed.

### Base images (base image, docs)

More than one base image, documented side by side: what they share
(security options, the layout bootc expects) and where they differ.
arch-base is the first.

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

### Imports (frontend)

- OCI artifacts as imports, pinned by digest
  (`oci://registry/name@sha256:…`), and Renovate updating the pins of
  URL and OCI imports.
- Files that an imported part copies, which today can only come from the
  recipe directory.
- Changing one entry of a list (one command of a group, one package)
  without restating the list, in the spirit of kustomize's strategic
  merge patch. Today a list is replaced as a whole.

### Smaller things

- `bootc status` shows the version label inherited from the Arch Linux
  image; a sensible default for `org.opencontainers.image.version`.
- `nomira recipe diff` per phase and group rather than per line.
- Multi-architecture builds; today the platform follows the build host.

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
- Booting without a unified kernel image: kernel and initramfs from
  `/usr/lib/modules`, as bootc installs them by default.
- Flatpak on the installed system: `/var/lib/flatpak` and user
  installations surviving updates.
