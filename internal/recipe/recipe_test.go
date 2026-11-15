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
		"unknown copy stage": "settings: {base: x}\nphases: [{name: a, groups: {g: ['copy --from=nope /a /b']}}]",
		"empty phases":       "settings: {base: x}\nphases: []",
		"missing base":       "settings: {name: x}\nphases: [{name: a, groups: {g: ['run true']}}]",
		"bad policy":         "settings: {base: x, validation: lax}\nphases: [{name: a, groups: {g: ['run true']}}]",
		"unknown key":        "settings: {base: x}\nphasen: []",
		"relative dst":       "settings: {base: x}\nphases: [{name: a, groups: {g: ['copy a b']}}]",
		"duplicate phase":    "settings: {base: x}\nphases: [{name: a, groups: {g: ['run true']}}, {name: a, groups: {g: ['run true']}}]",
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

func mapReader(files map[string]string) ReadFunc {
	return func(name string) ([]byte, error) {
		if s, ok := files[name]; ok {
			return []byte(s), nil
		}
		return nil, errf("no such file %q", name)
	}
}

func TestIncludes(t *testing.T) {
	files := map[string]string{
		"recipe.yml": `
settings: {base: x}
prepare:
  aur:
    from: y
    commands: {include: stages/aur.yml}
phases:
  - include: phases/base.yml
  - name: configs
    groups:
      kmscon: {include: groups/kmscon.yml}
      nm:
        - copy configs/nm/ /etc/NetworkManager/
`,
		"stages/aur.yml":    "- run make\n",
		"phases/base.yml":   "- name: base\n  groups:\n    system: {include: ../groups/system.yml}\n- include: extra.yml\n",
		"phases/extra.yml":  "name: extra\ngroups:\n  g: ['run true']\n",
		"groups/system.yml": "- run pacman -Syu\n",
		"groups/kmscon.yml": "- copy files/kmscon/ /etc/kmscon/\n",
	}
	r, err := ParseFile("recipe.yml", mapReader(files), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range r.Phases {
		names = append(names, p.Name)
	}
	if strings.Join(names, ",") != "base,extra,configs" {
		t.Fatalf("phase order: %v", names)
	}
	if len(r.Phases[2].Groups["kmscon"]) != 1 || len(r.Prepare["aur"].Commands) != 1 {
		t.Fatalf("includes not resolved: %+v", r)
	}
}

func TestIncludeErrors(t *testing.T) {
	base := "settings: {base: x}\nphases:\n"
	cases := map[string]map[string]string{
		"cycle": {
			"recipe.yml": base + "  - include: a.yml\n",
			"a.yml":      "- include: a.yml\n",
		},
		"escape":  {"recipe.yml": base + "  - include: ../x.yml\n"},
		"missing": {"recipe.yml": base + "  - include: nope.yml\n"},
		"mixed":   {"recipe.yml": base + "  - include: a.yml\n    name: x\n", "a.yml": "name: a\ngroups: {g: ['run true']}\n"},
		"unknown key in include": {
			"recipe.yml": base + "  - include: a.yml\n",
			"a.yml":      "name: a\ngruppen: {}\n",
		},
		"stream": {"recipe.yml": base + "  - include: a.yml\n"},
	}
	for name, files := range cases {
		if _, err := ParseFile("recipe.yml", mapReader(files), nil); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
