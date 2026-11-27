package compile

import (
	_ "embed"
	"strings"

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

// WriteSetState builds the lister exec for one group (pass 1 of the
// conflict check, see package conflict). Solve it with Evaluate and
// ReadFile WritesPath to obtain the group's write set.
//
// layer is what the group adds: everything in it is a write. lower and
// upper are the trees before and after the group; they are walked to find
// what the group deleted. No file is read. Both are nil for a group that was built on an empty state: it
// cannot delete anything.
//
// phase is the phase's tag (PhaseTag). The mounts get no content-based
// cache key: lower and upper are whole systems, and BuildKit would
// checksum every file of them.
func WriteSetState(toolbox string, lower, upper *llb.State, layer llb.State, phase, group string) llb.State {
	opts := []llb.RunOption{
		llb.Args([]string{"/bin/sh", "-c", listerScript}),
		llb.Dir("/"),
		llb.AddMount("/nomira/d", layer, llb.Readonly, noContentCache),
		llb.WithCustomNamef("[check %s/%s] write set", phase, group),
	}
	if lower != nil && upper != nil {
		opts = append(opts,
			llb.AddMount("/nomira/l", *lower, llb.Readonly, noContentCache),
			llb.AddMount("/nomira/u", *upper, llb.Readonly, noContentCache))
	}
	return toolboxState(toolbox).Run(opts...).Root()
}

// HashesPath is where the hash script leaves its output.
const HashesPath = "/nomira/out/hashes"

// hashScript (see hash.sh) hashes the given paths of a group's layer.
//
//go:embed hash.sh
var hashScript string

// HashState builds pass 2 of the conflict check for one group: it hashes
// the paths of layer that other groups of the phase write too. Solve it
// with Evaluate and ReadFile HashesPath.
func HashState(toolbox string, layer llb.State, paths []string, phase, group string) llb.State {
	list := llb.Scratch().File(llb.Mkfile("/paths", 0o644, []byte(strings.Join(paths, "\n")+"\n")),
		llb.WithCustomNamef("[check %s/%s] paths written by other groups too", phase, group))
	return toolboxState(toolbox).Run(
		llb.Args([]string{"/bin/sh", "-c", hashScript}),
		llb.Dir("/"),
		llb.AddMount("/nomira/d", layer, llb.Readonly, noContentCache),
		llb.AddMount("/nomira/p", list, llb.Readonly),
		llb.WithCustomNamef("[check %s/%s] hash %d shared paths", phase, group, len(paths)),
	).Root()
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
		llb.AddMount("/nomira/l", lower, llb.Readonly, noContentCache),
		llb.AddMount("/nomira/u", upper, llb.Readonly, noContentCache),
		llb.WithCustomNamef("[check %s/%s] independent of earlier phases", phase, group),
	).Root()
}

func toolboxState(toolbox string) llb.State {
	return llb.Image(toolbox, ResolveMode, llb.WithCustomNamef("[check] %s", toolbox)).
		AddEnv("PATH", defaultPath)
}

var noContentCache = llb.ContentCache(llb.MountContentCacheOff)
