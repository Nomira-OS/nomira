// Package frontend wires recipe parsing, LLB compilation and the conflict
// check into a BuildKit gateway frontend.
//
// Besides building, it answers two questions about a recipe through
// BuildKit, selected with the option mode: lint (does it parse?) and
// resolve (what is it with all imports merged?). Both return a small
// tree for --output type=local: recipe.yml (resolve only) and version,
// the version of the frontend.
package frontend

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/exporter/containerimage/exptypes"
	commonexptypes "github.com/moby/buildkit/exporter/exptypes"
	"github.com/moby/buildkit/frontend/gateway/client"
	digest "github.com/opencontainers/go-digest"
	"golang.org/x/sync/errgroup"

	"github.com/Nomira-OS/nomira/internal/compile"
	"github.com/Nomira-OS/nomira/internal/conflict"
	"github.com/Nomira-OS/nomira/internal/recipe"
)

const (
	localDockerfile = "dockerfile" // BuildKit's name for the frontend-file source
	keyFilename     = "filename"
	defaultFilename = "recipe.yml"
	keyResolveMode  = "image-resolve-mode"
	keyMode         = "mode"
	keyToolbox      = "toolbox"
	keyCheck        = "check"

	// DefaultToolbox is the image the checks run in: busybox's find, sort,
	// comm, sed, sha256sum and stat. Tests set another with the option
	// toolbox.
	// renovate: datasource=docker
	DefaultToolbox = "docker.io/library/busybox:1.38.0"
)

// Build is the gateway entrypoint (grpcclient.RunFromEnvironment target).
func Build(ctx context.Context, c client.Client) (*client.Result, error) {
	opts := c.BuildOpts().Opts

	filename := opts[keyFilename]
	if filename == "" {
		filename = defaultFilename
	}
	read := recipeReader(ctx, c)
	// image-resolve-mode, as for the Dockerfile frontend: whether images
	// given by tag are pulled again (pull), taken from the local store if
	// there (local), or the default.
	switch mode := opts[keyResolveMode]; mode {
	case "", "default":
		compile.ResolveMode = llb.ResolveModeDefault
	case "pull":
		compile.ResolveMode = llb.ResolveModeForcePull
	case "local":
		compile.ResolveMode = llb.ResolveModePreferLocal
	default:
		return nil, fmt.Errorf("image-resolve-mode %q: expected default, pull or local", mode)
	}

	toolbox := opts[keyToolbox]
	if toolbox == "" {
		toolbox = DefaultToolbox
	}
	// check=off skips the conflict checks: between the groups of a phase
	// and between the buckets of a collector that fans out.
	check := true
	switch opts[keyCheck] {
	case "", "on":
	case "off":
		check = false
	default:
		return nil, fmt.Errorf("check %q: expected on or off", opts[keyCheck])
	}

	r, err := recipe.ParseFile(filename, read)
	if err != nil {
		return nil, fmt.Errorf("recipe: %w", err)
	}
	switch mode := opts[keyMode]; mode {
	case "", "build":
	case "lint", "resolve":
		return answer(ctx, c, mode, r)
	default:
		return nil, fmt.Errorf("mode %q: expected build, lint or resolve", mode)
	}

	// Carry the base image's config (entrypoint, env, labels …) so the
	// exported image stays bootable; bootc images live or die by it.
	base, err := resolveBaseConfig(ctx, c, r.Settings.Base)
	if err != nil {
		return nil, fmt.Errorf("read image config of base %s: %w", r.Settings.Base, err)
	}
	var epoch *int64
	if !r.Settings.EpochOff {
		e := sourceDateEpoch(r.Settings.SourceDateEpoch, base)
		epoch = &e
		if r.Settings.Env == nil {
			r.Settings.Env = map[string]string{}
		}
		r.Settings.Env[recipe.EpochVar] = strconv.FormatInt(e, 10)
	}

	// The graph is assembled phase by phase, looking at real results on the
	// way: whether a group without run may be built apart from the state
	// before it, and whether the groups of a phase are independent. A problem stops the build before anything is
	// merged on top of it or exported.
	b, err := compile.Compile(r, compile.Hooks{
		Decide: func(ph recipe.Phase, lower llb.State, alone map[string]llb.State) (map[string]string, error) {
			return dependent(ctx, c, r, toolbox, ph, lower, alone)
		},
		PhaseDone: func(plan compile.PhasePlan) error {
			if !plan.Validate || !check {
				return nil
			}
			return checkConflicts(ctx, c, r, toolbox, plan)
		},
		Resolve: func(st llb.State) ([]byte, error) {
			return solveAndRead(ctx, c, st, compile.FanoutList)
		},
		CheckBuckets: func(scope string, buckets map[string]llb.State) error {
			if !check {
				return nil
			}
			return checkBuckets(ctx, c, toolbox, scope, buckets)
		},
	})
	if err != nil {
		return nil, err
	}

	// Solve the final state.
	def, err := b.Final.Marshal(ctx)
	if err != nil {
		return nil, fmt.Errorf("marshal final state: %w", err)
	}
	res, err := c.Solve(ctx, client.SolveRequest{Definition: def.ToPB()})
	if err != nil {
		return nil, err
	}
	ref, err := res.SingleRef()
	if err != nil {
		return nil, err
	}

	labels := map[string]string{}
	for k, v := range r.Settings.Labels {
		labels[k] = v
	}
	if r.Settings.Record {
		labels[recipe.RecipeLabel] = string(r.Resolved)
	}
	// The client compares it with its own version after the build.
	labels[recipe.FrontendVersionLabel] = Version
	cfg, err := imageConfig(base, labels, epoch)
	if err != nil {
		return nil, err
	}
	out := client.NewResult()
	out.AddMeta(exptypes.ExporterImageConfigKey, cfg)
	// The epoch for the exporter: with rewrite-timestamp it clamps the
	// file times in the layers to it. The base layers stay as they are;
	// the base config tells the exporter which ones they are.
	if epoch != nil {
		out.AddMeta(commonexptypes.ExporterEpochKey, []byte(strconv.FormatInt(*epoch, 10)))
		out.AddMeta(exptypes.ExporterImageBaseConfigKey, base)
	}
	out.SetRef(ref)
	return out, nil
}

// answer solves the result of mode lint or resolve.
func answer(ctx context.Context, c client.Client, mode string, r *recipe.Recipe) (*client.Result, error) {
	st := llb.Scratch().File(llb.Mkfile("/version", 0o644, []byte(Version+"\n")),
		llb.WithCustomName("["+mode+"] version"))
	if mode == "resolve" {
		st = st.File(llb.Mkfile("/recipe.yml", 0o644, r.Resolved), llb.WithCustomName("[resolve] recipe.yml"))
	}
	def, err := st.Marshal(ctx)
	if err != nil {
		return nil, err
	}
	return c.Solve(ctx, client.SolveRequest{Definition: def.ToPB()})
}

// recipeReader returns a reader for the files of the recipe. Each file is
// loaded on its own from the "dockerfile" local source when the parser
// asks for it, so nothing but the entry file and what it includes leaves
// the recipe directory. An import by URL is fetched by BuildKit, and
// checked against its #sha256= when it has one.
func recipeReader(ctx context.Context, c client.Client) recipe.ReadFunc {
	return func(name string) ([]byte, error) {
		if recipe.IsURL(name) {
			u, sum, err := recipe.SplitPin(name)
			if err != nil {
				return nil, err
			}
			const file = "recipe.yml"
			hopts := []llb.HTTPOption{llb.Filename(file), llb.WithCustomName("[load] " + u)}
			if sum != "" {
				hopts = append(hopts, llb.Checksum(digest.NewDigestFromEncoded(digest.SHA256, sum)))
			}
			return solveAndRead(ctx, c, llb.HTTP(u, hopts...), file)
		}
		src := llb.Local(localDockerfile,
			llb.IncludePatterns([]string{name}),
			llb.ExcludePatterns(compile.ContextExcludes),
			llb.WithCustomName("[load] "+name),
		)
		def, err := src.Marshal(ctx)
		if err != nil {
			return nil, err
		}
		res, err := c.Solve(ctx, client.SolveRequest{Definition: def.ToPB()})
		if err != nil {
			return nil, err
		}
		ref, err := res.SingleRef()
		if err != nil {
			return nil, err
		}
		return ref.ReadFile(ctx, client.ReadRequest{Filename: name})
	}
}

// dependent runs the guard for the groups of a phase that have no run, in
// parallel, and returns those that may not be built on an empty state,
// each with the reason.
//
// For such a group, merging would not give the same tree as building on
// lower. It is built on lower instead, which is always correct but ties
// its cache to lower. That is easy to miss, so the reason becomes part of
// the name of the group's steps and shows up in the output of every build.
// (client.Warn would be the natural channel, but a warning is tied to a
// vertex digest, and the solver's digests differ from those of the LLB
// definition; such warnings were silently dropped when tried.)
func dependent(ctx context.Context, c client.Client, r *recipe.Recipe, toolbox string, ph recipe.Phase, lower llb.State, alone map[string]llb.State) (map[string]string, error) {
	var mu sync.Mutex
	rejected := map[string]string{}
	eg, ectx := errgroup.WithContext(ctx)
	for group, upper := range alone {
		group, upper := group, upper
		eg.Go(func() error {
			st := compile.GuardState(toolbox, lower, upper, compile.PhaseTag(r, phaseIndex(r, ph.Name)), group)
			data, err := solveAndRead(ectx, c, st, compile.GuardPath)
			if err != nil {
				return fmt.Errorf("phase %q group %q: check independence: %w", ph.Name, group, err)
			}
			findings := guardFindings(string(data))
			if len(findings) == 0 {
				return nil
			}
			reason := findings[0]
			if len(findings) > 1 {
				reason += fmt.Sprintf(" (and %d more)", len(findings)-1)
			}
			mu.Lock()
			rejected[group] = reason
			mu.Unlock()
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return nil, err
	}
	return rejected, nil
}

// guardFindings turns the guard's output (see compile/guard.sh) into
// short reasons.
func guardFindings(out string) []string {
	var res []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		kind, rest, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		switch kind {
		case "L":
			res = append(res, fmt.Sprintf("/%s is a symbolic link there", rest))
		case "T":
			res = append(res, fmt.Sprintf("/%s is a file on one side and a directory on the other", rest))
		case "M":
			tok, p, _ := strings.Cut(rest, " ")
			lower, upper, _ := strings.Cut(tok, ":")
			res = append(res, fmt.Sprintf("/%s has mode.uid.gid %s there, not %s", p, lower, upper))
		}
	}
	return res
}

func solveAndRead(ctx context.Context, c client.Client, st llb.State, filename string) ([]byte, error) {
	def, err := st.Marshal(ctx)
	if err != nil {
		return nil, err
	}
	res, err := c.Solve(ctx, client.SolveRequest{Definition: def.ToPB(), Evaluate: true})
	if err != nil {
		return nil, err
	}
	ref, err := res.SingleRef()
	if err != nil {
		return nil, err
	}
	return ref.ReadFile(ctx, client.ReadRequest{Filename: filename})
}

// checkConflicts checks that the groups of a phase are independent, in
// two passes (see package conflict): every group's write set, in
// parallel; then, for the paths that more than one group writes, their
// content and metadata.
// checkBuckets checks that no file is in two buckets of a collector that
// fans out: pass 1 of the conflict check, strictly, since a file of two
// packages is a conflict even when it is the same.
func checkBuckets(ctx context.Context, c client.Client, toolbox, scope string, buckets map[string]llb.State) error {
	var mu sync.Mutex
	sets := map[string][]conflict.Entry{}
	eg, ectx := errgroup.WithContext(ctx)
	for name, layer := range buckets {
		name, layer := name, layer
		eg.Go(func() error {
			st := compile.WriteSetState(toolbox, nil, nil, layer, scope, name)
			data, err := solveAndRead(ectx, c, st, compile.WritesPath)
			if err != nil {
				return fmt.Errorf("%s %s: collect write set: %w", scope, name, err)
			}
			entries, err := conflict.ParseWriteSet(string(data))
			if err != nil {
				return fmt.Errorf("%s %s: %w", scope, name, err)
			}
			mu.Lock()
			sets[name] = entries
			mu.Unlock()
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return err
	}
	return conflict.CheckStrict(scope, sets)
}

func checkConflicts(ctx context.Context, c client.Client, r *recipe.Recipe, toolbox string, plan compile.PhasePlan) error {
	name := plan.Phase.Name
	tag := compile.PhaseTag(r, phaseIndex(r, name))
	var mu sync.Mutex
	sets := map[string][]conflict.Entry{}
	eg, ectx := errgroup.WithContext(ctx)
	for _, g := range plan.Phase.BuildGroupNames() {
		g := g
		eg.Go(func() error {
			var lower, upper *llb.State
			if !plan.Isolated[g] {
				up := plan.Uppers[g]
				lower, upper = &plan.Input, &up
			}
			st := compile.WriteSetState(toolbox, lower, upper, plan.Layers[g], tag, g)
			data, err := solveAndRead(ectx, c, st, compile.WritesPath)
			if err != nil {
				return fmt.Errorf("phase %q group %q: collect write set: %w", name, g, err)
			}
			entries, err := conflict.ParseWriteSet(string(data))
			if err != nil {
				return fmt.Errorf("phase %q group %q: %w", name, g, err)
			}
			mu.Lock()
			sets[g] = entries
			mu.Unlock()
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return err
	}

	eg, ectx = errgroup.WithContext(ctx)
	for g, paths := range conflict.Overlaps(sets) {
		g, paths := g, paths
		eg.Go(func() error {
			st := compile.HashState(toolbox, plan.Layers[g], paths, tag, g)
			data, err := solveAndRead(ectx, c, st, compile.HashesPath)
			if err != nil {
				return fmt.Errorf("phase %q group %q: hash shared paths: %w", name, g, err)
			}
			tokens, err := conflict.ParseHashes(string(data))
			if err != nil {
				return fmt.Errorf("phase %q group %q: %w", name, g, err)
			}
			mu.Lock()
			conflict.SetTokens(sets[g], tokens)
			mu.Unlock()
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return err
	}
	return conflict.Check(name, sets)
}

// resolveBaseConfig fetches the base image's OCI config bytes.
func resolveBaseConfig(ctx context.Context, c client.Client, ref string) ([]byte, error) {
	_, _, cfg, err := c.ResolveImageConfig(ctx, ref, resolveOpt())
	return cfg, err
}

func phaseIndex(r *recipe.Recipe, name string) int {
	for i, ph := range r.Phases {
		if ph.Name == name {
			return i
		}
	}
	return 0
}
