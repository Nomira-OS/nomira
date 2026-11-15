package recipe

import (
	"bytes"
	"fmt"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

// raw YAML shapes (strict decoding: unknown keys are errors).

type rawRecipe struct {
	Settings rawSettings         `yaml:"settings"`
	Prepare  map[string]rawStage `yaml:"prepare"`
	Phases   []rawPhase          `yaml:"phases"`
}

type rawSettings struct {
	Name       string            `yaml:"name"`
	Base       string            `yaml:"base"`
	Validation string            `yaml:"validation"`
	Toolbox    string            `yaml:"toolbox"`
	Args       map[string]string `yaml:"args"`

	Description string            `yaml:"description"`
	Labels      map[string]string `yaml:"labels"`
	Env         map[string]string `yaml:"env"`
	Platforms   []string          `yaml:"platforms"`
	Tags        []string          `yaml:"tags"`
}

type rawStage struct {
	From     string  `yaml:"from"`
	Commands cmdList `yaml:"commands"`
}

type rawPhase struct {
	Name       string             `yaml:"name"`
	Validation string             `yaml:"validation"`
	Shared     []string           `yaml:"shared"`
	Env        map[string]string  `yaml:"env"`
	Include    string             `yaml:"include"`
	Groups     map[string]cmdList `yaml:"groups"`
}

// Parse decodes, schema-validates and arg-expands a recipe. buildArgs are the
// caller-supplied overrides (from `--opt build-arg:NAME=...`); only args
// declared in settings.args may be overridden.
//
// Parse handles a single self-contained file; use ParseFile for recipes
// that use `include`.
func Parse(data []byte, buildArgs map[string]string) (*Recipe, error) {
	const entry = "recipe.yml"
	return ParseFile(entry, func(name string) ([]byte, error) {
		if name == entry {
			return data, nil
		}
		return nil, errf("includes are not available when the recipe is read from a single stream")
	}, buildArgs)
}

// ParseFile parses the recipe at entry, resolving `include` through read.
// Include paths are relative to the including file.
func ParseFile(entry string, read ReadFunc, buildArgs map[string]string) (*Recipe, error) {
	entry = path.Clean(entry)
	data, err := read(entry)
	if err != nil {
		return nil, errf("recipe: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var raw rawRecipe
	if err := dec.Decode(&raw); err != nil {
		return nil, errf("recipe: %w", err)
	}
	ld := &loader{read: read, stack: []string{entry}}

	// Effective args: declared defaults, overridden by build args.
	args := map[string]string{}
	for k, v := range raw.Settings.Args {
		args[k] = v
	}
	for k, v := range buildArgs {
		if _, declared := args[k]; !declared {
			return nil, errf("recipe: build-arg %q is not declared in settings.args", k)
		}
		args[k] = v
	}

	r := &Recipe{
		Settings: Settings{
			Name:       raw.Settings.Name,
			Base:       ExpandArgs(raw.Settings.Base, args),
			Validation: raw.Settings.Validation,
			Toolbox:    raw.Settings.Toolbox,
			Args:       args,

			Description: ExpandArgs(raw.Settings.Description, args),
			Labels:      expandMap(raw.Settings.Labels, args),
			Env:         expandMap(raw.Settings.Env, args),
			Platforms:   raw.Settings.Platforms,
			Tags:        raw.Settings.Tags,
		},
		Prepare: map[string]Stage{},
	}
	if err := checkEnvNames("settings.env", r.Settings.Env); err != nil {
		return nil, err
	}
	for _, p := range r.Settings.Platforms {
		if os, arch, ok := strings.Cut(p, "/"); !ok || os == "" || arch == "" {
			return nil, errf("recipe: settings.platforms: %q is not of the form os/arch", p)
		}
	}
	for i, t := range r.Settings.Tags {
		r.Settings.Tags[i] = ExpandArgs(t, args)
		if r.Settings.Tags[i] == "" || strings.ContainsAny(r.Settings.Tags[i], " /:") {
			return nil, errf("recipe: settings.tags: %q is not a valid tag", t)
		}
	}
	if r.Settings.Base == "" {
		return nil, errf("recipe: settings.base is required")
	}
	if !validPolicy(r.Settings.Validation) {
		return nil, errf("recipe: settings.validation %q invalid (strict|allow-identical)", r.Settings.Validation)
	}
	if r.Settings.Toolbox == "" {
		r.Settings.Toolbox = DefaultToolbox
	}

	// Prepare stages.
	for name, rs := range raw.Prepare {
		if name == "context" {
			return nil, errf("recipe: prepare stage name %q is reserved", name)
		}
		if rs.From == "" {
			return nil, errf("recipe: prepare.%s: from is required", name)
		}
		lines, err := ld.commands(entry, rs.Commands)
		if err != nil {
			return nil, errf("recipe: prepare.%s: %w", name, err)
		}
		if len(lines) == 0 {
			return nil, errf("recipe: prepare.%s: commands must not be empty", name)
		}
		st := Stage{From: ExpandArgs(rs.From, args)}
		for i, line := range lines {
			cmd, err := ParseCommand(ExpandArgs(line, args))
			if err != nil {
				return nil, errf("recipe: prepare.%s: command %d: %w", name, i+1, err)
			}
			if cc, ok := cmd.(CopyCmd); ok && cc.From != "" {
				return nil, errf("recipe: prepare.%s: copy --from inside prepare stages is not supported yet", name)
			}
			st.Commands = append(st.Commands, cmd)
		}
		r.Prepare[name] = st
	}

	// Phases.
	phases, err := ld.phases(entry, raw.Phases)
	if err != nil {
		return nil, errf("recipe: %w", err)
	}
	if len(phases) == 0 {
		return nil, errf("recipe: phases must not be empty")
	}
	seenPhase := map[string]bool{}
	for pi, rp := range phases {
		if rp.Name == "" {
			return nil, errf("recipe: phases[%d]: name is required", pi)
		}
		if seenPhase[rp.Name] {
			return nil, errf("recipe: duplicate phase name %q", rp.Name)
		}
		seenPhase[rp.Name] = true
		if !validPolicy(rp.Validation) {
			return nil, errf("recipe: phase %q: validation %q invalid (strict|allow-identical)", rp.Name, rp.Validation)
		}
		if len(rp.Groups) == 0 {
			return nil, errf("recipe: phase %q: groups must not be empty", rp.Name)
		}
		ph := Phase{
			Name:       rp.Name,
			Validation: rp.Validation,
			Shared:     rp.Shared,
			Env:        expandMap(rp.Env, args),
			Groups:     map[string][]Command{},
		}
		if err := checkEnvNames(fmt.Sprintf("phase %q: env", rp.Name), ph.Env); err != nil {
			return nil, err
		}
		for gname, lines := range rp.Groups {
			if len(lines) == 0 {
				return nil, errf("recipe: phase %q: group %q: commands must not be empty", rp.Name, gname)
			}
			var cmds []Command
			for i, line := range lines {
				cmd, err := ParseCommand(ExpandArgs(line, args))
				if err != nil {
					return nil, errf("recipe: phase %q: group %q: command %d: %w", rp.Name, gname, i+1, err)
				}
				if cc, ok := cmd.(CopyCmd); ok && cc.From != "" {
					if _, exists := r.Prepare[cc.From]; !exists {
						return nil, errf("recipe: phase %q: group %q: copy --from=%s references an unknown prepare stage", rp.Name, gname, cc.From)
					}
				}
				cmds = append(cmds, cmd)
			}
			ph.Groups[gname] = cmds
		}
		r.Phases = append(r.Phases, ph)
	}
	return r, nil
}

// Graph renders the pipeline as a Mermaid flowchart (for `nomira graph` /
// the frontend's graph mode).
func (r *Recipe) Graph() string {
	var b strings.Builder
	b.WriteString("flowchart TD\n")
	b.WriteString(fmt.Sprintf("    base[\"base: %s\"]\n", r.Settings.Base))
	for name, st := range r.Prepare {
		b.WriteString(fmt.Sprintf("    prep_%s[\"prepare %s (from %s)\"]\n", name, name, st.From))
	}
	prev := "base"
	for _, ph := range r.Phases {
		merge := "merge_" + ph.Name
		names := ph.GroupNames()
		if len(names) == 1 {
			id := fmt.Sprintf("g_%s_%s", ph.Name, names[0])
			b.WriteString(fmt.Sprintf("    %s[\"%s / %s\"]\n    %s --> %s\n", id, ph.Name, names[0], prev, id))
			prev = id
			continue
		}
		for _, g := range names {
			id := fmt.Sprintf("g_%s_%s", ph.Name, g)
			b.WriteString(fmt.Sprintf("    %s[\"%s / %s\"]\n    %s --> %s\n", id, ph.Name, g, prev, id))
			b.WriteString(fmt.Sprintf("    %s --> %s{{\"merge + validate (%s)\"}}\n", id, merge, ph.Policy(r.Settings.Validation)))
		}
		prev = merge
	}
	// --from edges
	for _, ph := range r.Phases {
		for g, cmds := range ph.Groups {
			for _, c := range cmds {
				if cc, ok := c.(CopyCmd); ok && cc.From != "" {
					b.WriteString(fmt.Sprintf("    prep_%s -. copy .-> g_%s_%s\n", cc.From, ph.Name, g))
				}
			}
		}
	}
	return b.String()
}

func expandMap(m map[string]string, args map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = ExpandArgs(v, args)
	}
	return out
}

func checkEnvNames(where string, env map[string]string) error {
	for k := range env {
		if !validEnvName(k) {
			return errf("recipe: %s: %q is not a valid variable name", where, k)
		}
	}
	return nil
}
