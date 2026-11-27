#!/bin/sh
# Exercises internal/compile/lister.sh and hash.sh against synthetic trees:
# lower (L), upper (U) and the layer between them (D), as BuildKit's
# DiffOp would produce it. Covers add, content change, metadata-only
# change, symlink change, a rewritten but identical file, file delete,
# directory delete, an unchanged file, a path with spaces, and an entry in
# the layer that is neither file nor symlink.
set -eu
here="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
L="$work/l"; U="$work/u"; D="$work/d"; O="$work/o"; T="$work/t"

mkdir -p "$L/etc/app" "$L/etc/gone" "$L/opt/same dir"
printf old > "$L/etc/app/changed.conf"
printf keep > "$L/etc/app/same.conf"
printf meta > "$L/etc/app/meta.conf"; chmod 644 "$L/etc/app/meta.conf"
printf bye > "$L/etc/app/deleted.conf"
printf same > "$L/etc/app/rewritten.conf"
printf x > "$L/etc/gone/inner.conf"
printf s > "$L/opt/same dir/spaced file.txt"
ln -s old-target "$L/etc/app/link"

cp -a "$L" "$U"
rm -rf "$U/etc/gone"                       # directory delete
rm "$U/etc/app/deleted.conf"               # file delete
printf new > "$U/etc/app/changed.conf"     # content change
chmod 600 "$U/etc/app/meta.conf"           # metadata-only change
ln -sfn new-target "$U/etc/app/link"       # symlink retarget
printf add > "$U/etc/app/added.conf"       # addition
printf S2 > "$U/opt/same dir/spaced file.txt"  # change in spaced path

# The layer holds what was written, including a file that was rewritten
# with the same content, and nothing of what stayed untouched.
mkdir -p "$D/etc/app" "$D/opt/same dir"
for f in changed.conf meta.conf added.conf link rewritten.conf; do
  cp -a "$U/etc/app/$f" "$D/etc/app/$f"
done
cp -a "$U/opt/same dir/spaced file.txt" "$D/opt/same dir/"
mkfifo "$D/etc/app/deleted.conf"           # stands in for a deletion marker

check() { # name got want
  if [ "$2" != "$3" ]; then
    echo "$1: MISMATCH"; echo "--- got"; echo "$2"; echo "--- want"; echo "$3"; exit 1
  fi
}

sh "$here/internal/compile/lister.sh" "$L" "$U" "$D" "$O" "$T"
check lister.sh "$(sort "$O/writes")" "$(printf '%s\n' \
  'D etc/app/deleted.conf' \
  'D etc/gone/' \
  'D etc/gone/inner.conf' \
  'W etc/app/added.conf' \
  'W etc/app/changed.conf' \
  'W etc/app/link' \
  'W etc/app/meta.conf' \
  'W etc/app/rewritten.conf' \
  'W opt/same dir/spaced file.txt' | sort)"

# A group built on an empty tree: no lower, no upper, every file a write.
rm -rf "$O" "$T"
sh "$here/internal/compile/lister.sh" "$work/none-l" "$work/none-u" "$D" "$O" "$T"
check "lister.sh (empty lower)" "$(sort "$O/writes")" "$(printf '%s\n' \
  'W etc/app/added.conf' \
  'W etc/app/changed.conf' \
  'W etc/app/link' \
  'W etc/app/meta.conf' \
  'W etc/app/rewritten.conf' \
  'W opt/same dir/spaced file.txt' | sort)"

# Pass 2: only the listed paths are hashed; a symlink by its target.
printf '%s\n' etc/app/meta.conf etc/app/link 'opt/same dir/spaced file.txt' etc/app/deleted.conf > "$work/paths"
sh "$here/internal/compile/hash.sh" "$D" "$work/paths" "$O"
link="$(printf 'link:new-target' | sha256sum | cut -d' ' -f1)"
meta="$(printf meta | sha256sum | cut -d' ' -f1)"
grep -qx "H $meta:600:[0-9]*:[0-9]* etc/app/meta.conf" "$O/hashes" || { echo "hash.sh: meta.conf"; cat "$O/hashes"; exit 1; }
grep -q "^H $link:[0-9:]* etc/app/link\$" "$O/hashes" || { echo "hash.sh: link"; cat "$O/hashes"; exit 1; }
grep -q ' opt/same dir/spaced file.txt$' "$O/hashes" || { echo "hash.sh: spaces"; cat "$O/hashes"; exit 1; }
[ "$(wc -l < "$O/hashes")" -eq 3 ] || { echo "hash.sh: the fifo was hashed"; cat "$O/hashes"; exit 1; }
echo "lister.sh, hash.sh: ok"
