# Roadmap

What is planned for nomira across its repositories, and what still has
to be tried on a real machine. Nothing here is a promise of a date.

## Planned

### Faster package installs (frontend, base image)

The biggest cost in a rebuild today: a collector runs one command with
all items, so adding one package installs all of them again, from the
package cache but still unpacked, with every hook and a new initramfs,
and the result is one large new layer to diff, compress and download.
For comparison, Arkane Linux reinstalls everything with pacstrap on
every build; Fedora's rpm-ostree keeps unpacked packages in a cache and
assembles the tree from it, and chunks the image into layers per package
group.

The plan, in steps that each stand on their own:

1. **Measure** where a collector run spends its time: download,
   signature checks, unpacking with install scripts, hooks, the
   initramfs (dracut), and diff and export of the layer. Without
   changing the base image: either a measurement recipe that splits the
   collector into phases (`pacman -Sw`, `pacman -S --dbonly`, with hooks
   masked, with all hooks) so that the build output times each part,
   or `podman run` on the base image with a measuring script mounted
   in. The export shows in the build output of the full recipe.
2. **Initramfs only when its inputs change**: done in arch-base
   (`/usr/lib/nomira/initramfs`, `recipes/initramfs.yml`); to be tried
   in a real build.
3. **Collectors that fan out** (frontend, generic). A collector with
   `resolve`, `each` and `then`, without a tool image of its own:
   `resolve` runs on the state before the phase and prints the files to
   fetch (name, URL, sha256); BuildKit downloads them, cached by URL and
   checksum; `each` unpacks a bucket of them on the unchanged base image
   into a layer of its own, so its cache depends only on the base image
   and the packages; the frontend merges the layers, with the conflict
   check between buckets; `then` finishes on the merged system. The
   frontend learns no package manager; the pacman side lives in
   arch-base's `recipes/packages.yml`.

   For `then`, `pacman -U --dbonly` is not enough: tried, it writes the
   database but runs neither hooks nor install scripts. What works,
   tried with cups and its dependencies against a normal install: an
   ordinary `pacman -U --overwrite '*'` with `NoExtract = *` followed
   by `!<target>` for every path target of the installed hooks. It is a
   real transaction, so database, install scripts and hooks are
   pacman's own (the same six hooks as a normal install, no file
   missing); only the files hooks trigger on are written again. libalpm
   skips path triggers for files under NoExtract (hook.c), hence the
   negations. Modules and firmware stay excluded, so they are not
   written twice; `then` runs `depmod` and the initramfs script of step
   2 itself. `each` has to honour the image's own NoExtract lines
   (documentation, man pages, locales), as pacman would.

Upstream, a feature request to pacman: running hooks and install
scripts for files that are already in place, for example `--dbonly`
with an option to keep hooks and scripts, or path triggers that also
match NoExtract files. It would make the negation list unnecessary.

**Discuss before building step 3**, once the prototype has numbers:
file capabilities through `bsdtar --xattrs`,
what an exec per bucket
costs in BuildKit, and how many layers bootc and registries handle well
(Fedora stays at 64 or fewer).

### Configuration defaults in /etc (base image, CLI, docs)

bootc merges `/etc` three ways on every update: a file the machine has
not changed follows the image, a changed one (content, owner, mode or
xattrs) stays as the machine has it and no longer receives updates.
The image's `/etc` is therefore already the default configuration.
Still to do:

- **Document where defaults go:** a vendor directory in `/usr` with
  drop-ins in `/etc` where the program supports it (systemd, sysctl,
  udev, NetworkManager's `/usr/lib/NetworkManager/conf.d`, polkit, …),
  following the [UAPI configuration files
  specification](https://uapi-group.org/specifications/specs/configuration_files_specification/);
  otherwise `/etc` in the image. Not `/usr/etc`: bootc reserves it
  (`bootc container lint` checks).
- **Choose how `/etc` is mounted** (base image).
  `/usr/lib/composefs/setup-root-conf.toml` offers `[etc] mount =`
  `bind` (default, three-way merge), `overlay` (the image's `/etc` as
  the lower layer, the machine's changes as the upper one: the SUSE-like
  split of defaults and local changes, visible in one directory) and
  `transient` (all of `/etc` from the image on every boot). Try
  `overlay` on a machine; perhaps offer the modes as options.
- **Show local changes:** a command such as `nomira etc diff` that lists
  what the machine changed against the image's `/etc`, and so which
  files no longer follow the image.
- Test the three-way merge with the composefs backend on hardware.

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
