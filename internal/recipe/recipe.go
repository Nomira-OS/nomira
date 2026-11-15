// Package recipe defines the nomira recipe schema and its parser.
//
// Shape (v2, "# syntax" era):
//
//	# syntax=ghcr.io/nomira-os/nomira:1-alpha
//	settings:
//	  name: meinsystem
//	  base: ghcr.io/…/arch-bootc@sha256:…
//	  validation: strict            # file default: strict | allow-identical
//	  toolbox: docker.io/library/busybox:1.36.1
//	  args:
//	    SNAPSHOT: "2026/11/10"      # overridable via --build-arg / --opt build-arg:SNAPSHOT
//	prepare:                        # optional named stages (multistage)
//	  aurbuild:
//	    from: docker.io/library/archlinux:base-devel
//	    commands:
//	      - run pacman -Syu --noconfirm git base-devel
//	phases:                         # ordered; groups within a phase are independent
//	  - name: base
//	    groups:
//	      system:
//	        - run --mount=type=cache,target=/var/cache/pacman/pkg pacman -Syu --needed --noconfirm foo
//	  - name: configs
//	    validation: allow-identical
//	    shared: [/usr/lib/tmpfiles.d/common.conf]
//	    groups:
//	      kmscon:
//	        - copy --chmod=0644 files/kmscon/ /etc/kmscon/
//	      aur:
//	        - copy --from=aurbuild /home/build/out/ /var/tmp/aur/
//
// Design principle: ordered things are YAML lists, independent things are
// YAML mappings (key = name, order meaningless).
package recipe

import (
	"fmt"
	"sort"
	"strings"
)

// Validation policies.
const (
	ValidationStrict         = "strict"
	ValidationAllowIdentical = "allow-identical"
)

// DefaultToolbox is the image used for write-set collection execs.
// renovate: datasource=docker
const DefaultToolbox = "docker.io/library/busybox:1.36.1"

// Recipe is the fully parsed and schema-validated build description.
type Recipe struct {
	Settings Settings
	Prepare  map[string]Stage // named multistage stages, referenced by `copy --from=<name>`
	Phases   []Phase
}

// Settings holds file-level configuration.
type Settings struct {
	Name       string
	Base       string            // base image ref (digest pinning recommended)
	Validation string            // file default policy
	Toolbox    string            // image for write-set collection
	Args       map[string]string // declared build args with defaults

	Description string            // becomes the org.opencontainers.image.description label
	Labels      map[string]string // image labels
	Env         map[string]string // build-time environment for every run
	// Platforms and Tags are declarations for the caller (CLI, CI): the
	// frontend validates and reports them via `inspect` but builds for the
	// platform BuildKit asks for and never tags anything itself.
	Platforms []string
	Tags      []string
}

// Stage is a named prepare stage with its own base image.
type Stage struct {
	From     string
	Commands []Command
}

// Phase is one serial pipeline step containing independent groups.
type Phase struct {
	Name       string
	Validation string            // empty = inherit file default
	Shared     []string          // allowlist: exact paths, or prefixes when ending in "/"
	Env        map[string]string // build-time environment for the phase's runs
	Groups     map[string][]Command
}

// Policy returns the effective validation policy for the phase.
func (p Phase) Policy(fileDefault string) string {
	if p.Validation != "" {
		return p.Validation
	}
	if fileDefault != "" {
		return fileDefault
	}
	return ValidationStrict
}

// GroupNames returns the phase's group names sorted for deterministic output.
func (p Phase) GroupNames() []string {
	names := make([]string, 0, len(p.Groups))
	for n := range p.Groups {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ---- commands -------------------------------------------------------------

// Command is one parsed recipe command (verb-first string syntax).
type Command interface {
	isCommand()
	// Raw returns the original command string (for progress names).
	Raw() string
}

// CacheMount is a `--mount=type=cache,...` flag on a run command.
type CacheMount struct {
	Target  string
	ID      string // defaults to Target when empty
	Sharing string // shared | private | locked (default shared)
}

// RunCmd executes a shell script via `/bin/sh -c`.
type RunCmd struct {
	Mounts  []CacheMount
	NoCache bool // always execute, even if BuildKit has the step cached
	Script  string
	raw     string
}

func (RunCmd) isCommand()    {}
func (c RunCmd) Raw() string { return c.raw }

// EnvCmd sets a build-time environment variable for the commands that
// follow it in the same group or stage.
type EnvCmd struct {
	Name, Value string
	raw         string
}

func (EnvCmd) isCommand()    {}
func (c EnvCmd) Raw() string { return c.raw }

// CopyCmd copies from the build context or a prepare stage into the image.
type CopyCmd struct {
	From  string // "" = build context, otherwise prepare stage name
	Chmod *uint32
	Chown string // "uid:gid" numeric
	Srcs  []string
	Dst   string
	raw   string
}

func (CopyCmd) isCommand()    {}
func (c CopyCmd) Raw() string { return c.raw }

// ---- helpers --------------------------------------------------------------

func validPolicy(s string) bool {
	return s == "" || s == ValidationStrict || s == ValidationAllowIdentical
}

// ExpandArgs substitutes ${NAME} for declared args only; everything else
// (e.g. pacman's $repo/$arch) is left untouched. Undeclared ${NAME} with
// uppercase arg-like names stay literal as well — substitution is opt-in by
// declaration.
func ExpandArgs(s string, args map[string]string) string {
	if len(args) == 0 || !strings.Contains(s, "${") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		j := strings.Index(s[i:], "${")
		if j < 0 {
			b.WriteString(s[i:])
			break
		}
		b.WriteString(s[i : i+j])
		rest := s[i+j:]
		k := strings.Index(rest, "}")
		if k < 0 {
			b.WriteString(rest)
			break
		}
		name := rest[2:k]
		if v, ok := args[name]; ok {
			b.WriteString(v)
		} else {
			b.WriteString(rest[:k+1]) // keep literal
		}
		i += j + k + 1
	}
	return b.String()
}

func errf(format string, a ...any) error { return fmt.Errorf(format, a...) }
