#!/bin/sh
# End-to-end tests: build small recipes with a real BuildKit and check the
# result. They cover what unit tests cannot: how BuildKit diffs, merges and
# caches what the frontend asks for.
#
# Needs buildctl and a running buildkitd (BUILDKIT_HOST), and:
#   FRONTEND  image of the frontend under test (required)
#   BASE      base image with /bin/sh      (default: alpine 3.22)
#   BASE2     a different base image       (default: alpine 3.21)
#   TOOLBOX   image with busybox tools     (default: the frontend's default)
#   HTTP_DIR, HTTP_URL  a directory and the http URL that serves it, for
#             imports by URL (case url_import; skipped without them)
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
    printf 'settings:\n  base: %s\n' "${RECIPE_BASE:-$BASE}"
    [ -z "${RECIPE_SETTINGS:-}" ] || printf '  %s\n' "$RECIPE_SETTINGS"
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
    --opt filename=recipe.yml ${TOOLBOX:+--opt toolbox="$TOOLBOX"} \
    ${BUILD_SECRET:+--secret "$BUILD_SECRET"} ${MODE:+--opt mode="$MODE"} \
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

# oci <case> [exporter options] : builds $work/<case> as an OCI image into
# img/ and sets $manifest and $config to its blobs. Plain tools only: the
# CI runs this script in the BuildKit image. The first digest in the index
# is the manifest's, and in the manifest the config's comes first.
oci() {
  d="$work/$1"
  rm -rf "$d/img" "$d/img.tar"
  buildctl build \
    --frontend gateway.v0 --opt source="$FRONTEND" \
    --local context="$d" --local dockerfile="$d" --opt filename=recipe.yml \
    ${TOOLBOX:+--opt toolbox="$TOOLBOX"} \
    --output "type=oci,dest=$d/img.tar,compression=gzip${2:+,$2}" \
    --progress=plain > "$d/log" 2>&1 || { fail "oci export failed"; tail -n 20 "$d/log"; return 1; }
  mkdir "$d/img" && tar -xf "$d/img.tar" -C "$d/img"
  manifest="$(blob "$d" "$(digests "$d/img/index.json" | head -n 1)")"
  config="$(blob "$d" "$(digests "$manifest" | head -n 1)")"
}
blob() { echo "$1/img/blobs/sha256/${2#sha256:}"; }
digests() { grep -o '"digest": *"sha256:[0-9a-f]*"' "$1" | grep -o 'sha256:[0-9a-f]*'; }

# ---- cases -------------------------------------------------------------

case_tmpfs_mount() {
  recipe tmpfs_mount <<'EOF'
build:
  content:
    a: ["run --mount=type=tmpfs,target=/scratch echo x > /scratch/f && cp /scratch/f /etc/from-tmpfs"]
EOF
  must_build tmpfs_mount || return 0
  has tmpfs_mount etc/from-tmpfs x
  hasnot tmpfs_mount scratch/f
}

case_context_excludes_output() {
  recipe context_excludes_output <<'EOF'
build:
  content:
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
build:
  system:
    fake:
      - copy --chmod=0755 files/bootc /usr/bin/bootc
      - run mkdir -p /usr/lib/modules/7.0 && echo kernel > /usr/lib/modules/7.0/vmlinuz && echo initrd > /usr/lib/modules/7.0/initramfs.img && echo dep > /usr/lib/modules/7.0/modules.dep
  kernel:
    out: ["run rm /usr/lib/modules/*/vmlinuz /usr/lib/modules/*/initramfs.img"]
  uki:
    uki:
      - run --mount=type=bind,from=kernel,target=/target --mount=type=bind,from=system,source=/usr/lib/modules,target=/kernels mkdir -p /boot/EFI/Linux && for d in /kernels/*; do bootc container ukify --rootfs /target --kernel-dir "$d" -- --output "/boot/EFI/Linux/$(basename "$d").efi"; done
EOF
  # The fake ukify writes kernel, initramfs and the tree below
  # /usr/lib/modules into the "UKI".
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

# The image carries the resolved recipe in its label org.nomira.recipe,
# with the imports merged. The context holds only what copies name.
case_record() {
  recipe record <<'EOF'
imports: [groups/a.yml]
build:
  content:
    b: ["run echo b > /etc/b"]
EOF
  mkdir -p "$work/record/groups"
  printf 'build:\n  content:\n    a: ["copy files/a /etc/a"]\n' > "$work/record/groups/a.yml"
  echo a > "$work/record/files/a"
  echo secret > "$work/record/files/unused"
  echo '{}' > "$work/record/unused.yml"
  must_build record || return 0
  has record etc/a a
  has record etc/b b
  hasnot record usr/share/nomira
  logged record 'load] context: files/a$'
  notlogged record 'unused'
  oci record || return 0
  grep -q '"org.nomira.recipe"' "$config" || { fail "no label org.nomira.recipe"; return 0; }
  grep -q 'copy files/a /etc/a' "$config" || fail "the label lacks the imported group"
  grep -q 'from groups/a.yml' "$config" || fail "the label does not say where the group came from"
  ! grep -q 'imports:' "$config" || fail "the label still has imports"
  grep -q '"org.nomira.frontend.version"' "$config" || fail "no label org.nomira.frontend.version"

  RECIPE_SETTINGS="record: false" recipe record_off <<'EOF'
build:
  content:
    a: ["run echo x > /etc/x"]
EOF
  oci record_off || return 0
  ! grep -q 'org.nomira.recipe' "$config" || fail "label despite record: false"
}

# Every run sees settings.source_date_epoch, and the exported image is
# clamped to it: its creation time and the times of the files in its
# layers.
case_source_date_epoch() {
  RECIPE_SETTINGS="source_date_epoch: 1234" recipe epoch <<'EOF'
build:
  content:
    a: ["run echo $SOURCE_DATE_EPOCH > /etc/epoch"]
EOF
  must_build epoch || return 0
  has epoch etc/epoch 1234
  oci epoch rewrite-timestamp=true || return 0
  # Go writes the keys of the config sorted, so its first "created" is
  # the image's.
  layer="$(blob "$d" "$(digests "$manifest" | tail -n 1)")"
  created="$(grep -o '"created": *"[^"]*"' "$config" | head -n 1 | sed 's/.*: *"//; s/"$//')"
  mkdir "$d/layer" && tar -xzf "$layer" -C "$d/layer"
  newest="$(find "$d/layer" -type f -exec stat -c %Y {} + | sort -n | tail -n 1)"
  got="$created $newest"
  [ "$got" = "1970-01-01T00:20:34Z 1234" ] || fail "creation time and newest file in the last layer: $got"

  # Without a source date epoch, runs do not see SOURCE_DATE_EPOCH.
  RECIPE_SETTINGS="source_date_epoch: false" recipe epoch_off <<'EOF'
build:
  content:
    a: ["run echo \"[${SOURCE_DATE_EPOCH:-}]\" > /etc/epoch"]
EOF
  must_build epoch_off || return 0
  has epoch_off etc/epoch '[]'
}

# settings.mounts reach every run: what goes to a tmpfs there never ends
# up in the image, unless the run mounts something at that target itself.
case_mounts() {
  RECIPE_SETTINGS="mounts: {/var/log: type=tmpfs, /var/cache: type=tmpfs, /var/lib/e2e: 'type=cache,id=e2e-mounts'}" recipe mounts <<EOF
stages:
  s:
    from: ${BASE}
    commands: ["run mkdir /out && test -d /var/lib/e2e && mount | grep -q ' /var/log type tmpfs' && echo yes > /out/seen"]
build:
  content:
    a: ["run echo l > /var/log/build.log && mkdir -p /var/cache/x && echo c > /var/cache/x/c && echo cached > /var/lib/e2e/f && echo kept > /etc/kept"]
  later:
    b: ["run --mount=type=bind,from=s,source=/out,target=/s cp /s/seen /etc/seen && cat /var/lib/e2e/f > /etc/from-cache"]
    c: ["run --mount=type=cache,target=/var/cache/pkg,id=e2e-nested echo x > /var/cache/pkg/f && cat /var/cache/pkg/f > /etc/nested"]
EOF
  must_build mounts || return 0
  has mounts etc/kept kept
  has mounts etc/seen yes
  has mounts etc/from-cache cached
  has mounts etc/nested x
  hasnot mounts var/log/build.log
  hasnot mounts var/cache/x
  hasnot mounts var/cache/pkg/f
  hasnot mounts var/lib/e2e/f

  # A run's own mount at the same target wins.
  RECIPE_SETTINGS="mounts: {/var/log: type=tmpfs}" recipe mounts_own <<'EOF'
build:
  content:
    a: ["run --mount=type=cache,target=/var/log,id=e2e-own echo l > /var/log/own && cat /var/log/own > /etc/own"]
EOF
  must_build mounts_own || return 0
  has mounts_own etc/own l
  hasnot mounts_own var/log/own

  # Without settings.mounts nothing is mounted.
  recipe mounts_none <<'EOF'
build:
  content:
    a: ["run echo l > /var/log/build.log"]
EOF
  must_build mounts_none || return 0
  has mounts_none var/log/build.log l

}

case_secret_mount() {
  recipe secret_mount <<'EOF'
build:
  content:
    a: ["run --mount=type=secret,id=tok,required=true wc -c < /run/secrets/tok | tr -d ' ' > /etc/tok-size"]
EOF
  printf abc > "$work/secret_mount/tok"
  BUILD_SECRET="id=tok,src=$work/secret_mount/tok" must_build secret_mount || return 0
  has secret_mount etc/tok-size 3
  hasnot secret_mount run/secrets/tok
  # A required secret that is not given fails the build. (A cached step
  # would not notice: the secret is not part of the cache key.)
  recipe secret_missing <<'EOF'
build:
  content:
    a: ["run --mount=type=secret,id=tok,required=true echo missing-$nonce > /etc/x"]
EOF
  sed -i "s/\$nonce/$(date +%s%N)/" "$work/secret_missing/recipe.yml"
  must_fail secret_missing "tok"
}

case_groups_merge() {
  recipe groups_merge <<'EOF'
build:
  content:
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
build:
  content:
    a: ["run echo a > /etc/a"]
    b: ["run echo b > /etc/a"]
EOF
  must_fail conflict_write 'groups "a" and "b": /etc/a (both write, with different content, mode or owner)'
  logged conflict_write 'its groups are not independent'
  logged conflict_write 'hash 1 shared paths'
}

case_conflict_delete() {
  # One group deletes what the other writes. A layer mounted on its own
  # does not show deletions; they are found by walking the trees.
  recipe conflict_delete <<'EOF'
build:
  content:
    a: ["run rm /etc/hostname"]
    b: ["run echo x > /etc/hostname"]
EOF
  must_fail conflict_delete '/etc/hostname'
  logged conflict_delete 'groups "a" and "b"'
}

case_conflict_delete_dir() {
  # Deleting a directory collides with a write below it.
  recipe conflict_delete_dir <<'EOF'
build:
  content:
    a: ["run rm -rf /etc/apk"]
    b: ["run echo x > /etc/apk/new"]
EOF
  must_fail conflict_delete_dir '/etc/apk'
}

case_conflict_isolated() {
  # A group without run against a group with run.
  recipe conflict_isolated <<'EOF'
build:
  content:
    a: ["copy files/a /etc/a"]
    b: ["run echo b > /etc/a"]
EOF
  echo a > "$work/conflict_isolated/files/a"
  must_fail conflict_isolated 'groups "a" and "b": /etc/a (both write, with different content, mode or owner)'
}

case_delete_alone_is_fine() {
  recipe delete_alone <<'EOF'
build:
  content:
    a: ["run rm /etc/hostname"]
    b: ["run echo b > /etc/b"]
EOF
  must_build delete_alone || return 0
  hasnot delete_alone etc/hostname
  has delete_alone etc/b b
}

# The same file with the same content, mode and owner from two groups is
# what either order gives: no conflict. So is the same deletion.
case_identical() {
  recipe identical <<'EOF'
build:
  content:
    a: ["run echo same > /etc/same && rm /etc/hostname && echo a > /etc/a"]
    b: ["run echo same > /etc/same && rm /etc/hostname"]
    c: ["copy files/same /etc/same"]
EOF
  echo same > "$work/identical/files/same"
  must_build identical || return 0
  has identical etc/same same
  has identical etc/a a
  hasnot identical etc/hostname
  # Only the shared path is hashed, and only in the groups that write it.
  logged identical '\[check 1/1 content/a\] hash 1 shared paths'
  notlogged identical 'content/a] hash 2'
}

case_rewrite_counts_as_write() {
  # A file rewritten with the same content is in the group's layer and
  # would cover a sibling's change, so it is a conflict.
  recipe rewrite <<'EOF'
build:
  content:
    a: ["run cp /etc/hostname /tmp/h && cat /tmp/h > /etc/hostname && rm /tmp/h"]
    b: ["run echo changed > /etc/hostname"]
EOF
  must_fail rewrite '/etc/hostname'
}

case_serial_phases() {
  # A later phase sees the result of an earlier one.
  recipe serial <<'EOF'
build:
  one:
    a: ["run echo 1 > /etc/n"]
  two:
    a: ["run echo $(( $(cat /etc/n) + 1 )) > /etc/n"]
    b: ["run cp /etc/n /etc/seen"]
EOF
  must_build serial || return 0
  has serial etc/n 2
  has serial etc/seen 1
}

case_collectors() {
  recipe collectors <<'EOF'
build:
  words:
    collect: run for w in ${items}; do echo "$w" >> /etc/words; done
  features:
    one:
      words: [pear, apple]
      later: [x]
      numbers: ["2"]
      commands: ["copy files/f /etc/one"]
    two:
      words: [apple, fig]
      numbers: ["1"]
    three: ["run test -f /etc/words && echo ok > /etc/three"]
  later:
    later:
      collect: run cat /etc/one > /etc/later && echo ${items} >> /etc/later
    numbers:
      collect: run echo ${items} > /etc/numbers
    nobody:
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
  has collectors etc/numbers "1 2"
  hasnot collectors etc/nobody
}

case_long_form_and_imports() {
  recipe long_form <<EOF
imports: [features/feature.yml, phases/last.yml]
stages:
  shared:
    from: ${BASE}
    commands: ["run mkdir /out && echo shared > /out/s"]
build:
  features:
    other:
      commands:
        - copy --from=gen /out/g /etc/other
EOF
  mkdir -p "$work/long_form/features" "$work/long_form/phases"
  cat > "$work/long_form/features/feature.yml" <<EOF
stages:
  gen:
    from: ${BASE}
    commands: ["run mkdir /out && echo generated > /out/g"]
build:
  features:
    feature:
      expects: [/etc/alpine-release]
      commands:
        - env GREETING=hello
        - copy --chmod=0600 files/f /etc/feature/f
        - copy --from=gen /out/g /etc/feature/generated
        - copy --from=shared /out/s /etc/feature/shared
EOF
  printf 'build:\n  last:\n    a:\n      - env WHO=nomira\n      - run echo "hi $WHO" > /etc/last\n' > "$work/long_form/phases/last.yml"
  echo f > "$work/long_form/files/f"
  must_build long_form || return 0
  has long_form etc/feature/f f
  mode long_form etc/feature/f 600
  has long_form etc/feature/generated generated
  has long_form etc/other generated
  has long_form etc/feature/shared shared
  has long_form etc/last "hi nomira"
}

# Imports by URL, fetched by BuildKit and checked against their pin; a
# file read from a URL imports relative to it.
case_url_import() {
  if [ -z "${HTTP_DIR:-}" ] || [ -z "${HTTP_URL:-}" ]; then
    echo "    skipped: set HTTP_DIR and HTTP_URL"
    return 0
  fi
  mkdir -p "$HTTP_DIR/parts"
  printf 'build:\n  content:\n    b: ["run echo b > /etc/b"]\n' > "$HTTP_DIR/parts/b.yml"
  sum="$(sha256sum "$HTTP_DIR/parts/b.yml" | cut -d' ' -f1)"
  printf 'imports: ["b.yml#sha256=%s"]\nbuild:\n  content:\n    a: ["run echo a > /etc/a"]\n' "$sum" > "$HTTP_DIR/parts/a.yml"
  sum="$(sha256sum "$HTTP_DIR/parts/a.yml" | cut -d' ' -f1)"
  recipe url_import <<EOF
imports: ["$HTTP_URL/parts/a.yml#sha256=$sum"]
build:
  content:
    c: ["run echo c > /etc/c"]
EOF
  must_build url_import || return 0
  has url_import etc/a a
  has url_import etc/b b
  has url_import etc/c c
  # resolve: the merged recipe names where each part came from.
  MODE=resolve must_build url_import || return 0
  grep -q "from $HTTP_URL/parts/b.yml" "$work/url_import/out/recipe.yml" || fail "resolve: $(cat "$work/url_import/out/recipe.yml")"
  [ -s "$work/url_import/out/version" ] || fail "resolve: no version"
  # A file that does not match its pin. (A pinned file is cached by its
  # pin: once fetched, BuildKit does not ask the server again.)
  recipe url_mismatch <<EOF
imports: ["$HTTP_URL/parts/a.yml#sha256=$(printf '%064d' 0)"]
EOF
  must_fail url_mismatch 'digest'
}

# lint parses the recipe and stops: nothing is built.
case_lint() {
  recipe lint <<'EOF'
build:
  content:
    a: ["run echo never > /etc/never"]
EOF
  MODE=lint must_build lint || return 0
  hasnot lint etc/never
  [ -s "$work/lint/out/version" ] || fail "lint: no version"
  notlogged lint 'run echo never'
  recipe lint_bad <<'EOF'
build:
  content:
    groups:
      a: ["run true"]
EOF
  MODE=lint must_fail lint_bad 'recipe: recipe.yml:5: build.content.groups is gone: groups stand directly under the phase now'
}

case_expects() {
  recipe expects <<'EOF'
build:
  features:
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
  body="build:
  features:
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
  cached indep '[1/1 features/copies] copy files/f /etc/copied'
  notcached indep "[1/1 features/runs] run echo $nonce > /etc/ran"
}

case_fallback_symlink() {
  # Copying below a symbolic link: the group is built on the state before
  # it, the file follows the link, and the build says so.
  recipe fb_symlink <<'EOF'
build:
  prepare-system:
    a: ["run mkdir /real && ln -s real /link"]
  features:
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
build:
  prepare-system:
    a: ["run mkdir -m 750 /etc/strict"]
  features:
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
build:
  prepare-system:
    a: ["run mkdir /etc/thing && echo keep > /etc/thing/keep"]
  features:
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
build:
  features:
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
delete_alone_is_fine identical rewrite_counts_as_write serial_phases
collectors long_form_and_imports url_import lint expects independent_of_base
fallback_symlink fallback_mode fallback_type isolated_single_group tmpfs_mount
context_excludes_output uki secret_mount record source_date_epoch mounts"

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
