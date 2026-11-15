package compile

import (
	_ "embed"

	"github.com/moby/buildkit/client/llb"
)

// WritesPath is where the lister script leaves a group's write set inside
// the lister state.
const WritesPath = "/nomira/out/writes"

// listerScript (see lister.sh) compares lower (S) and upper (Rᵢ) trees
// mounted read-only at /nomira/l and /nomira/u and writes the group's
// write set to WritesPath. Embedded so hack/test_lister.sh can exercise the
// exact same script against synthetic trees.
//
//go:embed lister.sh
var listerScript string

// WriteSetState builds the lister exec for one group: toolbox root, lower
// and upper mounted read-only. Solve it with Evaluate and ReadFile
// WritesPath to obtain the group's write set.
func WriteSetState(toolbox string, lower, upper llb.State, phase, group string) llb.State {
	tb := llb.Image(toolbox, llb.WithCustomNamef("[validate] toolbox %s", toolbox)).
		AddEnv("PATH", defaultPath)
	run := tb.Run(
		llb.Args([]string{"/bin/sh", "-c", listerScript}),
		llb.Dir("/"),
		llb.AddMount("/nomira/l", lower, llb.Readonly),
		llb.AddMount("/nomira/u", upper, llb.Readonly),
		llb.WithCustomNamef("[%s/%s] collect write set", phase, group),
	)
	return run.Root()
}
