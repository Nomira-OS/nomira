#!/bin/sh
# nomira independence guard. A group without run is built on an empty
# state (U) and merged onto the state before it (L). This script reports
# every place where that merge would not give the same tree as running the
# group's copies directly on L:
#   L <path>                    L has a symbolic link at a path U writes
#   T <path>                    type clash: a directory in U where L has a
#                               file, or a file or symlink in U where L has
#                               a directory
#   M <lower>:<upper> <path>    a directory that exists in both with
#                               different mode:uid:gid
# No output means the two ways are equivalent; any output makes the
# frontend build the group on L instead. The token is space-free, so paths
# may contain spaces. Needs only busybox tools.
set -eu
L="${1:-/nomira/l}"; U="${2:-/nomira/u}"; O="${3:-/nomira/out}"
mkdir -p "$O"
( cd "$U" && find . -mindepth 1 ) | LC_ALL=C sort > "$O/.entries"
: > "$O/guard"
skip=""
while IFS= read -r p; do
  f="${p#./}"
  # Below a clash the lower path may lead through a symlink; do not follow.
  if [ -n "$skip" ]; then
    case "$f" in "$skip"/*) continue ;; esac
  fi
  if [ -d "$U/$f" ] && [ ! -L "$U/$f" ]; then
    if [ -L "$L/$f" ]; then
      printf 'L %s\n' "$f" >> "$O/guard"; skip="$f"
    elif [ -d "$L/$f" ]; then
      um="$(stat -c '%a.%u.%g' "$U/$f")"
      lm="$(stat -c '%a.%u.%g' "$L/$f")"
      [ "$um" = "$lm" ] || printf 'M %s:%s %s\n' "$lm" "$um" "$f" >> "$O/guard"
    elif [ -e "$L/$f" ]; then
      printf 'T %s\n' "$f" >> "$O/guard"; skip="$f"
    fi
  elif [ -L "$L/$f" ]; then
    printf 'L %s\n' "$f" >> "$O/guard"
  elif [ -d "$L/$f" ]; then
    printf 'T %s\n' "$f" >> "$O/guard"
  fi
done < "$O/.entries"
rm -f "$O/.entries"
