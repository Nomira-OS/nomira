#!/bin/sh
# nomira write-set lister. Emits what a group changes:
#   W <sha256:mode:uid:gid> <path>   file or symlink the group writes
#   D <path>                         deleted file/symlink
#   D <path>/                        deleted directory (prefix semantics)
#
# Three trees, all read-only:
#   D  the group's layer: exactly what the merge will lay on top. Every
#      file and symlink in it is a write, also one whose content equals
#      the one below: the layer would still cover what a sibling group
#      writes to that path. Only these files are hashed.
#   L  the tree before the group, U the tree after it. They are walked but
#      never read, to find what the group deleted; a layer mounted on its
#      own does not show deletions. Both are absent for a group that was
#      built on an empty tree and cannot delete anything.
#
# Only regular files and symlinks count, so whatever a snapshotter may
# leave in a layer to mark a deletion is ignored. Token is space-free
# (symlinks hash their target string), so paths may contain spaces. Paths
# containing newlines are a known limitation of the line protocol. Needs
# only busybox tools (find, sort, comm, stat, sha256sum, readlink).
set -eu
L="${1:-/nomira/l}"; U="${2:-/nomira/u}"; D="${3:-/nomira/d}"
O="${4:-/nomira/out}"; T="${5:-/nomira/tmp}"
mkdir -p "$O" "$T"
: > "$O/writes"

if [ -d "$L" ] && [ -d "$U" ]; then
  ( cd "$U" && find . -mindepth 1 \( -type f -o -type l \) ) | LC_ALL=C sort > "$T/uf"
  ( cd "$L" && find . -mindepth 1 \( -type f -o -type l \) ) | LC_ALL=C sort > "$T/lf"
  ( cd "$U" && find . -mindepth 1 -type d ) | LC_ALL=C sort > "$T/ud"
  ( cd "$L" && find . -mindepth 1 -type d ) | LC_ALL=C sort > "$T/ld"
  LC_ALL=C comm -23 "$T/lf" "$T/uf" | while IFS= read -r p; do
    printf 'D %s\n' "${p#./}" >> "$O/writes"
  done
  LC_ALL=C comm -23 "$T/ld" "$T/ud" | while IFS= read -r p; do
    printf 'D %s/\n' "${p#./}" >> "$O/writes"
  done
fi

hashof() {
  if [ -L "$1" ]; then
    printf 'link:%s' "$(readlink "$1")" | sha256sum | cut -d' ' -f1
  else
    sha256sum "$1" | cut -d' ' -f1
  fi
}
( cd "$D" && find . -mindepth 1 \( -type f -o -type l \) ) | LC_ALL=C sort > "$T/df"
while IFS= read -r p; do
  printf 'W %s:%s %s\n' "$(hashof "$D/$p")" "$(stat -c '%a:%u:%g' "$D/$p")" "${p#./}" >> "$O/writes"
done < "$T/df"
