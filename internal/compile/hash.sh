#!/bin/sh
# nomira write-set lister, pass 2. For each path in P (one per line,
# relative) that the group's layer D holds, prints
#   H <sha256>:<mode>:<uid>:<gid> <path>
# A symlink hashes its target string, never what it points to. Only the
# paths that more than one group of a phase writes get here, usually none
# or a few, so one sha256sum and stat per path is fine.
set -eu
D="${1:-/nomira/d}"; P="${2:-/nomira/p/paths}"; O="${3:-/nomira/out}"
mkdir -p "$O"
: > "$O/hashes"
while IFS= read -r p; do
  f="$D/$p"
  if [ -L "$f" ]; then
    h="$(printf 'link:%s' "$(readlink "$f")" | sha256sum)"
  elif [ -f "$f" ]; then
    h="$(sha256sum < "$f")"
  else
    continue
  fi
  printf 'H %s:%s %s\n' "${h%% *}" "$(stat -c '%a:%u:%g' "$f")" "$p" >> "$O/hashes"
done < "$P"
