package frontend

import (
	"github.com/moby/buildkit/client/llb/sourceresolver"
)

// resolveOpt builds the options for ResolveImageConfig. Kept in its own file
// because this API moved across BuildKit versions (llb.ResolveImageConfigOpt
// → sourceresolver.Opt in v0.13+); adjust here on upgrades.
func resolveOpt() sourceresolver.Opt {
	return sourceresolver.Opt{
		ImageOpt: &sourceresolver.ResolveImageOpt{},
	}
}
