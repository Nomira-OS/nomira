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

stages:
  aurbuild:
    from: docker.io/library/archlinux:base-devel
    commands:
      - run pacman -Syu --noconfirm git

build:
  base:
    groups:
      system:
        - run --mount=type=cache,target=/var/cache/pacman/pkg pacman -Syu --needed --noconfirm kmscon
  configs:
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
		"unknown verb":       "settings: {base: x}\nbuild: {a: {groups: {g: ['frobnicate y']}}}",
		"unknown copy stage": "settings: {base: x}\nbuild: {a: {groups: {g: ['copy --from=nope /a /b']}}}",
		"empty phases":       "settings: {base: x}\nbuild: {}",
		"missing base":       "settings: {name: x}\nbuild: {a: {groups: {g: ['run true']}}}",
		"bad policy":         "settings: {base: x, validation: lax}\nbuild: {a: {groups: {g: ['run true']}}}",
		"unknown key":        "settings: {base: x}\nphasen: []",
		"relative dst":       "settings: {base: x}\nbuild: {a: {groups: {g: ['copy a b']}}}",
		"duplicate phase":    "settings: {base: x}\nbuild: {a: {groups: {g: ['run true']}}, a: {groups: {g: ['run true']}}}",
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

func TestImportsMerge(t *testing.T) {
	files := map[string]string{
		"recipe.yml": `# syntax=example/front:1
imports: [base.yml, machine/laptop.yml]
settings:
  name: laptop
  labels: {owner: me}
build:
  features:
    groups:
      editor: [run install-editor]
      network: null
`,
		"base.yml": `imports: [common.yml]
settings:
  name: base
  base: x
  labels: {tier: base}
  tags: [a, b]
stages:
  aur: {from: y, commands: [run make]}
build:
  packages:
    collect: run pacman -S ${items}
  features:
    groups:
      network: {packages: [networkmanager], commands: [run nm]}
      ssh: {packages: [openssh]}
  services:
    collect: run systemctl enable ${items}
`,
		"common.yml": "settings: {toolbox: busybox}\n",
		"machine/laptop.yml": `settings:
  tags: [c]
build:
  firmware:
    collect: run pacman -S ${items}
  packages: {}
  features:
    groups:
      hardware: {firmware: [linux-firmware-intel]}
  uki:
    groups:
      uki: [run make-uki]
`,
	}
	r, err := ParseFile("recipe.yml", mapReader(files), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range r.Phases {
		names = append(names, p.Name)
	}
	// firmware is first in its file: before packages, the next phase it
	// names; uki comes after features, its predecessor there.
	if got := strings.Join(names, " "); got != "firmware packages features uki services" {
		t.Errorf("phase order: %s", got)
	}
	st := r.Settings
	if st.Name != "laptop" || st.Base != "x" || st.Toolbox != "busybox" {
		t.Errorf("settings: %+v", st)
	}
	if st.Labels["owner"] != "me" || st.Labels["tier"] != "base" {
		t.Errorf("labels are merged: %v", st.Labels)
	}
	if strings.Join(st.Tags, ",") != "c" {
		t.Errorf("lists are replaced: %v", st.Tags)
	}
	feat := r.Phases[2]
	if strings.Join(feat.GroupNames(), ",") != "editor,hardware,ssh" {
		t.Errorf("groups (network removed by null): %v", feat.GroupNames())
	}
	if _, ok := r.Stages["aur"]; !ok {
		t.Error("stage from an import missing")
	}
	if got := strings.Join(r.Files, " "); got != "recipe.yml base.yml common.yml machine/laptop.yml" {
		t.Errorf("files: %s", got)
	}

	res := string(r.Resolved)
	if !strings.HasPrefix(res, "# syntax=example/front:1\n") {
		t.Errorf("resolved starts without the syntax line:\n%s", res)
	}
	for _, want := range []string{"from base.yml", "from machine/laptop.yml", "uki:", "linux-firmware-intel"} {
		if !strings.Contains(res, want) {
			t.Errorf("resolved lacks %q:\n%s", want, res)
		}
	}
	for _, unwanted := range []string{"imports", "network"} {
		if strings.Contains(res, unwanted) {
			t.Errorf("resolved contains %q:\n%s", unwanted, res)
		}
	}
	// The resolved recipe is a recipe: it parses to the same phases.
	again, err := Parse(r.Resolved, nil)
	if err != nil {
		t.Fatalf("resolved does not parse: %v\n%s", err, res)
	}
	if len(again.Phases) != len(r.Phases) || string(again.Resolved[strings.Index(string(again.Resolved), "\n")+1:]) == "" {
		t.Errorf("resolved parses differently")
	}
}

func TestImportErrors(t *testing.T) {
	head := "settings: {base: x}\nbuild: {a: {groups: {g: [run true]}}}\n"
	cases := map[string]map[string]string{
		"cycle":       {"recipe.yml": "imports: [a.yml]\n" + head, "a.yml": "imports: [recipe.yml]\n"},
		"escape":      {"recipe.yml": "imports: [../x.yml]\n" + head},
		"absolute":    {"recipe.yml": "imports: [/etc/x.yml]\n" + head},
		"missing":     {"recipe.yml": "imports: [nope.yml]\n" + head},
		"not a list":  {"recipe.yml": "imports: a.yml\n" + head},
		"unknown key": {"recipe.yml": "imports: [a.yml]\n" + head, "a.yml": "gruppen: {}\n"},
		"order": {
			"recipe.yml": "imports: [a.yml, b.yml]\nsettings: {base: x}\n",
			"a.yml":      "build: {p: {groups: {g: [run a]}}, q: {groups: {g: [run b]}}}\n",
			"b.yml":      "build: {q: {}, p: {}}\n",
		},
		"stream": {"recipe.yml": "imports: [a.yml]\n" + head},
	}
	for name, files := range cases {
		read := mapReader(files)
		if name == "stream" {
			if _, err := Parse([]byte(files["recipe.yml"]), nil); err == nil {
				t.Errorf("%s: expected error", name)
			}
			continue
		}
		if _, err := ParseFile("recipe.yml", read, nil); err == nil {
			t.Errorf("%s: expected error", name)
		} else if name == "order" && !strings.Contains(err.Error(), "different order") {
			t.Errorf("order: %v", err)
		}
	}
}

func TestBuildArgsInResolved(t *testing.T) {
	src := "settings: {base: x, args: {V: '1'}}\nbuild: {a: {groups: {g: ['run echo ${V}']}}}\n"
	r, err := Parse([]byte(src), map[string]string{"V": "2"})
	if err != nil {
		t.Fatal(err)
	}
	again, err := Parse(r.Resolved, nil)
	if err != nil {
		t.Fatal(err)
	}
	if again.Settings.Args["V"] != "2" {
		t.Errorf("override not in the resolved recipe:\n%s", r.Resolved)
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
build:
  a:
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
		"env name":  "settings: {base: x, env: {'1A': b}}\nbuild: {a: {groups: {g: ['run true']}}}",
		"env cmd":   "settings: {base: x}\nbuild: {a: {groups: {g: ['env NOVALUE']}}}",
		"platform":  "settings: {base: x, platforms: [amd64]}\nbuild: {a: {groups: {g: ['run true']}}}",
		"tag":       "settings: {base: x, tags: ['a:b']}\nbuild: {a: {groups: {g: ['run true']}}}",
		"phase env": "settings: {base: x}\nbuild: {a: {env: {'a-b': c}, groups: {g: ['run true']}}}",
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
imports: [features/terminal.yml]
settings: {base: x}
stages:
  shared:
    from: y
    commands: ['run true']
build:
  packages:
    collect: run --mount=type=cache,target=/c pacman -S --needed --noconfirm ${items}
  features:
    groups:
      editor:
        packages: [vim, kmscon]
      network:
        - copy files/nm/ /etc/NetworkManager/
  services:
    collect: run systemctl enable ${items}
  unused:
    collect: run echo ${items}
`,
		"features/terminal.yml": `
build:
  features:
    groups:
      terminal:
        packages: [kmscon]
        services: [kmsconvt@tty1.service]
        expects: [/usr/lib/systemd/system/kmsconvt@.service]
        stages:
          theme:
            from: z
            commands: [run ./gen-theme.sh]
        commands:
          - copy files/kmscon/ /etc/kmscon/
          - copy --from=theme /out/theme.conf /etc/kmscon/theme.conf
          - copy --from=shared /x /y
`,
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
	if st, ok := r.Stages["features/terminal/theme"]; !ok || len(st.Commands) != 1 {
		t.Errorf("local stage missing: %+v", r.Stages)
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
	head := "settings: {base: x}\nbuild:\n"
	bad := map[string]string{
		"no placeholder":       head + "  p: {collect: run pacman -S}\n",
		"not a run":            head + "  p: {collect: 'copy ${items} /x'}\n  f: {groups: {g: {p: [a]}}}\n",
		"collect with groups":  head + "  p: {collect: 'run x ${items}', groups: {g: ['run true']}}\n",
		"reserved name":        head + "  commands: {collect: 'run x ${items}'}\n",
		"unknown contribution": head + "  f: {groups: {g: {pakages: [a]}}}\n",
		"item with space":      head + "  p: {collect: 'run x ${items}'}\n  f: {groups: {g: {p: ['a b']}}}\n",
		"item with shell":      head + "  p: {collect: 'run x ${items}'}\n  f: {groups: {g: {p: ['a;rm']}}}\n",
		"item is an option":    head + "  p: {collect: 'run x ${items}'}\n  f: {groups: {g: {p: ['--now']}}}\n",
		"empty group":          head + "  f: {groups: {g: {expects: [/a]}}}\n",
		"relative expects":     head + "  f: {groups: {g: {expects: [a/b], commands: ['run true']}}}\n",
		"unknown local stage":  head + "  f: {groups: {g: {commands: ['copy --from=nope /a /b']}}}\n",
		"args named items":     "settings: {base: x, args: {items: a}}\nbuild:\n  f: {groups: {g: ['run true']}}\n",
		"neither":              head + "  f: {}\n",
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
		return map[string]string{"recipe.yml": "settings: {base: x}\nstages:\n  tools:\n    from: y\n    commands: ['run true']\nbuild:\n" + body}
	}
	good := recipe(`
  system:
    groups:
      a: ['run true']
  kernel:
    groups:
      k: ['run rm -f /vmlinuz']
  uki:
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
  a:
    groups:
      g: ['run --mount=type=bind,from=b,target=/x true']
  b:
    groups:
      g: ['run true']
`,
		"own phase": `
  a:
    groups:
      g: ['copy --from=a /x /y']
`,
		"unknown": `
  a:
    groups:
      g: ['run --mount=type=bind,from=nope,target=/x true']
`,
		"stage and phase": `
  tools:
    groups:
      g: ['run true']
`,
		"bind without from": `
  a:
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
stages:
  initramfs:
    from: packages
    commands: ['run true']
build:
  early:
    groups:
      g: ['` + use + `']
  packages:
    groups:
      p: ['run true']
  late:
    groups:
      g: ['` + use + `']
`}
	}
	if _, err := ParseFile("recipe.yml", mapReader(files("run true")), nil); err != nil {
		t.Fatal(err)
	}
	r, _ := ParseFile("recipe.yml", mapReader(files("run true")), nil)
	if r.Stages["initramfs"].FromPhase != "packages" {
		t.Errorf("FromPhase: %+v", r.Stages["initramfs"])
	}
	// Used by a phase before packages: rejected.
	if _, err := ParseFile("recipe.yml", mapReader(files("run --mount=type=bind,from=initramfs,target=/i true")), nil); err == nil {
		t.Error("stage used before its phase: accepted")
	}
}

func TestCompressionAndSecrets(t *testing.T) {
	parse := func(settings, cmd string) (*Recipe, error) {
		return ParseFile("recipe.yml", mapReader(map[string]string{"recipe.yml": "settings: {base: x" + settings + "}\nbuild:\n  a:\n    groups:\n      g: ['" + cmd + "']\n"}), nil)
	}
	r, err := parse("", "run true")
	if err != nil || r.Settings.Compression != "zstd" {
		t.Fatalf("default: %v, %+v", err, r)
	}
	if r, err = parse(", compression: uncompressed, secrets: {db.key: /etc/kernel/db.key}", "run --mount=type=secret,id=db.key true"); err != nil ||
		r.Settings.Compression != "uncompressed" || r.Settings.Secrets["db.key"] != "/etc/kernel/db.key" {
		t.Fatalf("declared: %v, %+v", err, r)
	}
	if _, err := parse(", compression: lz4", "run true"); err == nil {
		t.Error("compression lz4 accepted")
	}
	if _, err := parse("", "run --mount=type=secret,id=db.key true"); err == nil {
		t.Error("undeclared secret accepted")
	}
}

func TestContextPathsAndRecord(t *testing.T) {
	src := `
settings: {name: x, base: b}
build:
  features:
    groups:
      a: ["copy files/a.conf /etc/a.conf", "copy files/b/ /etc/b2/"]
      b: ["copy ./files/b/ /etc/b/"]
`
	r, err := Parse([]byte(src), nil)
	if err != nil {
		t.Fatal(err)
	}
	paths, all := r.ContextPaths()
	if all || strings.Join(paths, " ") != "files/a.conf files/b" {
		t.Errorf("context = %v %v", paths, all)
	}
	if !r.Settings.Record {
		t.Error("record is on by default")
	}
	r, err = Parse([]byte(strings.Replace(src, "base: b", "base: b, record: false", 1)), nil)
	if err != nil || r.Settings.Record {
		t.Errorf("record: false: %v", err)
	}
	if _, err = Parse([]byte(strings.Replace(src, "base: b", "base: b, record: features", 1)), nil); err == nil {
		t.Error("record: features must be rejected")
	}
}

func TestSourceDateEpochSetting(t *testing.T) {
	r, err := Parse([]byte("settings: {name: x, base: b, source_date_epoch: 1700000000}\nbuild: {p: {groups: {g: [run true]}}}\n"), nil)
	if err != nil || r.Settings.SourceDateEpoch == nil || *r.Settings.SourceDateEpoch != 1700000000 {
		t.Fatalf("%v %v", r, err)
	}
	_, err = Parse([]byte("settings: {name: x, base: b, env: {SOURCE_DATE_EPOCH: '1'}}\nbuild: {p: {groups: {g: [run true]}}}\n"), nil)
	if err == nil || !strings.Contains(err.Error(), "settings.source_date_epoch") {
		t.Errorf("env: %v", err)
	}
}

func TestSourceDateEpochOff(t *testing.T) {
	r, err := Parse([]byte("settings: {name: x, base: b, source_date_epoch: false}\nbuild: {p: {groups: {g: [run true]}}}\n"), nil)
	if err != nil || !r.Settings.EpochOff || r.Settings.SourceDateEpoch != nil {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err := Parse([]byte("settings: {name: x, base: b, source_date_epoch: soon}\nbuild: {p: {groups: {g: [run true]}}}\n"), nil); err == nil {
		t.Error("source_date_epoch: soon must be rejected")
	}
}

func TestVolatile(t *testing.T) {
	r, err := Parse([]byte("settings: {name: x, base: b}\nbuild: {p: {groups: {g: [run true]}}}\n"), nil)
	if err != nil || strings.Join(r.Settings.Volatile, " ") != "/run /tmp /var/cache /var/log /var/tmp" {
		t.Fatalf("default: %v %v", r.Settings.Volatile, err)
	}
	r, err = Parse([]byte("settings: {name: x, base: b, volatile: []}\nbuild: {p: {groups: {g: [run true]}}}\n"), nil)
	if err != nil || len(r.Settings.Volatile) != 0 {
		t.Fatalf("empty: %v %v", r.Settings.Volatile, err)
	}
	for _, bad := range []string{"/", "var/log", "/var/../log"} {
		if _, err := Parse([]byte("settings: {name: x, base: b, volatile: ['"+bad+"']}\nbuild: {p: {groups: {g: [run true]}}}\n"), nil); err == nil {
			t.Errorf("volatile %q must be rejected", bad)
		}
	}
}
