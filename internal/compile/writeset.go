package compile

import (
	_ "embed"

	"github.com/moby/buildkit/client/llb"
)

// WritesPath is where the lister script leaves a group's write set inside
// the lister state.
const WritesPath = "/nomira/out/writes"

// listerScript (see lister.sh) writes a group's write set to WritesPath.
// Embedded so hack/test_lister.sh can exercise the exact same script
// against synthetic trees.
//
//go:embed lister.sh
var listerScript string

// WriteSetState builds the lister exec for one group. Solve it with
// Evaluate and ReadFile WritesPath to obtain the group's write set.
//
// layer is what the group adds: everything in it is a write, and only
// those files are hashed. lower and upper are the trees before and after
// the group; they are only walked, never read, to find what the group
// deleted. Both are nil for a group that was built on an empty state: it
// cannot delete anything.
func WriteSetState(toolbox string, lower, upper *llb.State, layer llb.State, phase, group string) llb.State {
	opts := []llb.RunOption{
		llb.Args([]string{"/bin/sh", "-c", listerScript}),
		llb.Dir("/"),
		llb.AddMount("/nomira/d", layer, llb.Readonly),
		llb.WithCustomNamef("[%s/%s] collect write set", phase, group),
	}
	if lower != nil && upper != nil {
		opts = append(opts,
			llb.AddMount("/nomira/l", *lower, llb.Readonly),
			llb.AddMount("/nomira/u", *upper, llb.Readonly))
	}
	return toolboxState(toolbox).Run(opts...).Root()
}

// GuardPath is where the guard script leaves its findings.
const GuardPath = "/nomira/out/guard"

// guardScript (see guard.sh) reports where merging a group that was built
// on an empty state would differ from building it on the lower tree.
//
//go:embed guard.sh
var guardScript string

// GuardState builds the guard exec for one group without run. Solve it
// with Evaluate and ReadFile GuardPath: an empty file means the group may
// stay isolated. The exec depends on the lower state, so it runs again
// when the base changes, while the group's own layer stays cached.
func GuardState(toolbox string, lower, upper llb.State, phase, group string) llb.State {
	return toolboxState(toolbox).Run(
		llb.Args([]string{"/bin/sh", "-c", guardScript}),
		llb.Dir("/"),
		llb.AddMount("/nomira/l", lower, llb.Readonly),
		llb.AddMount("/nomira/u", upper, llb.Readonly),
		llb.WithCustomNamef("[%s/%s] check independence from earlier state", phase, group),
	).Root()
}

func toolboxState(toolbox string) llb.State {
	return llb.Image(toolbox, llb.WithCustomNamef("[validate] toolbox %s", toolbox)).
		AddEnv("PATH", defaultPath)
}
