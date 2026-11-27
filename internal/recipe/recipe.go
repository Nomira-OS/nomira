// Package recipe defines the nomira recipe schema and its parser.
//
// Shape:
//
//	# syntax=ghcr.io/nomira-os/nomira:latest
//	imports: [base.yml, https://…/arch-base/<commit>/recipes/base.yml]
//	settings:
//	  base: ghcr.io/…/arch-base:sha-…
//	  mounts:                       # for every run, as --mount; key = target
//	    /var/log: type=tmpfs
//	stages:                         # named stages (multi-stage)
//	  aurbuild:
//	    from: docker.io/library/archlinux:base-devel
//	    commands: [run git clone … /src]
//	build:                          # phases, in order; key = phase name
//	  packages:                     # a phase that is one collector
//	    collect: run pacman -S --needed --noconfirm ${items}
//	  features:                     # groups, independent of each other
//	    network:                    # short form: a list of commands
//	      - copy files/nm/ /etc/NetworkManager/
//	    terminal:                   # long form
//	      packages: [kmscon]        # contribution to the collector "packages"
//	      commands:
//	        - copy --from=aurbuild /out/theme.conf /etc/kmscon/theme.conf
//	    locales:                    # a collector among other groups
//	      collect: run locale-gen ${items}
//
// Design principle: ordered things are YAML lists or the keys of build,
// independent things are YAML mappings.
package recipe

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// Recipe is the fully parsed and schema-validated build description.
type Recipe struct {
	Settings Settings
	Stages   map[string]Stage // named stages, for copy --from= and bind mounts
	Phases   []Phase
	// Files are the recipe's own files, entry first, then every imported
	// file once: paths relative to the recipe directory, or URLs.
	Files []string
	// Resolved is the recipe with all imports merged, as YAML: what the
	// image records in its label org.nomira.recipe.
	Resolved []byte
}

// Settings holds file-level configuration. What only a BuildKit client
// can decide (platform, image names, exporter options, where secrets come
// from) is not part of a recipe.
type Settings struct {
	Base   string            // base image ref
	Labels map[string]string // image labels
	Env    map[string]string // build-time environment for every run
	// Mounts are added to every run, unless the run mounts something at
	// the same target itself.
	Mounts []Mount

	// SourceDateEpoch is the time, in seconds since 1970, that every run
	// sees as SOURCE_DATE_EPOCH and to which the exported image clamps its
	// timestamps. nil: the creation time of the base image. EpochOff
	// (source_date_epoch: false) turns all of that off.
	SourceDateEpoch *int64
	EpochOff        bool
	// Record says whether the image carries the resolved recipe in its
	// label RecipeLabel.
	Record bool
}

// RecipeLabel is the image label that holds the resolved recipe
// (Recipe.Resolved).
const RecipeLabel = "org.nomira.recipe"

// FrontendVersionLabel is the image label with the version of the
// frontend that built the image.
const FrontendVersionLabel = "org.nomira.frontend.version"

// Stage is a named stage with its own base image.
type Stage struct {
	From     string
	Commands []Command
}

// Group is one independent unit of a phase.
type Group struct {
	Commands []Command
	// Contribute maps a collector name to the items this group adds to it.
	Contribute map[string][]string
	// Collector groups only: the command template with ${items}, and the
	// sorted, unique items. Commands holds the filled-in command, or
	// nothing when there are no items.
	Collect *RunCmd
	Items   []string
}

// HasRun reports whether the group executes anything. A group without run
// never sees the state before it, so its result cannot depend on that
// state: the compiler builds it on an empty state when that is provably
// equivalent (see compile.Hooks.Decide).
func (g Group) HasRun() bool {
	for _, c := range g.Commands {
		if _, ok := c.(RunCmd); ok {
			return true
		}
	}
	return false
}

// IsCollector reports whether the group is a collector.
func (g Group) IsCollector() bool { return g.Collect != nil }

// Phase is one serial pipeline step: groups that are independent of each
// other, any of them may be a collector.
type Phase struct {
	Name   string
	Env    map[string]string // build-time environment for the phase's runs
	Groups map[string]Group
}

// Commands returns all commands of the phase, group by group in the order
// of their names.
func (p Phase) Commands() []Command {
	var cmds []Command
	for _, g := range p.GroupNames() {
		cmds = append(cmds, p.Groups[g].Commands...)
	}
	return cmds
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

// BuildGroupNames returns the sorted names of the groups that have
// commands. Groups that only contribute to collectors build nothing, and
// neither does a collector without items.
func (p Phase) BuildGroupNames() []string {
	var names []string
	for _, n := range p.GroupNames() {
		if len(p.Groups[n].Commands) > 0 {
			names = append(names, n)
		}
	}
	return names
}

// SecretIDs returns the ids of the secrets the recipe mounts, each with
// whether a step requires it.
func (r *Recipe) SecretIDs() map[string]bool {
	ids := map[string]bool{}
	add := func(cmds []Command) {
		for _, c := range cmds {
			run, ok := c.(RunCmd)
			if !ok {
				continue
			}
			for _, m := range run.Mounts {
				if m.Type == "secret" {
					ids[m.ID] = ids[m.ID] || m.Required
				}
			}
		}
	}
	for _, st := range r.Stages {
		add(st.Commands)
	}
	for _, ph := range r.Phases {
		add(ph.Commands())
	}
	return ids
}

// ContextPaths returns what the copy commands of the recipe take from the
// build context, as include patterns relative to it. all is true when a
// copy takes the whole context.
func (r *Recipe) ContextPaths() (paths []string, all bool) {
	seen := map[string]bool{}
	add := func(cmds []Command) {
		for _, c := range cmds {
			cc, ok := c.(CopyCmd)
			if !ok || cc.From != "" {
				continue
			}
			for _, src := range cc.Srcs {
				p := path.Clean("/" + src)[1:]
				if p == "" {
					all = true
					continue
				}
				if !seen[p] {
					seen[p] = true
					paths = append(paths, p)
				}
			}
		}
	}
	names := make([]string, 0, len(r.Stages))
	for k := range r.Stages {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, name := range names {
		add(r.Stages[name].Commands)
	}
	for _, ph := range r.Phases {
		add(ph.Commands())
	}
	sort.Strings(paths)
	return paths, all
}

// ---- commands -------------------------------------------------------------

// Command is one parsed recipe command (verb-first string syntax).
type Command interface {
	isCommand()
	// Raw returns the original command string (for progress names).
	Raw() string
}

// Mount is a `--mount=type=...` flag on a run command. Nothing written
// below a mount ends up in the layer.
type Mount struct {
	Type    string // cache | tmpfs | bind | secret
	Target  string
	ID      string // cache only; defaults to Target when empty
	Sharing string // cache only: shared | private | locked (default shared)
	From    string // bind only: a stage or an earlier phase
	Source  string // bind only: path inside From (default /)
	// secret only: Target defaults to /run/secrets/<ID>; Required makes
	// the build fail when the client does not provide the secret.
	Required bool
}

// RunCmd executes a shell script via `/bin/sh -c`.
type RunCmd struct {
	Mounts  []Mount
	NoCache bool // always execute, even if BuildKit has the step cached
	Script  string
	raw     string
}

func (RunCmd) isCommand()    {}
func (c RunCmd) Raw() string { return c.raw }

// EnvCmd sets a build-time environment variable for the commands that
// follow it in the same group or stage. Unlike settings.env it only
// reaches those commands, so changing it reruns only them.
type EnvCmd struct {
	Name, Value string
	raw         string
}

func (EnvCmd) isCommand()    {}
func (c EnvCmd) Raw() string { return c.raw }

// CopyCmd copies from the build context, a stage or an earlier phase into
// the image.
type CopyCmd struct {
	From  string // "" = build context, otherwise a stage or phase name
	Chmod *uint32
	Chown string // "uid:gid" numeric
	Srcs  []string
	Dst   string
	raw   string
}

func (CopyCmd) isCommand()    {}
func (c CopyCmd) Raw() string { return c.raw }

func errf(format string, a ...any) error { return fmt.Errorf(format, a...) }

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func joinSorted(m map[string]bool) string { return strings.Join(sortedKeys(m), ", ") }
