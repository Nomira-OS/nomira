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

	// The graph is assembled phase by phase, looking at real results on the
	// way: whether a group without run may be built apart from the state
	// before it, whether expected paths exist, and whether the groups of a
	// phase are disjoint. A problem stops the build before anything is
	// merged on top of it or exported.
	b, err := compile.Compile(r, compile.Hooks{
		Decide: func(ph recipe.Phase, group string, lower, upper llb.State) (bool, string, error) {
			return independent(ctx, c, r, ph, group, lower, upper)
		},
		PhaseDone: func(plan compile.PhasePlan) error {
			if err := checkExpects(ctx, c, plan); err != nil {
				return err
			}
			if !plan.Validate {
				return nil
			}
			return validatePhase(ctx, c, r, plan)
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

// independent runs the guard for a group without run. It reports whether
// merging the group, built on an empty state, gives the same tree as
// building it on lower. If not, the group is built on lower, which is
// always correct but ties its cache to lower. That is easy to miss, so it
// is reported as a build warning that names every path in the way.
func independent(ctx context.Context, c client.Client, r *recipe.Recipe, ph recipe.Phase, group string, lower, upper llb.State) (bool, string, error) {
	st := compile.GuardState(r.Settings.Toolbox, lower, upper, ph.Name, group)
	def, err := st.Marshal(ctx)
	if err != nil {
		return false, "", err
	}
	data, err := solveAndRead(ctx, c, st, compile.GuardPath)
	if err != nil {
		return false, "", fmt.Errorf("phase %q group %q: check independence: %w", ph.Name, group, err)
	}
	findings := guardFindings(string(data))
	if len(findings) == 0 {
		return true, "", nil
	}
	msg := fmt.Sprintf("phase %q: group %q has no run but is not independent of the state before it. "+
		"It is built on that state and will be rebuilt whenever the base image or an earlier phase changes.",
		ph.Name, group)
	detail := make([][]byte, 0, len(findings)+1)
	for _, f := range findings {
		detail = append(detail, []byte(f))
	}
	detail = append(detail, []byte("To make the group independent, copy to a path that is not affected, or accept the rebuild."))
	if dgst, err := def.Head(); err == nil {
		// A warning that cannot be delivered must not fail the build.
		_ = c.Warn(ctx, dgst, msg, client.WarnOpts{Level: 1, Detail: detail})
	}
	reason := findings[0]
	if len(findings) > 1 {
		reason += fmt.Sprintf(" (and %d more)", len(findings)-1)
	}
	return false, reason, nil
}

// guardFindings turns the guard's output (see compile/guard.sh) into
// sentences.
func guardFindings(out string) []string {
	var res []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		kind, rest, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		switch kind {
		case "T":
			res = append(res, fmt.Sprintf("/%s is a symbolic link or of another type there", rest))
		case "M":
			tok, p, _ := strings.Cut(rest, " ")
			lower, upper, _ := strings.Cut(tok, ":")
			res = append(res, fmt.Sprintf("/%s has mode.uid.gid %s there, the group would create it as %s", p, lower, upper))
		}
	}
	return res
}

// checkExpects verifies that the paths the groups of a phase expect exist
// in the state the phase starts from.
func checkExpects(ctx context.Context, c client.Client, plan compile.PhasePlan) error {
	var ref client.Reference
	var missing []string
	for _, g := range plan.Phase.GroupNames() {
		for _, p := range plan.Phase.Groups[g].Expects {
			if ref == nil {
				def, err := plan.Input.Marshal(ctx)
				if err != nil {
					return err
				}
				res, err := c.Solve(ctx, client.SolveRequest{Definition: def.ToPB()})
				if err != nil {
					return err
				}
				if ref, err = res.SingleRef(); err != nil {
					return err
				}
			}
			if _, err := ref.StatFile(ctx, client.StatRequest{Path: p}); err != nil {
				missing = append(missing, fmt.Sprintf("  group %q: %s", g, p))
			}
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("phase %q: expected paths do not exist in the state the phase starts from:\n%s",
		plan.Phase.Name, strings.Join(missing, "\n"))
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

	for _, g := range plan.Phase.BuildGroupNames() {
		g := g
		eg.Go(func() error {
			lower := &plan.Input
			if plan.Isolated[g] {
				lower = nil
			}
			st := compile.WriteSetState(r.Settings.Toolbox, lower, plan.Uppers[g], plan.Phase.Name, g)
			data, err := solveAndRead(ectx, c, st, compile.WritesPath)
			if err != nil {
				return fmt.Errorf("phase %q group %q: collect write set: %w", plan.Phase.Name, g, err)
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
