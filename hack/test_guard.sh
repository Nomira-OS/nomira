#!/bin/sh
# Exercises internal/compile/guard.sh against synthetic lower/upper trees.
set -eu
here="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
L="$work/l"; U="$work/u"; O="$work/o"

mkdir -p "$L/etc/app" "$L/etc/strict" "$L/var/usrlocal" "$L/usr/share/thing" "$L/etc/dir is here"
chmod 750 "$L/etc/strict"
ln -s ../var/usrlocal "$L/usr/local"
printf old > "$L/etc/app/app.conf"
ln -s ../usr/lib/os-release "$L/etc/os-release"
printf x > "$L/etc/plain"

mkdir -p "$U/etc/app" "$U/etc/new/deep" "$U/etc/strict" "$U/usr/local/bin" "$U/usr/share" "$U/etc/plain" "$U/etc/dir is here"
printf new > "$U/etc/app/app.conf"      # override of a file: fine
printf add > "$U/etc/new/deep/f"        # new directories: fine
printf s > "$U/etc/strict/rule"         # parent has another mode in L
printf b > "$U/usr/local/bin/tool"      # parent is a symlink in L
printf t > "$U/usr/share/thing"         # file where L has a directory
printf o > "$U/etc/os-release"          # file where L has a symlink
printf p > "$U/etc/plain/inner"         # directory where L has a file
printf q > "$U/etc/dir is here/a file"  # spaces, fine

sh "$here/internal/compile/guard.sh" "$L" "$U" "$O"

expect="$(cat <<'X'
L etc/os-release
T etc/plain
M 750.0.0:755.0.0 etc/strict
L usr/local
T usr/share/thing
X
)"
# uid and gid are those of the caller outside a build.
got="$(sed -E 's/([0-9]+)\.[0-9]+\.[0-9]+/\1.0.0/g' "$O/guard" | LC_ALL=C sort -k2)"
want="$(printf '%s\n' "$expect" | LC_ALL=C sort -k2)"
if [ "$got" != "$want" ]; then
  echo "guard output differs" >&2
  echo "--- want" >&2; echo "$want" >&2
  echo "--- got" >&2; echo "$got" >&2
  exit 1
fi

# An equivalent pair produces no findings.
rm -rf "$U" "$O"; mkdir -p "$U/etc/app"; printf n > "$U/etc/app/other.conf"
sh "$here/internal/compile/guard.sh" "$L" "$U" "$O"
if [ -s "$O/guard" ]; then
  echo "guard reported findings for an equivalent pair:" >&2
  cat "$O/guard" >&2
  exit 1
fi
echo "guard ok"
