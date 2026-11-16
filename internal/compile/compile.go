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
// collector phase is one run on S with the collected items.
package compile

import (
	"fmt"
	"os"
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
	// Output is the phase result.
	Output llb.State
	// Validate reports whether this phase needs write-set validation.
	Validate bool
}

// Build is the compiled pipeline.
type Build struct {
	Recipe *recipe.Recipe
	// Stages maps prepare-stage name → its final state.
	Stages map[string]llb.State
	// Phases in pipeline order.
	Phases []PhasePlan
	// Final is the last phase's output.
	Final llb.State
}

// Hooks let the caller look at real build results while the graph is
// assembled. Both are optional.
type Hooks struct {
	// Decide is asked for every group without run whether it may be built
	// on an empty state. lower is the phase input, upper the group built on
	// an empty state. Without Decide the answer is yes.
	Decide func(ph recipe.Phase, group string, lower, upper llb.State) (bool, error)
	// PhaseDone is called with each finished plan before the next phase is
	// compiled on top of it. An error stops the compilation, so a conflict
	// is reported before anything builds on the merged result.
	PhaseDone func(plan PhasePlan) error
}

// Compile builds the full LLB graph for a recipe.
func Compile(r *recipe.Recipe, hooks Hooks) (*Build, error) {
	b := &Build{Recipe: r, Stages: map[string]llb.State{}}

	ctxSrc := llb.Local(LocalContext,
		llb.WithCustomName("[context] load build context"),
		llb.SharedKeyHint(LocalContext),
	)

	// Prepare stages, global and group-local alike. BuildKit builds a stage
	// when something copies from it; the order here does not matter.
	for name, st := range r.Prepare {
		state := baseState(st.From, fmt.Sprintf("prepare %s", name))
		var err error
		state, err = applyCommands(state, st.Commands, mergeEnv(r.Settings.Env), ctxSrc, b.Stages, fmt.Sprintf("prepare %s", name))
		if err != nil {
			return nil, err
		}
		b.Stages[name] = state
	}

	// Phases.
	cur := baseState(r.Settings.Base, "base")
	for _, ph := range r.Phases {
		plan := PhasePlan{
			Phase:    ph,
			Input:    cur,
			Uppers:   map[string]llb.State{},
			Isolated: map[string]bool{},
		}
		env := mergeEnv(r.Settings.Env, ph.Env)

		if ph.IsCollector() {
			plan.Output = cur
			if len(ph.Items) > 0 {
				out, err := applyCommands(cur, []recipe.Command{*ph.Collect}, env, ctxSrc, b.Stages, "collect "+ph.Name)
				if err != nil {
					return nil, err
				}
				plan.Output = out
			}
		} else {
			names := ph.BuildGroupNames()
			for _, g := range names {
				scope := fmt.Sprintf("%s/%s", ph.Name, g)
				grp := ph.Groups[g]
				if !grp.HasRun() {
					alone, err := applyCommands(llb.Scratch(), grp.Commands, mergeEnv(env), ctxSrc, b.Stages, scope)
					if err != nil {
						return nil, err
					}
					ok := true
					if hooks.Decide != nil {
						if ok, err = hooks.Decide(ph, g, cur, alone); err != nil {
							return nil, err
						}
					}
					if ok {
						plan.Uppers[g] = alone
						plan.Isolated[g] = true
						continue
					}
				}
				upper, err := applyCommands(cur, grp.Commands, mergeEnv(env), ctxSrc, b.Stages, scope)
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
							llb.WithCustomNamef("[%s/%s] diff", ph.Name, g))
					}
					merge = append(merge, layer)
				}
				plan.Output = llb.Merge(merge,
					llb.WithCustomNamef("[%s] merge %d groups", ph.Name, len(names)))
			}
		}
		if hooks.PhaseDone != nil {
			if err := hooks.PhaseDone(plan); err != nil {
				return nil, err
			}
		}
		cur = plan.Output
		b.Phases = append(b.Phases, plan)
	}
	b.Final = cur
	return b, nil
}

func baseState(ref, name string) llb.State {
	return llb.Image(ref, llb.WithCustomNamef("[%s] from %s", name, ref)).
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
func applyCommands(st llb.State, cmds []recipe.Command, env map[string]string, ctxSrc llb.State, stages map[string]llb.State, scope string) (llb.State, error) {
	for _, c := range cmds {
		switch cmd := c.(type) {
		case recipe.EnvCmd:
			env[cmd.Name] = cmd.Value

		case recipe.RunCmd:
			opts := []llb.RunOption{
				llb.Args([]string{"/bin/sh", "-c", cmd.Script}),
				llb.Dir("/"),
				llb.WithCustomNamef("[%s] run %s", scope, firstLine(cmd.Script)),
			}
			for _, m := range cmd.Mounts {
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
					llb.WithCustomNamef("[%s] copy %s -> %s", scope, sp, cmd.Dst),
				)
			}
		default:
			return st, fmt.Errorf("%s: unsupported command type %T", scope, c)
		}
	}
	return st, nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	if len(s) > 60 {
		s = s[:57] + "…"
	}
	return s
}
