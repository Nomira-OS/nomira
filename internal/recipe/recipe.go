// Package recipe defines the nomira recipe schema and its parser.
//
// Shape:
//
//	# syntax=ghcr.io/nomira-os/nomira:1-alpha
//	settings:
//	  name: mysystem
//	  base: ghcr.io/…/arch-bootc@sha256:…
//	prepare:                        # optional named stages (multistage)
//	  aurbuild:
//	    from: docker.io/library/archlinux:base-devel
//	    commands:
//	      - run git clone … /src
//	phases:                         # ordered
//	  - name: packages              # a collector: one command, fed by groups
//	    collect: run pacman -S --needed --noconfirm ${items}
//	  - name: features              # groups within a phase are independent
//	    groups:
//	      network:                  # short form: a list of commands
//	        - copy files/nm/ /etc/NetworkManager/
//	      terminal:                 # long form
//	        packages: [kmscon]      # contribution to the phase "packages"
//	        expects: [/usr/lib/systemd/system/kmsconvt@.service]
//	        prepare:                # stages local to this group
//	          theme: {from: …, commands: […]}
//	        commands:
//	          - copy --from=theme /out/theme.conf /etc/kmscon/theme.conf
//	      editor: {include: features/editor.yml}
//
// Design principle: ordered things are YAML lists, independent things are
// YAML mappings (key = name, order meaningless).
package recipe

import (
	"fmt"
	"path"
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
const DefaultToolbox = "docker.io/library/busybox:1.38.0"

// Recipe is the fully parsed and schema-validated build description.
type Recipe struct {
	Settings Settings
	Prepare  map[string]Stage // named multistage stages, referenced by `copy --from=<name>`
	Phases   []Phase
	// Files are the recipe's own files, entry first, then every included
	// file once, as paths relative to the recipe directory.
	Files []string
}

// RecordPhase returns the index of the phase after which the record is
// added, or -1 when settings.record is false. The record comes right
// before the first phase that reads the result of an earlier phase (a
// bind mount or copy from it, or a prepare stage that starts from it):
// such a phase derives something from the image so far, a unified kernel
// image with its digest for instance, which must already cover the
// record. Without such a phase the record comes last. As late as that,
// a change to the recipe only rebuilds what comes after the record.
func (r *Recipe) RecordPhase() int {
	if !r.Settings.Record {
		return -1
	}
	for i := 1; i < len(r.Phases); i++ {
		if r.readsEarlierPhase(i) {
			return i - 1
		}
	}
	return len(r.Phases) - 1
}

func (r *Recipe) readsEarlierPhase(i int) bool {
	earlier := map[string]bool{}
	for _, ph := range r.Phases[:i] {
		earlier[ph.Name] = true
	}
	reads := func(from string) bool {
		if earlier[from] {
			return true
		}
		st, ok := r.Prepare[from]
		return ok && st.FromPhase != ""
	}
	for _, c := range r.Phases[i].Commands() {
		switch c := c.(type) {
		case CopyCmd:
			if c.From != "" && reads(c.From) {
				return true
			}
		case RunCmd:
			for _, m := range c.Mounts {
				if m.Type == "bind" && reads(m.From) {
					return true
				}
			}
		}
	}
	return false
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
	for _, name := range sortedStageNames(r.Prepare) {
		add(r.Prepare[name].Commands)
	}
	for _, ph := range r.Phases {
		add(ph.Commands())
	}
	sort.Strings(paths)
	return paths, all
}

func sortedStageNames(m map[string]Stage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
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
	// Compression and Secrets are for the caller, too: layer compression
	// of the exported image (zstd, gzip or uncompressed; default zstd), and
	// for each secret id the file on the build machine that provides it.
	// They live in the recipe because they shape the result.
	Compression string
	Secrets     map[string]string
	// SourceDateEpoch is the time, in seconds since 1970, that every run
	// sees as SOURCE_DATE_EPOCH and to which the exported image clamps its
	// timestamps. nil: the creation time of the base image. EpochOff
	// (source_date_epoch: false) turns all of that off.
	SourceDateEpoch *int64
	EpochOff        bool
	// Record says whether the recipe itself goes into the image, below
	// RecordDir (see Recipe.RecordPhase for where).
	Record bool
	// Volatile are the paths that every run gets as an empty tmpfs:
	// nothing written there ends up in the image.
	Volatile []string
}

// DefaultVolatile are the paths of a bootc system that hold no image
// content: they are tmpfs at runtime (/run, /tmp) or machine state in
// /var that bootc never takes from an image after the first install
// (logs, caches, temporary files). Writes there during the build are
// build-time artifacts; bootc container lint warns about them.
var DefaultVolatile = []string{"/run", "/tmp", "/var/cache", "/var/log", "/var/tmp"}

// RecordDir is where the image keeps the recipe it was built from: the
// recipe's files and everything its copy commands take from the build
// context, at their paths relative to the recipe directory. It is below
// /usr, so it is part of the verified, read-only image.
const RecordDir = "/usr/share/nomira/recipe"

// Stage is a named prepare stage with its own base image.
type Stage struct {
	From string
	// FromPhase is set when From names a phase: the stage then starts from
	// the system as it is after that phase, and only phases after it may
	// use the stage.
	FromPhase string
	Commands  []Command
}

// Group is one independent unit of a phase.
type Group struct {
	Commands []Command
	// Expects lists paths that must exist in the state the phase starts
	// from. They are checked on every build without invalidating the
	// group's cache.
	Expects []string
	// Contribute maps a collector phase name to the items this group adds
	// to it.
	Contribute map[string][]string
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

// Phase is one serial pipeline step. It either contains independent groups
// or is a collector: a single command that runs once with the items all
// groups of the recipe contributed under the phase's name.
type Phase struct {
	Name       string
	Validation string            // empty = inherit file default
	Shared     []string          // allowlist: exact paths, or prefixes when ending in "/"
	Env        map[string]string // build-time environment for the phase's runs
	Groups     map[string]Group

	// Collector phases only.
	Collect *RunCmd  // the command with ${items} filled in
	Items   []string // sorted, unique contributions
}

// Commands returns all commands of the phase: the collector's, or those
// of its groups in the order of their names.
func (p Phase) Commands() []Command {
	var cmds []Command
	if p.Collect != nil {
		cmds = append(cmds, *p.Collect)
	}
	for _, g := range p.GroupNames() {
		cmds = append(cmds, p.Groups[g].Commands...)
	}
	return cmds
}

// IsCollector reports whether the phase is a collector.
func (p Phase) IsCollector() bool { return p.Collect != nil }

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

// BuildGroupNames returns the sorted names of the groups that have
// commands. Groups that only contribute to collectors build nothing.
func (p Phase) BuildGroupNames() []string {
	var names []string
	for _, n := range p.GroupNames() {
		if len(p.Groups[n].Commands) > 0 {
			names = append(names, n)
		}
	}
	return names
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
	From    string // bind only: a prepare stage or an earlier phase
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
