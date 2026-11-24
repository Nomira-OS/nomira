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

### Fewer initramfs builds (recipes)

The initramfs stage runs dracut again whenever its phase changes, even
when the result is the same bytes. If that turns out too slow, skip
dracut when none of its inputs changed: keep the last image and the
checksums of the files it was built from in a cache mount. The hard part
is knowing dracut's inputs, new firmware for instance, without running it.

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
  the rebuilt initramfs must have the same checksum, so that
  `bootc status --verbose` shows the new image as soft-rebootable.
- Builds as a normal user (podman-managed buildkitd) on a development
  machine.
