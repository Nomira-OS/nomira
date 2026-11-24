#!/bin/sh
# End-to-end tests: build small recipes with a real BuildKit and check the
# result. They cover what unit tests cannot: how BuildKit diffs, merges and
# caches what the frontend asks for.
#
# Needs buildctl and a running buildkitd (BUILDKIT_HOST), and:
#   FRONTEND  image of the frontend under test (required)
#   BASE      base image with /bin/sh      (default: alpine 3.22)
#   BASE2     a different base image       (default: alpine 3.21)
#   TOOLBOX   image with busybox tools     (default: the frontend's own)
#   KEEP=1    keep the work directory
#
# The CI workflow e2e.yml provides all of that. Run a single case with
#   sh hack/e2e.sh conflict_write
set -eu

: "${FRONTEND:?set FRONTEND to the frontend image under test}"
BASE="${BASE:-docker.io/library/alpine:3.22}"
BASE2="${BASE2:-docker.io/library/alpine:3.21}"
TOOLBOX="${TOOLBOX:-}"

work="$(mktemp -d)"
[ -n "${KEEP:-}" ] || trap 'rm -rf "$work"' EXIT
failed=0
count=0

# recipe <case> : writes $work/<case>/recipe.yml from stdin, with a settings
# block in front. The body starts at the top-level key after settings.
recipe() {
  d="$work/$1"
  mkdir -p "$d/files"
  {
    printf 'settings:\n  name: e2e\n  base: %s\n' "${RECIPE_BASE:-$BASE}"
    [ -z "$TOOLBOX" ] || printf '  toolbox: %s\n' "$TOOLBOX"
    cat
  } > "$d/recipe.yml"
}

# build <case> : builds $work/<case>, log in log, result tree in out/.
build() {
  d="$work/$1"
  rm -rf "$d/out"
  buildctl build \
    --frontend gateway.v0 --opt source="$FRONTEND" \
    --local context="$d" --local dockerfile="$d" \
    --opt filename=recipe.yml \
    ${BUILD_SECRET:+--secret "$BUILD_SECRET"} \
    --output type=local,dest="$d/out" \
    --progress=plain > "$d/log" 2>&1
}

fail() {
  echo "    FAIL: $*"
  case_failed=1
}

must_build() {
  build "$1" || { fail "build failed"; sed 's/^/      | /' "$work/$1/log" | tail -n 25; return 1; }
}

must_fail() { # <case> <pattern in the log>
  if build "$1"; then
    fail "build succeeded, expected an error matching: $2"
    return 1
  fi
  grep -q -- "$2" "$work/$1/log" || {
    fail "error does not match: $2"
    sed 's/^/      | /' "$work/$1/log" | tail -n 15
  }
}

has() { # <case> <path> [content]
  f="$work/$1/out/$2"
  [ -e "$f" ] || [ -L "$f" ] || { fail "missing in result: /$2"; return 0; }
  [ $# -lt 3 ] || [ "$(cat "$f")" = "$3" ] || fail "/$2 contains '$(cat "$f")', expected '$3'"
}

hasnot() { [ ! -e "$work/$1/out/$2" ] || fail "unexpected in result: /$2"; }

mode() { # <case> <path> <octal mode>
  got="$(stat -c '%a' "$work/$1/out/$2")"
  [ "$got" = "$3" ] || fail "/$2 has mode $got, expected $3"
}

logged() { grep -q -- "$2" "$work/$1/log" || fail "log lacks: $2"; }
notlogged() { ! grep -q -- "$2" "$work/$1/log" || fail "log contains: $2"; }

# cached <case> <step name> : the step was taken from the cache.
cached() {
  grep -F -A1 -- "$2" "$work/$1/log" | grep -q 'CACHED' || fail "not cached: $2"
}
notcached() {
  ! grep -F -A1 -- "$2" "$work/$1/log" | grep -q 'CACHED' || fail "cached, expected a rebuild: $2"
}

# ---- cases -------------------------------------------------------------

case_tmpfs_mount() {
  recipe tmpfs_mount <<'EOF'
phases:
  - name: content
    groups:
      a: ["run --mount=type=tmpfs,target=/scratch echo x > /scratch/f && cp /scratch/f /etc/from-tmpfs"]
EOF
  must_build tmpfs_mount || return 0
  has tmpfs_mount etc/from-tmpfs x
  hasnot tmpfs_mount scratch/f
}

case_context_excludes_output() {
  recipe context_excludes_output <<'EOF'
phases:
  - name: content
    groups:
      a: ["copy .nomira/big /big"]
EOF
  mkdir -p "$work/context_excludes_output/.nomira"
  echo big > "$work/context_excludes_output/.nomira/big"
  must_fail context_excludes_output "big"
}

# Bind mounts from earlier phases, as a recipe uses them for a unified
# kernel image, against a stand-in for bootc: its "UKI" holds the kernel,
# the initramfs and the file list of the root file system it was given.
case_uki() {
  recipe uki <<'EOF'
phases:
  - name: system
    groups:
      fake:
        - copy --chmod=0755 files/bootc /usr/bin/bootc
        - run mkdir -p /usr/lib/modules/7.0 && echo kernel > /usr/lib/modules/7.0/vmlinuz && echo initrd > /usr/lib/modules/7.0/initramfs.img && echo dep > /usr/lib/modules/7.0/modules.dep
  - name: kernel
    groups:
      out: ["run rm /usr/lib/modules/*/vmlinuz /usr/lib/modules/*/initramfs.img"]
  - name: uki
    groups:
      uki:
        - run --mount=type=bind,from=kernel,target=/target --mount=type=bind,from=system,source=/usr/lib/modules,target=/kernels mkdir -p /boot/EFI/Linux && for d in /kernels/*; do bootc container ukify --rootfs /target --kernel-dir "$d" -- --output "/boot/EFI/Linux/$(basename "$d").efi"; done
EOF
  cat > "$work/uki/files/bootc" <<'EOF'
#!/bin/sh
set -eu
[ "$1 $2" = "container ukify" ] || exit 2
{ cat "$6/vmlinuz" "$6/initramfs.img"; find "$4/usr/lib/modules" | sed "s|^$4||" | sort; } > "$9"
EOF
  must_build uki || return 0
  has uki usr/lib/modules/7.0/modules.dep dep
  hasnot uki usr/lib/modules/7.0/vmlinuz
  hasnot uki target
  hasnot uki kernels
  efi="$work/uki/out/boot/EFI/Linux/7.0.efi"
  [ -f "$efi" ] || { fail "no UKI"; return 0; }
  expected="$(printf 'kernel\ninitrd\n/usr/lib/modules\n/usr/lib/modules/7.0\n/usr/lib/modules/7.0/modules.dep')"
  [ "$(cat "$efi")" = "$expected" ] || fail "UKI content: $(cat "$efi")"
}

case_secret_mount() {
  recipe secret_mount <<'EOF'
phases:
  - name: content
    groups:
      a: ["run --mount=type=secret,id=tok,required=true wc -c < /run/secrets/tok | tr -d ' ' > /etc/tok-size"]
EOF
  printf abc > "$work/secret_mount/tok"
  BUILD_SECRET="id=tok,src=$work/secret_mount/tok" must_build secret_mount || return 0
  has secret_mount etc/tok-size 3
  hasnot secret_mount run/secrets/tok
  # A required secret that is not given fails the build. (A cached step
  # would not notice: the secret is not part of the cache key.)
  recipe secret_missing <<'EOF'
phases:
  - name: content
    groups:
      a: ["run --mount=type=secret,id=tok,required=true echo missing-$nonce > /etc/x"]
EOF
  sed -i "s/\$nonce/$(date +%s%N)/" "$work/secret_missing/recipe.yml"
  must_fail secret_missing "tok"
}

# A prepare stage that starts from a phase: built from the system after
# that phase, cached as long as that phase is, never part of the image.
case_stage_from_phase() {
  recipe stage_from_phase <<'EOF'
prepare:
  derived:
    from: packages
    commands:
      - run mkdir /out && cat /etc/pkgs > /out/derived
phases:
  - name: packages
    groups:
      p: ["run echo pkgs-NONCE > /etc/pkgs"]
  - name: config
    groups:
      c: ["copy files/conf /etc/conf"]
  - name: use
    groups:
      u: ["run --mount=type=bind,from=derived,source=/out,target=/d cp /d/derived /etc/derived"]
EOF
  nonce="$(date +%s%N)"
  sed -i "s/NONCE/$nonce/" "$work/stage_from_phase/recipe.yml"
  echo one > "$work/stage_from_phase/files/conf"
  must_build stage_from_phase || return 0
  has stage_from_phase etc/derived "pkgs-$nonce"
  hasnot stage_from_phase out
  # A change after the phase leaves the stage cached.
  echo two > "$work/stage_from_phase/files/conf"
  must_build stage_from_phase || return 0
  has stage_from_phase etc/conf two
  cached stage_from_phase '[prepare derived] run mkdir /out'
}

case_groups_merge() {
  recipe groups_merge <<'EOF'
phases:
  - name: content
    groups:
      a: ["run echo a > /etc/a"]
      b: ["run echo b > /etc/b"]
EOF
  must_build groups_merge || return 0
  has groups_merge etc/a a
  has groups_merge etc/b b
  has groups_merge bin/sh
}

case_conflict_write() {
  recipe conflict_write <<'EOF'
phases:
  - name: content
    groups:
      a: ["run echo a > /etc/a"]
      b: ["run echo b > /etc/a"]
EOF
  must_fail conflict_write 'groups "a" and "b": /etc/a (both write)'
  logged conflict_write 'declared-independent groups are not disjoint (policy: strict)'
}

case_conflict_delete() {
  # One group deletes what the other writes. A layer mounted on its own
  # does not show deletions; they are found by walking the trees.
  recipe conflict_delete <<'EOF'
phases:
  - name: content
    groups:
      a: ["run rm /etc/hostname"]
      b: ["run echo x > /etc/hostname"]
EOF
  must_fail conflict_delete '/etc/hostname'
  logged conflict_delete 'groups "a" and "b"'
}

case_conflict_delete_dir() {
  # Deleting a directory collides with a write below it.
  recipe conflict_delete_dir <<'EOF'
phases:
  - name: content
    groups:
      a: ["run rm -rf /etc/apk"]
      b: ["run echo x > /etc/apk/new"]
EOF
  must_fail conflict_delete_dir '/etc/apk'
}

case_conflict_isolated() {
  # A group without run against a group with run.
  recipe conflict_isolated <<'EOF'
phases:
  - name: content
    groups:
      a: ["copy files/a /etc/a"]
      b: ["run echo b > /etc/a"]
EOF
  echo a > "$work/conflict_isolated/files/a"
  must_fail conflict_isolated 'groups "a" and "b": /etc/a (both write)'
}

case_delete_alone_is_fine() {
  recipe delete_alone <<'EOF'
phases:
  - name: content
    groups:
      a: ["run rm /etc/hostname"]
      b: ["run echo b > /etc/b"]
EOF
  must_build delete_alone || return 0
  hasnot delete_alone etc/hostname
  has delete_alone etc/b b
}

case_allow_identical() {
  recipe allow_identical <<'EOF'
phases:
  - name: content
    validation: allow-identical
    groups:
      a: ["run echo same > /etc/same"]
      b: ["run echo same > /etc/same"]
EOF
  must_build allow_identical || return 0
  has allow_identical etc/same same
}

case_shared() {
  recipe shared <<'EOF'
phases:
  - name: content
    shared: [/etc/shared/]
    groups:
      a: ["run mkdir -p /etc/shared && echo a > /etc/shared/f"]
      b: ["run mkdir -p /etc/shared && echo b > /etc/shared/f"]
EOF
  must_build shared || return 0
  has shared etc/shared/f
}

case_rewrite_counts_as_write() {
  # A file rewritten with the same content is in the group's layer and
  # would cover a sibling's change, so it is a conflict under strict.
  recipe rewrite <<'EOF'
phases:
  - name: content
    groups:
      a: ["run cp /etc/hostname /tmp/h && cat /tmp/h > /etc/hostname && rm /tmp/h"]
      b: ["run echo changed > /etc/hostname"]
EOF
  must_fail rewrite '/etc/hostname'
}

case_serial_phases() {
  # A later phase sees the result of an earlier one.
  recipe serial <<'EOF'
phases:
  - name: one
    groups:
      a: ["run echo 1 > /etc/n"]
  - name: two
    groups:
      a: ["run echo $(( $(cat /etc/n) + 1 )) > /etc/n"]
      b: ["run cp /etc/n /etc/seen"]
EOF
  must_build serial || return 0
  has serial etc/n 2
  has serial etc/seen 1
}

case_collectors() {
  recipe collectors <<'EOF'
phases:
  - name: words
    collect: run for w in ${items}; do echo "$w" >> /etc/words; done
  - name: features
    groups:
      one:
        words: [pear, apple]
        later: [x]
        commands: ["copy files/f /etc/one"]
      two:
        words: [apple, fig]
      three: ["run test -f /etc/words && echo ok > /etc/three"]
  - name: later
    collect: run cat /etc/one > /etc/later && echo ${items} >> /etc/later
  - name: nobody
    collect: run echo ${items} > /etc/nobody
EOF
  echo f > "$work/collectors/files/f"
  must_build collectors || return 0
  has collectors etc/words "apple
fig
pear"
  has collectors etc/three ok
  has collectors etc/later "f
x"
  hasnot collectors etc/nobody
}

case_long_form_and_includes() {
  recipe long_form <<EOF
prepare:
  shared:
    from: ${BASE}
    commands: ["run mkdir /out && echo shared > /out/s"]
phases:
  - name: features
    groups:
      feature: {include: features/feature.yml}
      other:
        prepare:
          gen:
            from: ${BASE}
            commands: ["run mkdir /out && echo other > /out/g"]
        commands:
          - copy --from=gen /out/g /etc/other
  - include: phases/last.yml
EOF
  mkdir -p "$work/long_form/features" "$work/long_form/phases"
  cat > "$work/long_form/features/feature.yml" <<EOF
expects: [/etc/alpine-release]
prepare:
  gen:
    from: ${BASE}
    commands: {include: gen.yml}
commands:
  - env GREETING=hello
  - copy --chmod=0600 files/f /etc/feature/f
  - copy --from=gen /out/g /etc/feature/generated
  - copy --from=shared /out/s /etc/feature/shared
EOF
  printf -- '- run mkdir /out && echo feature > /out/g\n' > "$work/long_form/features/gen.yml"
  printf -- 'name: last\ngroups:\n  a:\n    - env WHO=nomira\n    - run echo "hi $WHO" > /etc/last\n' > "$work/long_form/phases/last.yml"
  echo f > "$work/long_form/files/f"
  must_build long_form || return 0
  has long_form etc/feature/f f
  mode long_form etc/feature/f 600
  # Two groups each have a stage called gen; they do not mix.
  has long_form etc/feature/generated feature
  has long_form etc/other other
  has long_form etc/feature/shared shared
  has long_form etc/last "hi nomira"
}

case_expects() {
  recipe expects <<'EOF'
phases:
  - name: features
    groups:
      a:
        expects: [/etc/alpine-release, /etc/does-not-exist]
        commands: ["copy files/f /etc/f"]
EOF
  echo f > "$work/expects/files/f"
  must_fail expects 'group "a": /etc/does-not-exist'
  notlogged expects 'group "a": /etc/alpine-release'
}

case_independent_of_base() {
  # A group without run must come from the cache when only the base
  # changes; a group with run must not.
  # The nonce keeps a cache left by an earlier run of this script from
  # answering for the group with run.
  nonce="$(date +%s)-$$"
  body="phases:
  - name: features
    groups:
      copies:
        - copy files/f /etc/copied
      runs:
        - run echo $nonce > /etc/ran
"
  printf '%s' "$body" | recipe indep
  echo f > "$work/indep/files/f"
  must_build indep || return 0
  has indep etc/copied f
  mode indep etc 755
  has indep etc/alpine-release
  notlogged indep 'WARNING'

  printf '%s' "$body" | RECIPE_BASE="$BASE2" recipe indep
  must_build indep || return 0
  has indep etc/copied f
  has indep etc/ran "$nonce"
  cached indep '[features/copies] copy files/f -> /etc/copied'
  notcached indep "[features/runs] run echo $nonce > /etc/ran"
}

case_fallback_symlink() {
  # Copying below a symbolic link: the group is built on the state before
  # it, the file follows the link, and the build says so.
  recipe fb_symlink <<'EOF'
phases:
  - name: prepare-system
    groups:
      a: ["run mkdir /real && ln -s real /link"]
  - name: features
    groups:
      viaLink: ["copy files/f /link/f"]
      plain: ["copy files/f /etc/plain"]
EOF
  echo f > "$work/fb_symlink/files/f"
  must_build fb_symlink || return 0
  has fb_symlink real/f f
  [ -L "$work/fb_symlink/out/link" ] || fail "/link is no longer a symbolic link"
  has fb_symlink etc/plain f
  logged fb_symlink 'features/viaLink, WARNING: not independent of earlier state: /link is a symbolic link there'
  notlogged fb_symlink 'features/plain, WARNING'
}

case_fallback_mode() {
  # Copying into a directory with its own mode: the mode must survive.
  recipe fb_mode <<'EOF'
phases:
  - name: prepare-system
    groups:
      a: ["run mkdir -m 750 /etc/strict"]
  - name: features
    groups:
      intoStrict: ["copy files/f /etc/strict/f"]
EOF
  echo f > "$work/fb_mode/files/f"
  must_build fb_mode || return 0
  has fb_mode etc/strict/f f
  mode fb_mode etc/strict 750
  # The warning is repeated when everything comes from the cache.
  must_build fb_mode || return 0
  logged fb_mode 'features/intoStrict, WARNING: not independent of earlier state: /etc/strict has mode.uid.gid 750.0.0 there, not 755.0.0'
}

case_fallback_type() {
  # A file where the state before has a directory.
  recipe fb_type <<'EOF'
phases:
  - name: prepare-system
    groups:
      a: ["run mkdir /etc/thing && echo keep > /etc/thing/keep"]
  - name: features
    groups:
      intoDir: ["copy files/thing /etc/thing"]
EOF
  echo f > "$work/fb_type/files/thing"
  must_build fb_type || return 0
  has fb_type etc/thing/keep keep
  logged fb_type 'features/intoDir, WARNING: not independent of earlier state: /etc/thing is a file on one side and a directory on the other'
}

case_isolated_single_group() {
  # A phase with one group without run is merged, not run on the input.
  recipe single <<'EOF'
phases:
  - name: features
    groups:
      only: ["copy files/ /etc/only/"]
EOF
  echo 1 > "$work/single/files/one"
  mkdir -p "$work/single/files/sub"; echo 2 > "$work/single/files/sub/two"
  must_build single || return 0
  has single etc/only/one 1
  has single etc/only/sub/two 2
  has single etc/alpine-release
}

# ---- run ---------------------------------------------------------------

all="groups_merge conflict_write conflict_delete conflict_delete_dir conflict_isolated
delete_alone_is_fine allow_identical shared rewrite_counts_as_write serial_phases
collectors long_form_and_includes expects independent_of_base fallback_symlink
fallback_mode fallback_type isolated_single_group tmpfs_mount
context_excludes_output uki secret_mount stage_from_phase"

for c in ${*:-$all}; do
  count=$((count + 1))
  case_failed=0
  start=$(date +%s)
  echo "=== $c"
  "case_$c"
  if [ "$case_failed" = 0 ]; then
    echo "    ok ($(( $(date +%s) - start ))s)"
  else
    failed=$((failed + 1))
  fi
done

echo
if [ "$failed" = 0 ]; then
  echo "e2e: all $count cases passed"
else
  echo "e2e: $failed of $count cases failed"
  [ -z "${KEEP:-}" ] || echo "work directory: $work"
  exit 1
fi
