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
3. **Collectors that fan out**: in the frontend and in arch-base's
   `packages.yml`; to be measured in a real build. The design: a collector with
   `resolve`, `each` and `then`, without a tool image of its own, all on
   the state `S` where the collector stands: `resolve` prints the files
   to fetch (name, URL, sha256); BuildKit downloads them, cached by their
   checksum; `each` unpacks a bucket of them into a layer of its own, so
   its cache depends only on `S` and the bucket's packages; the frontend
   merges the layers, with the conflict check between buckets; `then`
   finishes on the merged system. The frontend learns no package
   manager; the pacman side lives in arch-base's `recipes/packages.yml`.

   For `then`, `pacman -U --dbonly` is not enough: tried, it writes the
   database but runs neither hooks nor install scripts. What works,
   tried with cups and its dependencies against a normal install: an
   ordinary `pacman -U --overwrite '*'` with `NoExtract = *` followed
   by a negation for what the hooks in effect trigger on. It is a real
   transaction, so database, install scripts and hooks are pacman's own.
   libalpm skips path triggers for files under NoExtract (hook.c), hence
   the negations. A hook without `NeedsTargets` needs one matching path,
   so its target `usr/share/fonts/*` is negated as the directory
   `usr/share/fonts/`, which every package with fonts lists; only
   `NeedsTargets` hooks get their targets as they are. No font, firmware
   file or module is written twice, and depmod and the initramfs run as
   their own hooks. In `each` no hook runs: libalpm looks for hooks below
   `--root`, so a bucket's own hooks would run in a system that holds
   only that bucket; their files stay under NoExtract during the
   transaction and are unpacked afterwards. Both tried with pacman 6.0.2
   and packages made for the test, against a normal install: the same
   hooks with the same targets, the same files. The base image leaves
   NoExtract open, so packages are whole unless a recipe says otherwise.

Buckets: a package's bucket is a function of its key alone (rendezvous
hashing over the bucket numbers), never of the other packages, their
versions or the order: adding or removing a package changes only its
own bucket, and a different number of buckets moves only the packages
that have to move. Buckets are not balanced, since balancing would make
a bucket depend on the others. `resolve` may print a fourth value per
file, the key, which otherwise is the name; arch-base gives pkgbase
(`pacman -Sp --print-format '%n %l %h %e'`), so that split packages
updated together share a bucket. Every bucket is built on the base
image on its own; the order of the merge is fixed but decides nothing,
since buckets must not overlap: the conflict check runs between buckets,
strictly (any file in two buckets is a conflict, as it is for pacman).
Bounded at 64 by default: one layer per package would mean hundreds of
layers, beyond what container storage (500), registries and pulls handle
well. Later: the number of buckets configurable, and more distribution
logics as options: xattrs in the manner of chunkah's `user.component`,
packages above a size of their own, frequency hints.

**Reusing layers without a local cache**: done. The frontend passes
caches the client imports on to every solve, and `nomira build` writes
an inline cache into every image and imports it from the last published
image and from `--cache-from` images. Tried with all local cache pruned:
unchanged buckets come from the import, only the changed bucket and
`then` run; `then` needs every package and downloads them. Still open:
the cost of the import on a large image, to measure with the rest.

`then` stays one transaction over all packages: database, install
scripts and hooks for everything, whenever any bucket changed. Running
it only for the changed packages would mean building on the previous
image, so the result would depend on the build history instead of the
recipe, or replacing pacman's own handling of database, scripts and
hooks. rpm-ostree does the same as nomira here: it caches the unpacked
packages (pkgcache) and runs the scripts over the whole tree on every
compose; it skips a compose only when nothing changed, as BuildKit's
cache does. What makes `then` cheap is writing little; to be measured.

Done as well: `then` marks the packages no item named as dependencies,
as `pacman -S` would; it gets the items for that. Packages the system
has in another version are upgraded in a transaction of their own,
since an upgrade under NoExtract would lose their files, and `each`
checks a bucket against the system as pacman would without
`--overwrite`.

Done: the conflict check as a build option (`--opt check=off`, `nomira build
--no-check`), on by default: faster builds for who accepts that groups
or buckets that overlap are merged in a fixed order without a word.

Upstream, a feature request to pacman: running hooks and install
scripts for files that are already in place, for example `--dbonly`
with an option to keep hooks and scripts, or path triggers that also
match NoExtract files. It would make the negation list unnecessary.

**To measure and discuss** now that step 3 is built (each unpacks with
pacman, so file capabilities and NoExtract are pacman's own), with
`podman run` on the base image: what `then` costs against a normal
install, what an exec per bucket costs in BuildKit, the export with
`rewrite-timestamp`, and how many layers bootc and registries handle well
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
