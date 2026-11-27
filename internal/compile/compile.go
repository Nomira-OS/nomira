// Package compile turns a parsed recipe into LLB states.
//
// Phases are serial. The groups of a phase fan out and their results are
// merged back (MergeOp). A group contributes one layer:
//
//   - a group with run is executed on the phase's input state S; its layer
//     is DiffOp(S, result);
//   - a group without run never sees S, so it is built on an empty state
//     and its result is the layer. Its cache then depends on neither the
//     base image nor earlier phases. This is only done when Hooks.Decide
//     confirms that merging gives the same tree as building on S.
//
// A phase with a single group that runs on S skips diff and merge. A
// collector is a group like any other, with one run: the collected items.
package compile

import (
	"fmt"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/moby/buildkit/client/llb"

	"github.com/Nomira-OS/nomira/internal/recipe"
)

const (
	// LocalContext is the name of the build-context local source.
	LocalContext = "context"
	defaultPath  = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
)

// PhasePlan is one compiled phase.
type PhasePlan struct {
	Phase recipe.Phase
	// Input is the state the phase starts from (S).
	Input llb.State
	// Uppers maps group name → the group's result: S plus the group's
	// changes, or for an isolated group only its own files.
	Uppers map[string]llb.State
	// Isolated marks the groups that were built on an empty state.
	Isolated map[string]bool
	// Layers maps group name → what the group adds to Input: the diff of a
	// group with run, or the whole result of an isolated group. Empty for a
	// phase that is not merged.
	Layers map[string]llb.State
	// Output is the phase result.
	Output llb.State
	// Validate reports whether this phase needs write-set validation.
	Validate bool
}

// Build is the compiled pipeline.
type Build struct {
	Recipe *recipe.Recipe
	// Stages maps stage name → its final state.
	Stages map[string]llb.State
	// Phases in pipeline order.
	Phases []PhasePlan
	// Final is the last phase's output.
	Final llb.State
}

// Hooks let the caller look at real build results while the graph is
// assembled. Both are optional.
type Hooks struct {
	// Decide is asked once per phase which of its groups without run may
	// not be built on an empty state. lower is the phase input, alone maps
	// each such group to its result on an empty state. The answer maps the
	// rejected groups to a reason in a few words, which is shown next to
	// the group's steps. Without Decide all of them are accepted. One call
	// per phase lets the caller check the groups in parallel.
	Decide func(ph recipe.Phase, lower llb.State, alone map[string]llb.State) (rejected map[string]string, err error)
	// PhaseDone is called with each finished plan before the next phase is
	// compiled on top of it. An error stops the compilation, so a conflict
	// is reported before anything builds on the merged result.
	PhaseDone func(plan PhasePlan) error
	// Resolve solves the resolve step of a collector that fans out and
	// returns the file FanoutList in st. Required for such collectors.
	Resolve func(st llb.State) ([]byte, error)
	// CheckBuckets is shown the buckets of a collector that fans out,
	// by name, before then runs on them; an error stops the build.
	// Optional: without it the buckets are not checked.
	CheckBuckets func(scope string, buckets map[string]llb.State) error
}

// ContextExcludes are never sent from the recipe directory.
var ContextExcludes = []string{".nomira"}

// Compile builds the full LLB graph for a recipe.
func Compile(r *recipe.Recipe, hooks Hooks) (*Build, error) {
	b := &Build{Recipe: r, Stages: map[string]llb.State{}}

	// Only what the copy commands name is loaded from the build context.
	// .nomira holds the CLI's build output next to the recipe: images of
	// a gigabyte and more that no recipe copies from.
	ctxPaths, ctxAll := r.ContextPaths()
	ctxOpts := []llb.LocalOption{
		llb.WithCustomName("[load] context: " + describePaths(ctxPaths, ctxAll)),
		llb.SharedKeyHint(LocalContext),
		llb.ExcludePatterns(ContextExcludes),
	}
	if !ctxAll {
		ctxOpts = append(ctxOpts, llb.IncludePatterns(ctxPaths))
	}
	ctxSrc := llb.Local(LocalContext, ctxOpts...)

	mounts := r.Settings.Mounts

	// Stages. BuildKit builds a stage when something uses it; the order
	// here does not matter.
	for name, st := range r.Stages {
		state, err := applyCommands(baseState(st.From, "stage "+name), st.Commands, mergeEnv(r.Settings.Env), ctxSrc, b.Stages, "stage "+name, mounts)
		if err != nil {
			return nil, err
		}
		b.Stages[name] = state
	}

	// What copy --from= and bind mounts can name: stages, and the result of
	// every phase once it is done.
	sources := map[string]llb.State{}
	for name, st := range b.Stages {
		sources[name] = st
	}

	// Phases.
	cur := baseState(r.Settings.Base, "base")
	for pi, ph := range r.Phases {
		tag := PhaseTag(r, pi)
		plan := PhasePlan{
			Phase:    ph,
			Input:    cur,
			Uppers:   map[string]llb.State{},
			Isolated: map[string]bool{},
			Layers:   map[string]llb.State{},
		}
		env := mergeEnv(r.Settings.Env, ph.Env)

		names := ph.BuildGroupNames()
		// Groups without run, built on an empty state.
		alone := map[string]llb.State{}
		for _, g := range names {
			if grp := ph.Groups[g]; !grp.HasRun() {
				st, err := applyCommands(llb.Scratch(), grp.Commands, mergeEnv(env), ctxSrc, sources, tag+"/"+g, mounts)
				if err != nil {
					return nil, err
				}
				alone[g] = st
			}
		}
		var rejected map[string]string
		if hooks.Decide != nil && len(alone) > 0 {
			var err error
			if rejected, err = hooks.Decide(ph, cur, alone); err != nil {
				return nil, err
			}
		}
		for _, g := range names {
			scope := tag + "/" + g
			if st, ok := alone[g]; ok {
				reason, no := rejected[g]
				if !no {
					plan.Uppers[g] = st
					plan.Isolated[g] = true
					continue
				}
				if reason != "" {
					// Visible in the progress output of every build.
					scope += ", WARNING: not independent of earlier state: " + reason
				}
			}
			if f := ph.Groups[g].Fanout; f != nil {
				upper, err := fanout(cur, f, env, sources, scope, mounts, hooks)
				if err != nil {
					return nil, err
				}
				plan.Uppers[g] = upper
				continue
			}
			upper, err := applyCommands(cur, ph.Groups[g].Commands, mergeEnv(env), ctxSrc, sources, scope, mounts)
			if err != nil {
				return nil, err
			}
			plan.Uppers[g] = upper
		}
		switch {
		case len(names) == 0:
			plan.Output = cur
		case len(names) == 1 && !plan.Isolated[names[0]]:
			// Serial case: no diff, no merge, no validation.
			plan.Output = plan.Uppers[names[0]]
		default:
			plan.Validate = len(names) > 1
			merge := []llb.State{cur}
			for _, g := range names {
				layer := plan.Uppers[g]
				if !plan.Isolated[g] {
					layer = llb.Diff(cur, layer,
						llb.WithCustomNamef("[%s/%s] diff", tag, g))
				}
				plan.Layers[g] = layer
				merge = append(merge, layer)
			}
			plan.Output = llb.Merge(merge,
				llb.WithCustomNamef("[%s] merge %d groups", tag, len(names)))
		}
		if hooks.PhaseDone != nil {
			if err := hooks.PhaseDone(plan); err != nil {
				return nil, err
			}
		}
		cur = plan.Output
		sources[ph.Name] = cur
		b.Phases = append(b.Phases, plan)
	}
	b.Final = cur
	return b, nil
}

// describePaths names what is loaded from the build context.
func describePaths(paths []string, all bool) string {
	switch {
	case all:
		return "build context"
	case len(paths) == 0:
		return "build context (nothing)"
	}
	return strings.Join(paths, " ")
}

// ResolveMode is how images are looked up: the client's
// image-resolve-mode (default, pull, local).
var ResolveMode = llb.ResolveModeDefault

func baseState(ref, name string) llb.State {
	return llb.Image(ref, ResolveMode, llb.WithCustomNamef("[%s] %s", name, ref)).
		AddEnv("PATH", defaultPath)
}

// mergeEnv returns a fresh map; later maps win.
func mergeEnv(maps ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// applyCommands folds a group's commands over a state. env is the group's
// build-time environment (settings, then phase); `env` commands extend it
// for the commands that follow. It is passed to each run rather than stored
// in the state, so it never leaks into other groups, later phases or the
// image configuration.
//
// Every run gets the mounts of settings.mounts (defaults), unless it
// mounts something at the same target itself.
func applyCommands(st llb.State, cmds []recipe.Command, env map[string]string, ctxSrc llb.State, stages map[string]llb.State, scope string, defaults []recipe.Mount) (llb.State, error) {
	for _, c := range cmds {
		switch cmd := c.(type) {
		case recipe.EnvCmd:
			env[cmd.Name] = cmd.Value

		case recipe.RunCmd:
			opts, err := runOptions(cmd, env, stages, scope, defaults)
			if err != nil {
				return st, err
			}
			st = st.Run(opts...).Root()

		case recipe.CopyCmd:
			src := ctxSrc
			if cmd.From != "" {
				s, ok := stages[cmd.From]
				if !ok {
					return st, fmt.Errorf("%s: copy --from=%s: unknown stage", scope, cmd.From)
				}
				src = s
			}
			for _, sp := range cmd.Srcs {
				info := &llb.CopyInfo{
					CreateDestPath:      true,
					AllowWildcard:       true,
					AllowEmptyWildcard:  false,
					CopyDirContentsOnly: strings.HasSuffix(sp, "/"),
					FollowSymlinks:      false,
				}
				if cmd.Chmod != nil {
					mode := os.FileMode(*cmd.Chmod)
					info.Mode = &llb.ChmodOpt{Mode: mode}
				}
				if cmd.Chown != "" {
					var uid, gid int
					fmt.Sscanf(cmd.Chown, "%d:%d", &uid, &gid)
					info.ChownOpt = &llb.ChownOpt{
						User:  &llb.UserOpt{UID: uid},
						Group: &llb.UserOpt{UID: gid},
					}
				}
				st = st.File(
					llb.Copy(src, sp, cmd.Dst, info),
					llb.WithCustomNamef("[%s] copy %s %s", scope, sp, cmd.Dst),
				)
			}
		default:
			return st, fmt.Errorf("%s: unsupported command type %T", scope, c)
		}
	}
	return st, nil
}

// runOptions are the options of one run: the command, its mounts (and
// those of settings.mounts it does not mount itself), its environment.
func runOptions(cmd recipe.RunCmd, env map[string]string, stages map[string]llb.State, scope string, defaults []recipe.Mount) ([]llb.RunOption, error) {
	opts := []llb.RunOption{
		llb.Args([]string{"/bin/sh", "-c", cmd.Script}),
		llb.Dir("/"),
		llb.WithCustomNamef("[%s] run %s", scope, firstLine(cmd.Script)),
	}
	own := map[string]bool{}
	for _, m := range cmd.Mounts {
		own[path.Clean(m.Target)] = true
	}
	var all []recipe.Mount
	for _, m := range defaults {
		if !own[m.Target] {
			all = append(all, m)
		}
	}
	all = append(all, cmd.Mounts...)
	for _, m := range all {
		if m.Type == "tmpfs" {
			opts = append(opts, llb.AddMount(m.Target, llb.Scratch(), llb.Tmpfs()))
			continue
		}
		if m.Type == "secret" {
			// Provided by the client (buildctl --secret); never part
			// of a layer or of the cache key.
			sopts := []llb.SecretOption{llb.SecretID(m.ID)}
			if !m.Required {
				sopts = append(sopts, llb.SecretOptional)
			}
			opts = append(opts, llb.AddSecret(m.Target, sopts...))
			continue
		}
		if m.Type == "bind" {
			src, ok := stages[m.From]
			if !ok {
				return nil, fmt.Errorf("%s: --mount from=%s: no such stage or phase", scope, m.From)
			}
			// No content-based cache key: BuildKit would checksum every file
			// of the mounted tree, a whole system for a UKI, on every build
			// in which it changed. Its LLB definition identifies it as well.
			opts = append(opts, llb.AddMount(m.Target, src, llb.SourcePath(m.Source), llb.Readonly, noContentCache))
			continue
		}
		sharing := llb.CacheMountShared
		switch m.Sharing {
		case "private":
			sharing = llb.CacheMountPrivate
		case "locked":
			sharing = llb.CacheMountLocked
		}
		opts = append(opts, llb.AddMount(m.Target, llb.Scratch(),
			llb.AsPersistentCacheDir(m.ID, sharing)))
	}
	names := make([]string, 0, len(env))
	for k := range env {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		opts = append(opts, llb.AddEnv(k, env[k]))
	}
	if cmd.NoCache {
		opts = append(opts, llb.IgnoreCache)
	}
	return opts, nil
}

// PhaseTag names phase i in step names: its position and name, "2/6
// features", so that every line of the build output says how far along
// the build is.
func PhaseTag(r *recipe.Recipe, i int) string {
	return fmt.Sprintf("%d/%d %s", i+1, len(r.Phases), r.Phases[i].Name)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	if len(s) > 72 {
		s = s[:71] + "…"
	}
	return s
}
