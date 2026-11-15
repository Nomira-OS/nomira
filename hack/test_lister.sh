#!/bin/sh
# Exercises internal/compile/lister.sh against synthetic lower/upper trees:
# add, content change, metadata-only change, symlink change, file delete,
# directory delete, unchanged file, path with spaces.
set -eu
here="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
L="$work/l"; U="$work/u"; O="$work/o"; T="$work/t"

mkdir -p "$L/etc/app" "$L/etc/gone" "$L/opt/same dir"
printf old > "$L/etc/app/changed.conf"
printf keep > "$L/etc/app/same.conf"
printf meta > "$L/etc/app/meta.conf"; chmod 644 "$L/etc/app/meta.conf"
printf bye > "$L/etc/app/deleted.conf"
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

sh "$here/internal/compile/lister.sh" "$L" "$U" "$O" "$T"

got="$(sed -E 's/^W [a-f0-9]{64}:[0-9]+:[0-9]+:[0-9]+ /W /' "$O/writes" | sort)"
want="$(printf '%s\n' \
  'D etc/app/deleted.conf' \
  'D etc/gone/' \
  'D etc/gone/inner.conf' \
  'W etc/app/added.conf' \
  'W etc/app/changed.conf' \
  'W etc/app/link' \
  'W etc/app/meta.conf' \
  'W opt/same dir/spaced file.txt' | sort)"

if [ "$got" = "$want" ]; then
  echo "lister.sh: ok"
else
  echo "lister.sh: MISMATCH"; echo "--- got"; echo "$got"; echo "--- want"; echo "$want"; exit 1
fi
