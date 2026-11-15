// Package frontend wires recipe parsing, LLB compilation and write-set
// validation into a BuildKit gateway frontend. The same binary also serves
// `lint` and `graph` modes (see cmd/frontend) so a thin CLI in any language
// can reuse the parser by running the frontend image via podman.
package frontend

import (
	"context"
	"fmt"
	"strings"

	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/exporter/containerimage/exptypes"
	"github.com/moby/buildkit/frontend/gateway/client"
	"golang.org/x/sync/errgroup"

	"github.com/Nomira-OS/nomira/internal/compile"
	"github.com/Nomira-OS/nomira/internal/conflict"
	"github.com/Nomira-OS/nomira/internal/recipe"
)

const (
	localDockerfile = "dockerfile" // BuildKit's name for the frontend-file source
	keyFilename     = "filename"
	defaultFilename = "recipe.yml"
	buildArgPrefix  = "build-arg:"
)

// Build is the gateway entrypoint (grpcclient.RunFromEnvironment target).
func Build(ctx context.Context, c client.Client) (*client.Result, error) {
	opts := c.BuildOpts().Opts

	filename := opts[keyFilename]
	if filename == "" {
		filename = defaultFilename
	}
	read, err := recipeReader(ctx, c, filename)
	if err != nil {
		return nil, err
	}

	buildArgs := map[string]string{}
	for k, v := range opts {
		if name, ok := strings.CutPrefix(k, buildArgPrefix); ok {
			buildArgs[name] = v
		}
	}

	r, err := recipe.ParseFile(filename, read, buildArgs)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filename, err)
	}

	b, err := compile.Compile(r)
	if err != nil {
		return nil, err
	}

	// Validate every multi-group phase before solving the final state:
	// conflicts must fail the build before anything is merged or exported.
	for _, plan := range b.Phases {
		if !plan.Validate {
			continue
		}
		if err := validatePhase(ctx, c, r, plan); err != nil {
			return nil, err
		}
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

	// Carry the base image's config (entrypoint, env, labels …) so the
	// exported image stays bootable; bootc images live or die by it.
	base, err := resolveBaseConfig(ctx, c, r.Settings.Base)
	if err != nil {
		return nil, fmt.Errorf("read image config of base %s: %w", r.Settings.Base, err)
	}
	cfg, err := imageConfig(base, r.Settings.Description, r.Settings.Labels)
	if err != nil {
		return nil, err
	}
	out := client.NewResult()
	out.AddMeta(exptypes.ExporterImageConfigKey, cfg)
	out.SetRef(ref)
	return out, nil
}

// recipeReader loads the recipe directory (entry file plus all YAML files,
// so includes resolve) from the "dockerfile" local source and returns a
// reader over it.
func recipeReader(ctx context.Context, c client.Client, filename string) (recipe.ReadFunc, error) {
	src := llb.Local(localDockerfile,
		llb.IncludePatterns([]string{filename, "*.yml", "*.yaml", "**/*.yml", "**/*.yaml"}),
		llb.WithCustomName("[recipe] load "+filename),
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
	return func(name string) ([]byte, error) {
		return ref.ReadFile(ctx, client.ReadRequest{Filename: name})
	}, nil
}

// validatePhase collects each group's write set (in parallel) and checks
// pairwise disjointness under the phase's policy.
func validatePhase(ctx context.Context, c client.Client, r *recipe.Recipe, plan compile.PhasePlan) error {
	policy := plan.Phase.Policy(r.Settings.Validation)

	type result struct {
		group   string
		entries []conflict.Entry
	}
	eg, ectx := errgroup.WithContext(ctx)
	results := make(chan result, len(plan.Uppers))

	for _, g := range plan.Phase.GroupNames() {
		g := g
		eg.Go(func() error {
			st := compile.WriteSetState(r.Settings.Toolbox, plan.Input, plan.Uppers[g], plan.Phase.Name, g)
			def, err := st.Marshal(ectx)
			if err != nil {
				return err
			}
			res, err := c.Solve(ectx, client.SolveRequest{Definition: def.ToPB(), Evaluate: true})
			if err != nil {
				return fmt.Errorf("phase %q group %q: collect write set: %w", plan.Phase.Name, g, err)
			}
			ref, err := res.SingleRef()
			if err != nil {
				return err
			}
			data, err := ref.ReadFile(ectx, client.ReadRequest{Filename: compile.WritesPath})
			if err != nil {
				return fmt.Errorf("phase %q group %q: read write set: %w", plan.Phase.Name, g, err)
			}
			entries, err := conflict.ParseWriteSet(string(data))
			if err != nil {
				return fmt.Errorf("phase %q group %q: %w", plan.Phase.Name, g, err)
			}
			results <- result{group: g, entries: entries}
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return err
	}
	close(results)

	sets := map[string][]conflict.Entry{}
	for res := range results {
		sets[res.group] = res.entries
	}
	return conflict.Check(plan.Phase.Name, policy, plan.Phase.Shared, sets)
}

// resolveBaseConfig fetches the base image's OCI config bytes.
func resolveBaseConfig(ctx context.Context, c client.Client, ref string) ([]byte, error) {
	_, _, cfg, err := c.ResolveImageConfig(ctx, ref, resolveOpt())
	return cfg, err
}
