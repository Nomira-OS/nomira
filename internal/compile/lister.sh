#!/bin/sh
# nomira write-set lister: compares lower (S) and upper (R) trees and emits
#   W <sha256:mode:uid:gid> <path>   added or changed file/symlink
#   D <path>                         deleted file/symlink
#   D <path>/                        deleted directory (prefix semantics)
# Token is space-free (symlinks hash their target string), so paths may
# contain spaces. Paths containing newlines are a known limitation of the
# line protocol. Needs only busybox tools (find, sort, comm, stat,
# sha256sum, readlink, cmp-free).
set -eu
L="${1:-/nomira/l}"; U="${2:-/nomira/u}"; O="${3:-/nomira/out}"; T="${4:-/nomira/tmp}"
mkdir -p "$O" "$T"
( cd "$U" && find . -mindepth 1 \( -type f -o -type l \) ) | sort > "$T/uf"
( cd "$L" && find . -mindepth 1 \( -type f -o -type l \) ) | sort > "$T/lf"
( cd "$U" && find . -mindepth 1 -type d ) | sort > "$T/ud"
( cd "$L" && find . -mindepth 1 -type d ) | sort > "$T/ld"
: > "$O/writes"
hashof() {
  if [ -L "$1" ]; then
    printf 'link:%s' "$(readlink "$1")" | sha256sum | cut -d' ' -f1
  else
    sha256sum "$1" | cut -d' ' -f1
  fi
}
comm -23 "$T/lf" "$T/uf" | while IFS= read -r p; do
  printf 'D %s\n' "${p#./}" >> "$O/writes"
done
comm -23 "$T/ld" "$T/ud" | while IFS= read -r p; do
  printf 'D %s/\n' "${p#./}" >> "$O/writes"
done
while IFS= read -r p; do
  f="${p#./}"
  uh="$(hashof "$U/$p")"
  um="$(stat -c '%a:%u:%g' "$U/$p")"
  if [ -e "$L/$p" ] || [ -L "$L/$p" ]; then
    lh="$(hashof "$L/$p")"
    lm="$(stat -c '%a:%u:%g' "$L/$p")"
    if [ "$uh" = "$lh" ] && [ "$um" = "$lm" ]; then continue; fi
  fi
  printf 'W %s:%s %s\n' "$uh" "$um" "$f" >> "$O/writes"
done < "$T/uf"
