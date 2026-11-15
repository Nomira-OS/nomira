// Package compile turns a parsed recipe into LLB states:
// phases serial, groups fanned out on the phase's input state, diffed
// (DiffOp) and merged back (MergeOp). Single-group phases skip diff/merge
// and validation entirely — there is nothing to compare, and the big serial
// phases (pacman) stay cheap that way.
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
	// Uppers maps group name → state after running the group on Input (Rᵢ).
	Uppers map[string]llb.State
	// Diffs maps group name → DiffOp(Input, Uppers[g]) (Δᵢ); nil-equivalent
	// (absent) for single-group phases.
	Diffs map[string]llb.State
	// Output is the phase result: Merge(Input, Δ…) or, for a single group,
	// that group's upper directly.
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

// Compile builds the full LLB graph for a recipe.
func Compile(r *recipe.Recipe) (*Build, error) {
	b := &Build{Recipe: r, Stages: map[string]llb.State{}}

	ctxSrc := llb.Local(LocalContext,
		llb.WithCustomName("[context] load build context"),
		llb.SharedKeyHint(LocalContext),
	)

	// Prepare stages.
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
			Phase:  ph,
			Input:  cur,
			Uppers: map[string]llb.State{},
			Diffs:  map[string]llb.State{},
		}
		names := ph.GroupNames()
		for _, g := range names {
			upper, err := applyCommands(cur, ph.Groups[g], mergeEnv(r.Settings.Env, ph.Env), ctxSrc, b.Stages, fmt.Sprintf("%s/%s", ph.Name, g))
			if err != nil {
				return nil, err
			}
			plan.Uppers[g] = upper
		}
		if len(names) == 1 {
			// Serial case: no diff, no merge, no validation.
			plan.Output = plan.Uppers[names[0]]
		} else {
			plan.Validate = true
			merge := []llb.State{cur}
			for _, g := range names {
				d := llb.Diff(cur, plan.Uppers[g],
					llb.WithCustomNamef("[%s/%s] diff", ph.Name, g))
				plan.Diffs[g] = d
				merge = append(merge, d)
			}
			plan.Output = llb.Merge(merge,
				llb.WithCustomNamef("[%s] merge %d groups", ph.Name, len(names)))
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
