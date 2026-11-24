package recipe

import (
	"strings"
	"testing"
)

const sample = `
settings:
  name: mysystem
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
	run := r.Phases[0].Groups["system"].Commands[0].(RunCmd)
	if len(run.Mounts) != 1 || run.Mounts[0].Target != "/var/cache/pacman/pkg" {
		t.Errorf("cache mount not parsed: %+v", run.Mounts)
	}
	if !strings.HasPrefix(run.Script, "pacman -Syu") {
		t.Errorf("script mangled: %q", run.Script)
	}
	cp := r.Phases[1].Groups["kmscon"].Commands[0].(CopyCmd)
	if cp.Chmod == nil || *cp.Chmod != 0o644 {
		t.Errorf("chmod not parsed: %+v", cp.Chmod)
	}
	boot := r.Phases[1].Groups["boot"].Commands[0].(RunCmd)
	if !strings.Contains(boot.Script, "--snapshot 2026/11/10") {
		t.Errorf("arg expansion failed: %q", boot.Script)
	}
}

func TestBuildArgOverride(t *testing.T) {
	r, err := Parse([]byte(sample), map[string]string{"SNAPSHOT": "2026/12/01"})
	if err != nil {
		t.Fatal(err)
	}
	boot := r.Phases[1].Groups["boot"].Commands[0].(RunCmd)
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
	if len(r.Phases[2].Groups["kmscon"].Commands) != 1 || len(r.Prepare["aur"].Commands) != 1 {
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

func TestEnvNoCacheAndMetadata(t *testing.T) {
	src := `
settings:
  base: x
  description: demo ${V}
  labels: {org.example.version: "${V}"}
  env: {LANG: C.UTF-8}
  platforms: [linux/amd64, linux/arm64]
  tags: ["${V}", stable]
  args: {V: "1.2"}
phases:
  - name: a
    env: {MAKEFLAGS: -j4}
    groups:
      g:
        - env GREETING=hello world
        - run --no-cache --mount=type=cache,target=/c make
`
	r, err := Parse([]byte(src), nil)
	if err != nil {
		t.Fatal(err)
	}
	s := r.Settings
	if s.Description != "demo 1.2" || s.Labels["org.example.version"] != "1.2" || s.Env["LANG"] != "C.UTF-8" {
		t.Errorf("settings: %+v", s)
	}
	if strings.Join(s.Tags, ",") != "1.2,stable" || len(s.Platforms) != 2 {
		t.Errorf("tags/platforms: %v %v", s.Tags, s.Platforms)
	}
	if r.Phases[0].Env["MAKEFLAGS"] != "-j4" {
		t.Errorf("phase env: %v", r.Phases[0].Env)
	}
	cmds := r.Phases[0].Groups["g"].Commands
	if e := cmds[0].(EnvCmd); e.Name != "GREETING" || e.Value != "hello world" {
		t.Errorf("env cmd: %+v", e)
	}
	if run := cmds[1].(RunCmd); !run.NoCache || len(run.Mounts) != 1 || run.Script != "make" {
		t.Errorf("run cmd: %+v", run)
	}

	bad := map[string]string{
		"env name":  "settings: {base: x, env: {'1A': b}}\nphases: [{name: a, groups: {g: ['run true']}}]",
		"env cmd":   "settings: {base: x}\nphases: [{name: a, groups: {g: ['env NOVALUE']}}]",
		"platform":  "settings: {base: x, platforms: [amd64]}\nphases: [{name: a, groups: {g: ['run true']}}]",
		"tag":       "settings: {base: x, tags: ['a:b']}\nphases: [{name: a, groups: {g: ['run true']}}]",
		"phase env": "settings: {base: x}\nphases: [{name: a, env: {'a-b': c}, groups: {g: ['run true']}}]",
	}
	for name, s := range bad {
		if _, err := Parse([]byte(s), nil); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestCollectorsAndLongGroups(t *testing.T) {
	files := map[string]string{
		"recipe.yml": `
settings: {base: x}
prepare:
  shared:
    from: y
    commands: ['run true']
phases:
  - name: packages
    collect: run --mount=type=cache,target=/c pacman -S --needed --noconfirm ${items}
  - name: features
    groups:
      terminal: {include: features/terminal.yml}
      editor:
        packages: [vim, kmscon]
      network:
        - copy files/nm/ /etc/NetworkManager/
  - name: services
    collect: run systemctl enable ${items}
  - name: unused
    collect: run echo ${items}
`,
		"features/terminal.yml": `
packages: [kmscon]
services: [kmsconvt@tty1.service]
expects: [/usr/lib/systemd/system/kmsconvt@.service]
prepare:
  theme:
    from: z
    commands: {include: theme.yml}
commands:
  - copy files/kmscon/ /etc/kmscon/
  - copy --from=theme /out/theme.conf /etc/kmscon/theme.conf
  - copy --from=shared /x /y
`,
		"features/theme.yml": "- run ./gen-theme.sh\n",
	}
	r, err := ParseFile("recipe.yml", mapReader(files), nil)
	if err != nil {
		t.Fatal(err)
	}
	pk := r.Phases[0]
	if !pk.IsCollector() || strings.Join(pk.Items, ",") != "kmscon,vim" {
		t.Fatalf("packages collector: %+v", pk)
	}
	if pk.Collect.Script != "pacman -S --needed --noconfirm kmscon vim" || len(pk.Collect.Mounts) != 1 {
		t.Errorf("collect command: %+v", pk.Collect)
	}
	if got := r.Phases[2].Collect.Script; got != "systemctl enable kmsconvt@tty1.service" {
		t.Errorf("services: %q", got)
	}
	if len(r.Phases[3].Items) != 0 {
		t.Errorf("collector without contributions has items: %v", r.Phases[3].Items)
	}
	feat := r.Phases[1]
	term := feat.Groups["terminal"]
	if term.HasRun() || len(term.Commands) != 3 || term.Expects[0] != "/usr/lib/systemd/system/kmsconvt@.service" {
		t.Errorf("terminal: %+v", term)
	}
	// The local stage is namespaced, the global one is not.
	if from := term.Commands[1].(CopyCmd).From; from != "features/terminal/theme" {
		t.Errorf("local stage reference: %q", from)
	}
	if from := term.Commands[2].(CopyCmd).From; from != "shared" {
		t.Errorf("global stage reference: %q", from)
	}
	if st, ok := r.Prepare["features/terminal/theme"]; !ok || len(st.Commands) != 1 {
		t.Errorf("local stage missing: %+v", r.Prepare)
	}
	// A group that only contributes builds nothing.
	if strings.Join(feat.BuildGroupNames(), ",") != "network,terminal" || len(feat.GroupNames()) != 3 {
		t.Errorf("build groups: %v", feat.BuildGroupNames())
	}
	if g := r.Graph(); !strings.Contains(g, "collect packages (2 items)") || !strings.Contains(g, "prep_features_terminal_theme") {
		t.Errorf("graph:\n%s", g)
	}
}

func TestCollectorErrors(t *testing.T) {
	head := "settings: {base: x}\nphases:\n"
	bad := map[string]string{
		"no placeholder":       head + "  - {name: p, collect: run pacman -S}\n",
		"not a run":            head + "  - {name: p, collect: 'copy ${items} /x'}\n  - {name: f, groups: {g: {p: [a]}}}\n",
		"collect with groups":  head + "  - {name: p, collect: 'run x ${items}', groups: {g: ['run true']}}\n",
		"reserved name":        head + "  - {name: commands, collect: 'run x ${items}'}\n",
		"unknown contribution": head + "  - {name: f, groups: {g: {pakages: [a]}}}\n",
		"item with space":      head + "  - {name: p, collect: 'run x ${items}'}\n  - {name: f, groups: {g: {p: ['a b']}}}\n",
		"item with shell":      head + "  - {name: p, collect: 'run x ${items}'}\n  - {name: f, groups: {g: {p: ['a;rm']}}}\n",
		"item is an option":    head + "  - {name: p, collect: 'run x ${items}'}\n  - {name: f, groups: {g: {p: ['--now']}}}\n",
		"empty group":          head + "  - {name: f, groups: {g: {expects: [/a]}}}\n",
		"relative expects":     head + "  - {name: f, groups: {g: {expects: [a/b], commands: ['run true']}}}\n",
		"include plus keys":    head + "  - {name: f, groups: {g: {include: a.yml, expects: [/a]}}}\n",
		"unknown local stage":  head + "  - {name: f, groups: {g: {commands: ['copy --from=nope /a /b']}}}\n",
		"args named items":     "settings: {base: x, args: {items: a}}\nphases:\n  - {name: f, groups: {g: ['run true']}}\n",
		"neither":              head + "  - {name: f}\n",
	}
	for name, s := range bad {
		if _, err := Parse([]byte(s), nil); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestTmpfsMount(t *testing.T) {
	c, err := ParseCommand("run --mount=type=tmpfs,target=/etc/pacman.d/gnupg --mount=type=cache,target=/c pacman-key --init")
	if err != nil {
		t.Fatal(err)
	}
	run := c.(RunCmd)
	if len(run.Mounts) != 2 || run.Mounts[0] != (Mount{Type: "tmpfs", Target: "/etc/pacman.d/gnupg"}) ||
		run.Mounts[1] != (Mount{Type: "cache", Target: "/c", ID: "/c", Sharing: "shared"}) || run.Script != "pacman-key --init" {
		t.Errorf("parsed: %+v", run)
	}
	for _, bad := range []string{
		"run --mount=type=tmpfs,target=/x,id=a true",
		"run --mount=type=bind,target=/x true",
		"run --mount=type=tmpfs true",
	} {
		if _, err := ParseCommand(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

func TestBindMountsAndPhaseSources(t *testing.T) {
	recipe := func(body string) map[string]string {
		return map[string]string{"recipe.yml": "settings: {base: x}\nprepare:\n  tools:\n    from: y\n    commands: ['run true']\nphases:\n" + body}
	}
	good := recipe(`
  - name: system
    groups:
      a: ['run true']
  - name: kernel
    groups:
      k: ['run rm -f /vmlinuz']
  - name: uki
    groups:
      u:
        - run --mount=type=bind,from=kernel,target=/target --mount=type=bind,from=system,source=/usr/lib/modules,target=/kernels --mount=type=bind,from=tools,target=/t ls /target
        - copy --from=system /etc/hostname /etc/hostname
`)
	r, err := ParseFile("recipe.yml", mapReader(good), nil)
	if err != nil {
		t.Fatal(err)
	}
	run := r.Phases[2].Groups["u"].Commands[0].(RunCmd)
	want := []Mount{
		{Type: "bind", From: "kernel", Source: "/", Target: "/target"},
		{Type: "bind", From: "system", Source: "/usr/lib/modules", Target: "/kernels"},
		{Type: "bind", From: "tools", Source: "/", Target: "/t"},
	}
	for i, m := range want {
		if run.Mounts[i] != m {
			t.Errorf("mount %d: %+v, want %+v", i, run.Mounts[i], m)
		}
	}
	if c := r.Phases[2].Groups["u"].Commands[1].(CopyCmd); c.From != "system" {
		t.Errorf("copy --from: %q", c.From)
	}

	for name, body := range map[string]string{
		"later phase": `
  - name: a
    groups:
      g: ['run --mount=type=bind,from=b,target=/x true']
  - name: b
    groups:
      g: ['run true']
`,
		"own phase": `
  - name: a
    groups:
      g: ['copy --from=a /x /y']
`,
		"unknown": `
  - name: a
    groups:
      g: ['run --mount=type=bind,from=nope,target=/x true']
`,
		"stage and phase": `
  - name: tools
    groups:
      g: ['run true']
`,
		"bind without from": `
  - name: a
    groups:
      g: ['run --mount=type=bind,target=/x true']
`,
	} {
		if _, err := ParseFile("recipe.yml", mapReader(recipe(body)), nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSecretMount(t *testing.T) {
	c, err := ParseCommand("run --mount=type=secret,id=db.key,required=true --mount=type=secret,id=tok,target=/t sbsign")
	if err != nil {
		t.Fatal(err)
	}
	run := c.(RunCmd)
	if run.Mounts[0] != (Mount{Type: "secret", ID: "db.key", Target: "/run/secrets/db.key", Required: true}) ||
		run.Mounts[1] != (Mount{Type: "secret", ID: "tok", Target: "/t"}) {
		t.Errorf("parsed: %+v", run.Mounts)
	}
	for _, bad := range []string{
		"run --mount=type=secret,target=/x true",
		"run --mount=type=secret,id=a/b true",
		"run --mount=type=cache,target=/x,required=true true",
	} {
		if _, err := ParseCommand(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

func TestStageFromPhase(t *testing.T) {
	files := func(use string) map[string]string {
		return map[string]string{"recipe.yml": `
settings: {base: x}
prepare:
  initramfs:
    from: packages
    commands: ['run true']
phases:
  - name: early
    groups:
      g: ['` + use + `']
  - name: packages
    groups:
      p: ['run true']
  - name: late
    groups:
      g: ['` + use + `']
`}
	}
	if _, err := ParseFile("recipe.yml", mapReader(files("run true")), nil); err != nil {
		t.Fatal(err)
	}
	r, _ := ParseFile("recipe.yml", mapReader(files("run true")), nil)
	if r.Prepare["initramfs"].FromPhase != "packages" {
		t.Errorf("FromPhase: %+v", r.Prepare["initramfs"])
	}
	// Used by a phase before packages: rejected.
	if _, err := ParseFile("recipe.yml", mapReader(files("run --mount=type=bind,from=initramfs,target=/i true")), nil); err == nil {
		t.Error("stage used before its phase: accepted")
	}
}
