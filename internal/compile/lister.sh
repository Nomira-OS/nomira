#!/bin/sh
# nomira write-set lister, pass 1. Lists what a group changes, without
# reading any file:
#   W <path>     file or symlink the group writes
#   D <path>     deleted file/symlink
#   D <path>/    deleted directory (prefix semantics)
#
# Three trees, all read-only:
#   D  the group's layer: exactly what the merge will lay on top. Every
#      file and symlink in it is a write, also one whose content equals
#      the one below: the layer would still cover what a sibling group
#      writes to that path.
#   L  the tree before the group, U the tree after it. They are walked to
#      find what the group deleted; a layer mounted on its own does not
#      show deletions. Both are absent for a group that was built on an
#      empty tree and cannot delete anything.
#
# Pass 2 (hash.sh) hashes only the paths that more than one group writes.
# Only regular files and symlinks count, so whatever a snapshotter may
# leave in a layer to mark a deletion is ignored. Paths may contain
# spaces; paths containing newlines are a known limitation of the line
# protocol. One find per tree and no process per file: busybox find,
# sort, comm and sed are all it needs.
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
  LC_ALL=C comm -23 "$T/lf" "$T/uf" | sed 's#^\./#D #' >> "$O/writes"
  LC_ALL=C comm -23 "$T/ld" "$T/ud" | sed 's#^\./\(.*\)$#D \1/#' >> "$O/writes"
fi
( cd "$D" && find . -mindepth 1 \( -type f -o -type l \) ) | LC_ALL=C sort | sed 's#^\./#W #' >> "$O/writes"
