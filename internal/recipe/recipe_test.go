package recipe

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sample = `
settings:
  base: ghcr.io/example/arch-bootc:sha-1234567
  mounts:
    /var/cache/pacman/pkg: type=cache,sharing=locked
    /var/log: type=tmpfs

stages:
  aurbuild:
    from: docker.io/library/archlinux:base-devel
    commands:
      - run pacman -Syu --noconfirm git

build:
  base:
    system:
      - run pacman -Syu --needed --noconfirm kmscon
  configs:
    kmscon:
      - copy --chmod=0644 files/kmscon/ /etc/kmscon/
    aur:
      - copy --from=aurbuild /out/ /var/tmp/aur/
    boot:
      - env SNAPSHOT=2026/11/10
      - |
        run ./gen.sh \
          --snapshot ${SNAPSHOT} --out /usr/lib/bootc/kargs.d/
`

func parse(t *testing.T, src string) *Recipe {
	t.Helper()
	r, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestParseSample(t *testing.T) {
	r := parse(t, sample)
	if len(r.Phases) != 2 || len(r.Phases[1].Groups) != 3 {
		t.Fatalf("unexpected structure: %+v", r.Phases)
	}
	want := []Mount{
		{Type: "cache", Target: "/var/cache/pacman/pkg", ID: "/var/cache/pacman/pkg", Sharing: "locked"},
		{Type: "tmpfs", Target: "/var/log"},
	}
	if len(r.Settings.Mounts) != 2 || r.Settings.Mounts[0] != want[0] || r.Settings.Mounts[1] != want[1] {
		t.Errorf("mounts: %+v", r.Settings.Mounts)
	}
	run := r.Phases[0].Groups["system"].Commands[0].(RunCmd)
	if !strings.HasPrefix(run.Script, "pacman -Syu") {
		t.Errorf("script mangled: %q", run.Script)
	}
	cp := r.Phases[1].Groups["kmscon"].Commands[0].(CopyCmd)
	if cp.Chmod == nil || *cp.Chmod != 0o644 {
		t.Errorf("chmod not parsed: %+v", cp.Chmod)
	}
	boot := r.Phases[1].Groups["boot"].Commands
	if e := boot[0].(EnvCmd); e.Name != "SNAPSHOT" {
		t.Errorf("env: %+v", e)
	}
	// ${…} is left to the shell.
	if !strings.Contains(boot[1].(RunCmd).Script, "--snapshot ${SNAPSHOT}") {
		t.Errorf("script: %q", boot[1].(RunCmd).Script)
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"unknown verb":       "settings: {base: x}\nbuild: {a: {g: ['frobnicate y']}}",
		"unknown copy stage": "settings: {base: x}\nbuild: {a: {g: ['copy --from=nope /a /b']}}",
		"empty build":        "settings: {base: x}\nbuild: {}",
		"missing base":       "settings: {labels: {a: b}}\nbuild: {a: {g: ['run true']}}",
		"unknown key":        "settings: {base: x}\nphasen: []",
		"relative dst":       "settings: {base: x}\nbuild: {a: {g: ['copy a b']}}",
		"duplicate phase":    "settings: {base: x}\nbuild: {a: {g: ['run true']}, a: {g: ['run true']}}",
		"env in a group":     "settings: {base: x}\nbuild: {a: {g: {env: {A: b}, commands: ['run true']}}}",
		"mount relative":     "settings: {base: x, mounts: {var/log: type=tmpfs}}\nbuild: {a: {g: ['run true']}}",
		"mount bind":         "settings: {base: x, mounts: {/x: 'type=bind,from=a'}}\nbuild: {a: {g: ['run true']}}",
		"mount secret":       "settings: {base: x, mounts: {/x: 'type=secret,id=a'}}\nbuild: {a: {g: ['run true']}}",
		"mount target twice": "settings: {base: x, mounts: {/x: 'type=tmpfs,target=/y'}}\nbuild: {a: {g: ['run true']}}",
		"mount not a string": "settings: {base: x, mounts: {/x: {type: tmpfs}}}\nbuild: {a: {g: ['run true']}}",
		"stage key":          "settings: {base: x}\nstages: {s: {from: a, commands: ['run a'], env: {}}}\nbuild: {a: {g: ['run true']}}",
	}
	for name, src := range cases {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestRemovedKeysExplain(t *testing.T) {
	cases := map[string]string{
		"settings: {base: x, compression: zstd}\nbuild: {a: {g: [run true]}}":      "nomira build --compression",
		"settings: {base: x, secrets: {a: b}}\nbuild: {a: {g: [run true]}}":        "--secret",
		"settings: {base: x, name: n}\nbuild: {a: {g: [run true]}}":                "nomira.conf",
		"settings: {base: x, description: d}\nbuild: {a: {g: [run true]}}":         "org.opencontainers.image.description",
		"settings: {base: x, validation: strict}\nbuild: {a: {g: [run true]}}":     "identical writes",
		"settings: {base: x, toolbox: busybox}\nbuild: {a: {g: [run true]}}":       "built in",
		"settings: {base: x, args: {A: b}}\nbuild: {a: {g: [run true]}}":           "env command",
		"settings: {base: x, volatile: [/x]}\nbuild: {a: {g: [run true]}}":         "settings.mounts",
		"settings: {base: x}\nbuild: {a: {groups: {g: [run true]}}}":               "directly under the phase",
		"settings: {base: x}\nbuild: {a: {shared: [/x], g: [run true]}}":           "identical writes",
		"settings: {base: x}\nbuild: {a: {g: {stages: {}, commands: [run true]}}}": "top of the recipe",
		"settings: {base: x}\nphases: []":                                          "keys of build",
	}
	for src, want := range cases {
		_, err := Parse([]byte(src))
		if err == nil || !strings.Contains(err.Error(), "is gone") || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v", src, err)
		}
	}
}

func TestErrorsNameFileAndLine(t *testing.T) {
	files := map[string]string{
		"recipe.yml":  "imports: [parts/f.yml]\nsettings: {base: x}\nbuild:\n  a:\n    g: [run true]\n",
		"parts/f.yml": "build:\n  a:\n    h:\n      packages: [vim]\n",
	}
	_, err := ParseFile("recipe.yml", mapReader(files))
	if err == nil || !strings.Contains(err.Error(), "parts/f.yml:3") || !strings.Contains(err.Error(), `"packages"`) {
		t.Errorf("error: %v", err)
	}
	_, err = Parse([]byte("settings:\n  base: x\n  labels: [a]\nbuild: {a: {g: [run true]}}\n"))
	if err == nil || !strings.Contains(err.Error(), "recipe.yml:3") {
		t.Errorf("error: %v", err)
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
  labels: {owner: me}
build:
  features:
    editor: [run install-editor]
    network: null
`,
		"base.yml": `imports: [common.yml]
settings:
  base: x
  labels: {tier: base}
  mounts: {/tmp: type=tmpfs}
stages:
  aur: {from: y, commands: [run make]}
build:
  packages:
    collect: run pacman -S ${items}
  features:
    network: {packages: [networkmanager], commands: [run nm]}
    ssh: {packages: [openssh]}
  services:
    collect: run systemctl enable ${items}
`,
		"common.yml": "settings: {labels: {common: 'yes'}}\n",
		"machine/laptop.yml": `settings:
  mounts: {/var/log: type=tmpfs}
build:
  firmware:
    collect: run pacman -S ${items}
  packages: {}
  features:
    hardware: {firmware: [linux-firmware-intel]}
  uki:
    uki: [run make-uki]
`,
	}
	r, err := ParseFile("recipe.yml", mapReader(files))
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
	if st.Base != "x" || st.Labels["owner"] != "me" || st.Labels["tier"] != "base" || st.Labels["common"] != "yes" {
		t.Errorf("settings: %+v", st)
	}
	if len(st.Mounts) != 2 {
		t.Errorf("mounts are merged by target: %+v", st.Mounts)
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
	again, err := Parse(r.Resolved)
	if err != nil {
		t.Fatalf("resolved does not parse: %v\n%s", err, res)
	}
	if len(again.Phases) != len(r.Phases) {
		t.Errorf("resolved parses differently")
	}
}

func TestImportErrors(t *testing.T) {
	head := "settings: {base: x}\nbuild: {a: {g: [run true]}}\n"
	cases := map[string]map[string]string{
		"cycle":       {"recipe.yml": "imports: [a.yml]\n" + head, "a.yml": "imports: [recipe.yml]\n"},
		"escape":      {"recipe.yml": "imports: [../x.yml]\n" + head},
		"absolute":    {"recipe.yml": "imports: [/etc/x.yml]\n" + head},
		"http":        {"recipe.yml": "imports: ['http://example.org/a.yml']\n" + head},
		"http rel":    {"recipe.yml": "imports: ['http://example.org/a.yml#sha256=" + strings.Repeat("0", 64) + "']\n" + head, "http://example.org/a.yml#sha256=" + strings.Repeat("0", 64): "imports: [b.yml]\n"},
		"bad pin":     {"recipe.yml": "imports: ['https://example.org/a.yml#sha256=abc']\n" + head},
		"missing":     {"recipe.yml": "imports: [nope.yml]\n" + head},
		"not a list":  {"recipe.yml": "imports: a.yml\n" + head},
		"unknown key": {"recipe.yml": "imports: [a.yml]\n" + head, "a.yml": "gruppen: {}\n"},
		"order": {
			"recipe.yml": "imports: [a.yml, b.yml]\nsettings: {base: x}\n",
			"a.yml":      "build: {p: {g: [run a]}, q: {g: [run b]}}\n",
			"b.yml":      "build: {q: {}, p: {}}\n",
		},
		"stream": {"recipe.yml": "imports: [a.yml]\n" + head},
	}
	for name, files := range cases {
		read := mapReader(files)
		if name == "stream" {
			if _, err := Parse([]byte(files["recipe.yml"])); err == nil {
				t.Errorf("%s: expected error", name)
			}
			continue
		}
		if _, err := ParseFile("recipe.yml", read); err == nil {
			t.Errorf("%s: expected error", name)
		} else if name == "order" && !strings.Contains(err.Error(), "different order") {
			t.Errorf("order: %v", err)
		}
	}
}

func TestURLImports(t *testing.T) {
	const base = "https://example.org/arch-base/0123abc/recipes/"
	pin := "#sha256=" + strings.Repeat("ab", 32)
	files := map[string]string{
		"recipe.yml":                "imports: ['" + base + "packages.yml" + pin + "', local.yml]\nsettings: {base: x}\nbuild: {features: {g: {packages: [vim]}}}\n",
		base + "packages.yml" + pin: "imports: [mounts.yml]\nbuild: {packages: {collect: 'run pacman -S ${items}'}}\n",
		base + "mounts.yml":         "settings: {mounts: {/var/log: type=tmpfs}}\n",
		"local.yml":                 "settings: {labels: {a: b}}\n",
	}
	r, err := ParseFile("recipe.yml", mapReader(files))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Settings.Mounts) != 1 || r.Phases[0].Groups["packages"].Collect == nil {
		t.Errorf("recipe: %+v", r)
	}
	if !strings.Contains(string(r.Resolved), "from "+base+"mounts.yml") {
		t.Errorf("resolved:\n%s", r.Resolved)
	}
	u, sum, err := SplitPin(base + "packages.yml" + pin)
	if err != nil || u != base+"packages.yml" || sum != strings.Repeat("ab", 32) {
		t.Errorf("SplitPin: %q %q %v", u, sum, err)
	}
}

func TestEnvNoCacheAndMetadata(t *testing.T) {
	src := `
settings:
  base: x
  labels: {org.example.version: "1.2"}
  env: {LANG: C.UTF-8}
build:
  a:
    env: {MAKEFLAGS: -j4}
    g:
      - env GREETING=hello world
      - run --no-cache --mount=type=cache,target=/c make
`
	r := parse(t, src)
	s := r.Settings
	if s.Labels["org.example.version"] != "1.2" || s.Env["LANG"] != "C.UTF-8" {
		t.Errorf("settings: %+v", s)
	}
	if r.Phases[0].Env["MAKEFLAGS"] != "-j4" || len(r.Phases[0].Groups) != 1 {
		t.Errorf("phase: %+v", r.Phases[0])
	}
	cmds := r.Phases[0].Groups["g"].Commands
	if e := cmds[0].(EnvCmd); e.Name != "GREETING" || e.Value != "hello world" {
		t.Errorf("env cmd: %+v", e)
	}
	if run := cmds[1].(RunCmd); !run.NoCache || len(run.Mounts) != 1 || run.Script != "make" {
		t.Errorf("run cmd: %+v", run)
	}

	bad := map[string]string{
		"env name":  "settings: {base: x, env: {'1A': b}}\nbuild: {a: {g: ['run true']}}",
		"env cmd":   "settings: {base: x}\nbuild: {a: {g: ['env NOVALUE']}}",
		"phase env": "settings: {base: x}\nbuild: {a: {env: {'a-b': c}, g: ['run true']}}",
	}
	for name, s := range bad {
		if _, err := Parse([]byte(s)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestCollectors(t *testing.T) {
	files := map[string]string{
		"recipe.yml": `
imports: [features/terminal.yml]
settings: {base: x}
stages:
  theme:
    from: y
    commands: ['run ./gen-theme.sh']
build:
  packages:
    collect: run --mount=type=cache,target=/c pacman -S --needed --noconfirm ${items}
  features:
    editor:
      packages: [vim, kmscon]
      locales: [de_DE.UTF-8]
    network:
      - copy files/nm/ /etc/NetworkManager/
  setup:
    services:
      collect: run systemctl enable ${items}
    locales:
      collect: run locale-gen ${items}
    unused:
      collect: run echo ${items}
    hostname: ['run echo host > /etc/hostname']
`,
		"features/terminal.yml": `
build:
  features:
    terminal:
      packages: [kmscon]
      services: [kmsconvt@tty1.service]
      expects: [/usr/lib/systemd/system/kmsconvt@.service]
      commands:
        - copy files/kmscon/ /etc/kmscon/
        - copy --from=theme /out/theme.conf /etc/kmscon/theme.conf
`,
	}
	r, err := ParseFile("recipe.yml", mapReader(files))
	if err != nil {
		t.Fatal(err)
	}
	pk := r.Phases[0].Groups["packages"]
	if len(r.Phases[0].Groups) != 1 || !pk.IsCollector() || strings.Join(pk.Items, ",") != "kmscon,vim" {
		t.Fatalf("packages collector: %+v", r.Phases[0])
	}
	if run := pk.Commands[0].(RunCmd); run.Script != "pacman -S --needed --noconfirm kmscon vim" || len(run.Mounts) != 1 {
		t.Errorf("collect command: %+v", run)
	}
	setup := r.Phases[2]
	if got := setup.Groups["services"].Commands[0].(RunCmd).Script; got != "systemctl enable kmsconvt@tty1.service" {
		t.Errorf("services: %q", got)
	}
	if got := setup.Groups["locales"].Commands[0].(RunCmd).Script; got != "locale-gen de_DE.UTF-8" {
		t.Errorf("locales: %q", got)
	}
	if g := setup.Groups["unused"]; len(g.Items) != 0 || len(g.Commands) != 0 {
		t.Errorf("collector without contributions: %+v", g)
	}
	if strings.Join(setup.BuildGroupNames(), ",") != "hostname,locales,services" {
		t.Errorf("setup builds: %v", setup.BuildGroupNames())
	}
	feat := r.Phases[1]
	term := feat.Groups["terminal"]
	if term.HasRun() || len(term.Commands) != 2 || term.Expects[0] != "/usr/lib/systemd/system/kmsconvt@.service" {
		t.Errorf("terminal: %+v", term)
	}
	// A group that only contributes builds nothing.
	if strings.Join(feat.BuildGroupNames(), ",") != "network,terminal" || len(feat.GroupNames()) != 3 {
		t.Errorf("build groups: %v", feat.BuildGroupNames())
	}
}

func TestCollectorErrors(t *testing.T) {
	head := "settings: {base: x}\nbuild:\n"
	bad := map[string]string{
		"no placeholder":       head + "  p: {collect: run pacman -S}\n",
		"not a run":            head + "  p: {collect: 'copy ${items} /x'}\n  f: {g: {p: [a]}}\n",
		"collect with groups":  head + "  p: {collect: 'run x ${items}', g: ['run true']}\n",
		"collector and more":   head + "  p: {c: {collect: 'run x ${items}', commands: ['run true']}}\n",
		"same name twice":      head + "  p: {collect: 'run x ${items}'}\n  q: {p: {collect: 'run y ${items}'}}\n",
		"same name in a phase": head + "  p: {c: {collect: 'run x ${items}'}}\n  q: {c: {collect: 'run y ${items}'}}\n",
		"unknown contribution": head + "  f: {g: {pakages: [a]}}\n",
		"item with space":      head + "  p: {collect: 'run x ${items}'}\n  f: {g: {p: ['a b']}}\n",
		"item with shell":      head + "  p: {collect: 'run x ${items}'}\n  f: {g: {p: ['a;rm']}}\n",
		"item is an option":    head + "  p: {collect: 'run x ${items}'}\n  f: {g: {p: ['--now']}}\n",
		"empty group":          head + "  f: {g: {expects: [/a]}}\n",
		"relative expects":     head + "  f: {g: {expects: [a/b], commands: ['run true']}}\n",
		"neither":              head + "  f: {}\n",
	}
	for name, s := range bad {
		if _, err := Parse([]byte(s)); err == nil {
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
    a: ['run true']
  kernel:
    k: ['run rm -f /vmlinuz']
  uki:
    u:
      - run --mount=type=bind,from=kernel,target=/target --mount=type=bind,from=system,source=/usr/lib/modules,target=/kernels --mount=type=bind,from=tools,target=/t ls /target
      - copy --from=system /etc/hostname /etc/hostname
`)
	r, err := ParseFile("recipe.yml", mapReader(good))
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
		"later phase":       "  a:\n    g: ['run --mount=type=bind,from=b,target=/x true']\n  b:\n    g: ['run true']\n",
		"own phase":         "  a:\n    g: ['copy --from=a /x /y']\n",
		"unknown":           "  a:\n    g: ['run --mount=type=bind,from=nope,target=/x true']\n",
		"stage and phase":   "  tools:\n    g: ['run true']\n",
		"bind without from": "  a:\n    g: ['run --mount=type=bind,target=/x true']\n",
	} {
		if _, err := ParseFile("recipe.yml", mapReader(recipe(body))); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A stage starts from an image, not from a phase.
	if _, err := Parse([]byte("settings: {base: x}\nstages: {s: {from: a, commands: ['copy --from=a /x /y']}}\nbuild: {a: {g: ['run true']}}")); err == nil {
		t.Error("stage copying from a phase: accepted")
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
	r := parse(t, "settings: {base: x}\nbuild: {a: {g: ['run --mount=type=secret,id=db.key,required=true --mount=type=secret,id=tok true']}}\n")
	if ids := r.SecretIDs(); len(ids) != 2 || !ids["db.key"] || ids["tok"] {
		t.Errorf("secret ids: %v", ids)
	}
}

func TestContextPathsAndRecord(t *testing.T) {
	src := `
settings: {base: b}
build:
  features:
    a: ["copy files/a.conf /etc/a.conf", "copy files/b/ /etc/b2/"]
    b: ["copy ./files/b/ /etc/b/"]
`
	r := parse(t, src)
	paths, all := r.ContextPaths()
	if all || strings.Join(paths, " ") != "files/a.conf files/b" {
		t.Errorf("context = %v %v", paths, all)
	}
	if !r.Settings.Record {
		t.Error("record is on by default")
	}
	r = parse(t, strings.Replace(src, "base: b", "base: b, record: false", 1))
	if r.Settings.Record {
		t.Error("record: false")
	}
	if _, err := Parse([]byte(strings.Replace(src, "base: b", "base: b, record: features", 1))); err == nil {
		t.Error("record: features must be rejected")
	}
}

func TestSourceDateEpochSetting(t *testing.T) {
	r := parse(t, "settings: {base: b, source_date_epoch: 1700000000}\nbuild: {p: {g: [run true]}}\n")
	if r.Settings.SourceDateEpoch == nil || *r.Settings.SourceDateEpoch != 1700000000 {
		t.Fatalf("%+v", r.Settings)
	}
	_, err := Parse([]byte("settings: {base: b, env: {SOURCE_DATE_EPOCH: '1'}}\nbuild: {p: {g: [run true]}}\n"))
	if err == nil || !strings.Contains(err.Error(), "settings.source_date_epoch") {
		t.Errorf("env: %v", err)
	}
	r = parse(t, "settings: {base: b, source_date_epoch: false}\nbuild: {p: {g: [run true]}}\n")
	if !r.Settings.EpochOff || r.Settings.SourceDateEpoch != nil {
		t.Fatalf("%+v", r.Settings)
	}
	if _, err := Parse([]byte("settings: {base: b, source_date_epoch: soon}\nbuild: {p: {g: [run true]}}\n")); err == nil {
		t.Error("source_date_epoch: soon must be rejected")
	}
}

func TestExamples(t *testing.T) {
	r, err := ParseFile("recipe.yml", func(name string) ([]byte, error) {
		return os.ReadFile(filepath.Join("../../examples", filepath.FromSlash(name)))
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Phases) != 3 || len(r.Phases[1].Groups) != 3 {
		t.Errorf("phases: %+v", r.Phases)
	}
}
