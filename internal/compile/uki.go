package compile

import (
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/solver/pb"
)

// The unified kernel image (UKI) follows bootc's build pattern for sealed
// images (bootc-sealed-images(7)): move kernel and initramfs out of the
// finished root file system, build the UKI from them with
// `bootc container ukify`, which embeds the composefs digest of the root
// file system in the kernel command line, and put the UKI into
// /boot/EFI/Linux. /boot is not part of the digest.
//
// Both steps run in the image itself, so the digest is computed by the
// bootc that checks it at boot, and ukify is the image's own.

// splitScript moves kernel and initramfs to /kernel, a separate mount. bootc
// moves them with rename(2), so it moves into a directory on the root file
// system first; that directory is gone again when the step ends.
const splitScript = `set -eu
mkdir /.nomira-kernel
bootc container split-kernel-and-rootfs --rootfs / --output /.nomira-kernel
cp -a /.nomira-kernel/. /kernel/
rm -rf /.nomira-kernel
`

// ukifyScript builds the UKI of the root file system at /target from the
// kernel at /kernel/<kver>.
const ukifyScript = `set -eu
set -- /kernel/*
if [ "$#" != 1 ] || [ ! -d "$1" ]; then
    echo "nomira: expected exactly one kernel in /usr/lib/modules, found: $*" >&2
    exit 1
fi
kver="$(basename "$1")"
mkdir -p /out/EFI/Linux
bootc container ukify --rootfs /target --kernel-dir "$1" -- --output "/out/EFI/Linux/$kver.efi"
`

// WithUKI returns root with its kernel and initramfs replaced by a UKI in
// /boot/EFI/Linux. The UKI is not signed: ukify runs with an empty
// /etc/systemd/ukify.conf, whatever the image ships there.
func WithUKI(root llb.State) llb.State {
	split := root.Run(
		llb.Args([]string{"/bin/sh", "-c", splitScript}),
		llb.Dir("/"),
		llb.Network(pb.NetMode_NONE),
		llb.AddMount("/kernel", llb.Scratch()),
		llb.WithCustomName("[uki] move kernel and initramfs out of the image"),
	)
	system := split.Root()
	kernel := split.GetMount("/kernel")

	noConfig := llb.Scratch().File(llb.Mkfile("/ukify.conf", 0o644, nil),
		llb.WithCustomName("[uki] empty ukify.conf"))
	uki := system.Run(
		llb.Args([]string{"/bin/sh", "-c", ukifyScript}),
		llb.Dir("/"),
		llb.Network(pb.NetMode_NONE),
		llb.AddMount("/target", system, llb.Readonly),
		llb.AddMount("/kernel", kernel, llb.Readonly),
		llb.AddMount("/etc/systemd/ukify.conf", noConfig, llb.SourcePath("/ukify.conf"), llb.Readonly),
		llb.AddMount("/out", llb.Scratch()),
		llb.WithCustomName("[uki] build the unified kernel image"),
	).GetMount("/out")

	return system.File(llb.Copy(uki, "/", "/boot/", &llb.CopyInfo{
		CopyDirContentsOnly: true,
		CreateDestPath:      true,
	}), llb.WithCustomName("[uki] add the unified kernel image to /boot"))
}
