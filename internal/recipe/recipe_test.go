package recipe

import (
	"strings"
	"testing"
)

const sample = `
settings:
  name: meinsystem
  base: ghcr.io/example/arch-bootc:latest
  validation: strict
  args:
    SNAPSHOT: "2026/11/10"

prepare:
  aurbuild:
    from: docker.io/library/archlinux:base-devel
    commands:
      - run pacman -Syu --noconfirm git

phases:
  - name: base
    groups:
      system:
        - run --mount=type=cache,target=/var/cache/pacman/pkg pacman -Syu --needed --noconfirm kmscon
  - name: configs
    validation: allow-identical
    shared:
      - /usr/lib/tmpfiles.d/
    groups:
      kmscon:
        - copy --chmod=0644 files/kmscon/ /etc/kmscon/
      aur:
        - copy --from=aurbuild /out/ /var/tmp/aur/
      boot:
        - |
          run python3 ./gen.py \
            --snapshot ${SNAPSHOT} --out /usr/lib/bootc/kargs.d/
`

func TestParseSample(t *testing.T) {
	r, err := Parse([]byte(sample), nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Settings.Toolbox != DefaultToolbox {
		t.Errorf("toolbox default missing: %q", r.Settings.Toolbox)
	}
	if len(r.Phases) != 2 || len(r.Phases[1].Groups) != 3 {
		t.Fatalf("unexpected structure: %+v", r.Phases)
	}
	run := r.Phases[0].Groups["system"][0].(RunCmd)
	if len(run.Mounts) != 1 || run.Mounts[0].Target != "/var/cache/pacman/pkg" {
		t.Errorf("cache mount not parsed: %+v", run.Mounts)
	}
	if !strings.HasPrefix(run.Script, "pacman -Syu") {
		t.Errorf("script mangled: %q", run.Script)
	}
	cp := r.Phases[1].Groups["kmscon"][0].(CopyCmd)
	if cp.Chmod == nil || *cp.Chmod != 0o644 {
		t.Errorf("chmod not parsed: %+v", cp.Chmod)
	}
	boot := r.Phases[1].Groups["boot"][0].(RunCmd)
	if !strings.Contains(boot.Script, "--snapshot 2026/11/10") {
		t.Errorf("arg expansion failed: %q", boot.Script)
	}
}

func TestBuildArgOverride(t *testing.T) {
	r, err := Parse([]byte(sample), map[string]string{"SNAPSHOT": "2026/12/01"})
	if err != nil {
		t.Fatal(err)
	}
	boot := r.Phases[1].Groups["boot"][0].(RunCmd)
	if !strings.Contains(boot.Script, "2026/12/01") {
		t.Errorf("override failed: %q", boot.Script)
	}
	if _, err := Parse([]byte(sample), map[string]string{"NOPE": "x"}); err == nil {
		t.Error("undeclared build-arg should fail")
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"unknown verb":       "settings: {base: x}\nphases: [{name: a, groups: {g: ['frobnicate y']}}]",
		"unknown copy stage":  "settings: {base: x}\nphases: [{name: a, groups: {g: ['copy --from=nope /a /b']}}]",
		"empty phases":        "settings: {base: x}\nphases: []",
		"missing base":        "settings: {name: x}\nphases: [{name: a, groups: {g: ['run true']}}]",
		"bad policy":          "settings: {base: x, validation: lax}\nphases: [{name: a, groups: {g: ['run true']}}]",
		"unknown key":         "settings: {base: x}\nphasen: []",
		"relative dst":        "settings: {base: x}\nphases: [{name: a, groups: {g: ['copy a b']}}]",
		"duplicate phase":     "settings: {base: x}\nphases: [{name: a, groups: {g: ['run true']}}, {name: a, groups: {g: ['run true']}}]",
	}
	for name, src := range cases {
		if _, err := Parse([]byte(src), nil); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestExpandArgsKeepsUndeclared(t *testing.T) {
	got := ExpandArgs("echo ${A} ${B} $repo", map[string]string{"A": "1"})
	if got != "echo 1 ${B} $repo" {
		t.Errorf("got %q", got)
	}
}
