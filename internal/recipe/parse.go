package recipe

import (
	"bytes"
	"fmt"
	"path"
	"sort"
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
	Name       string              `yaml:"name"`
	Validation string              `yaml:"validation"`
	Shared     []string            `yaml:"shared"`
	Env        map[string]string   `yaml:"env"`
	Include    string              `yaml:"include"`
	Collect    string              `yaml:"collect"`
	Groups     map[string]rawGroup `yaml:"groups"`
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

	if _, ok := args[itemsVar]; ok {
		return nil, errf("recipe: settings.args: %q is reserved for collector phases", itemsVar)
	}

	// Prepare stages.
	for name, rs := range raw.Prepare {
		if name == "context" || strings.Contains(name, "/") {
			return nil, errf("recipe: prepare stage name %q is not allowed", name)
		}
		fs, err := ld.stage(entry, rs)
		if err != nil {
			return nil, errf("recipe: prepare.%s: %w", name, err)
		}
		st, err := buildStage(fs, args)
		if err != nil {
			return nil, errf("recipe: prepare.%s: %w", name, err)
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
	collectors := map[string]bool{}
	for pi, rp := range phases {
		if rp.Name == "" {
			return nil, errf("recipe: phases[%d]: name is required", pi)
		}
		if seenPhase[rp.Name] {
			return nil, errf("recipe: duplicate phase name %q", rp.Name)
		}
		seenPhase[rp.Name] = true
		if rp.Collect != "" {
			collectors[rp.Name] = true
		}
	}
	for name := range raw.Prepare {
		if seenPhase[name] {
			return nil, errf("recipe: %q is both a prepare stage and a phase", name)
		}
	}
	items := map[string]map[string]bool{} // collector -> set of items
	earlier := map[string]bool{}          // phases before the current one
	for _, rp := range phases {
		ph := Phase{
			Name:       rp.Name,
			Validation: rp.Validation,
			Shared:     rp.Shared,
			Env:        expandMap(rp.Env, args),
			Groups:     map[string]Group{},
		}
		if err := checkEnvNames(fmt.Sprintf("phase %q: env", rp.Name), ph.Env); err != nil {
			return nil, err
		}
		if rp.Collect != "" {
			if len(rp.Groups) > 0 || rp.Validation != "" || len(rp.Shared) > 0 {
				return nil, errf("recipe: phase %q: collect cannot be combined with groups, validation or shared", rp.Name)
			}
			if ReservedGroupKey(rp.Name) {
				return nil, errf("recipe: phase %q: a collector cannot use this name, it has a fixed meaning in groups", rp.Name)
			}
			if !strings.Contains(rp.Collect, itemsRef) {
				return nil, errf("recipe: phase %q: collect must contain %s", rp.Name, itemsRef)
			}
			r.Phases = append(r.Phases, ph)
			earlier[rp.Name] = true
			continue
		}
		if !validPolicy(rp.Validation) {
			return nil, errf("recipe: phase %q: validation %q invalid (strict|allow-identical)", rp.Name, rp.Validation)
		}
		if len(rp.Groups) == 0 {
			return nil, errf("recipe: phase %q: needs groups or collect", rp.Name)
		}
		for gname, fg := range rp.Groups {
			where := fmt.Sprintf("recipe: phase %q: group %q", rp.Name, gname)
			if len(fg.Lines) == 0 && len(fg.Contribute) == 0 {
				return nil, errf("%s: needs commands or a contribution to a collector", where)
			}
			// Stages of the group. Their names are local to it.
			local := map[string]string{}
			for sname, fs := range fg.Prepare {
				if sname == "context" || strings.Contains(sname, "/") {
					return nil, errf("%s: prepare stage name %q is not allowed", where, sname)
				}
				st, err := buildStage(fs, args)
				if err != nil {
					return nil, errf("%s: prepare.%s: %w", where, sname, err)
				}
				qual := rp.Name + "/" + gname + "/" + sname
				local[sname] = qual
				r.Prepare[qual] = st
			}
			grp := Group{}
			for i, line := range fg.Lines {
				cmd, err := ParseCommand(ExpandArgs(line, args))
				if err != nil {
					return nil, errf("%s: command %d: %w", where, i+1, err)
				}
				resolve := func(name string) (string, error) {
					return resolveFrom(name, local, raw.Prepare, earlier, seenPhase)
				}
				if cmd, err = resolveSources(cmd, resolve); err != nil {
					return nil, errf("%s: command %d: %w", where, i+1, err)
				}
				grp.Commands = append(grp.Commands, cmd)
			}
			for _, e := range fg.Expects {
				if !strings.HasPrefix(e, "/") || path.Clean(e) != strings.TrimSuffix(e, "/") && e != "/" {
					return nil, errf("%s: expects: %q is not a clean absolute path", where, e)
				}
				grp.Expects = append(grp.Expects, path.Clean(e))
			}
			for cname, list := range fg.Contribute {
				if !collectors[cname] {
					return nil, errf("%s: unknown key %q: no collector phase of that name (collectors: %s)", where, cname, strings.Join(sortedKeys(collectors), ", "))
				}
				for _, it := range list {
					it = ExpandArgs(it, args)
					if !validItem(it) {
						return nil, errf("%s: %s: %q is not a valid item (letters, digits and @%%+=:,./_- only, not starting with -)", where, cname, it)
					}
					if items[cname] == nil {
						items[cname] = map[string]bool{}
					}
					items[cname][it] = true
				}
				if grp.Contribute == nil {
					grp.Contribute = map[string][]string{}
				}
				grp.Contribute[cname] = list
			}
			ph.Groups[gname] = grp
		}
		r.Phases = append(r.Phases, ph)
		earlier[rp.Name] = true
	}

	// Collectors: fill in what the groups contributed. The items are sorted
	// and unique so that the command, and with it the cache key, does not
	// depend on the order of groups or files.
	for i, rp := range phases {
		if rp.Collect == "" {
			continue
		}
		list := sortedKeys(items[rp.Name])
		line := strings.ReplaceAll(rp.Collect, itemsRef, strings.Join(list, " "))
		cmd, err := ParseCommand(ExpandArgs(line, args))
		if err != nil {
			return nil, errf("recipe: phase %q: collect: %w", rp.Name, err)
		}
		run, ok := cmd.(RunCmd)
		if !ok {
			return nil, errf("recipe: phase %q: collect must be a run command", rp.Name)
		}
		before := map[string]bool{}
		for _, p := range phases[:i] {
			before[p.Name] = true
		}
		resolved, err := resolveSources(run, func(name string) (string, error) {
			return resolveFrom(name, nil, raw.Prepare, before, seenPhase)
		})
		if err != nil {
			return nil, errf("recipe: phase %q: collect: %w", rp.Name, err)
		}
		run = resolved.(RunCmd)
		r.Phases[i].Collect = &run
		r.Phases[i].Items = list
	}
	return r, nil
}

const (
	itemsVar = "items"
	itemsRef = "${items}"
)

// buildStage parses the commands of a prepare stage.
func buildStage(fs flatStage, args map[string]string) (Stage, error) {
	if fs.From == "" {
		return Stage{}, errf("from is required")
	}
	if len(fs.Lines) == 0 {
		return Stage{}, errf("commands must not be empty")
	}
	st := Stage{From: ExpandArgs(fs.From, args)}
	for i, line := range fs.Lines {
		cmd, err := ParseCommand(ExpandArgs(line, args))
		if err != nil {
			return Stage{}, errf("command %d: %w", i+1, err)
		}
		if cc, ok := cmd.(CopyCmd); ok && cc.From != "" {
			return Stage{}, errf("copy --from inside prepare stages is not supported yet")
		}
		if rc, ok := cmd.(RunCmd); ok {
			for _, m := range rc.Mounts {
				if m.Type == "bind" {
					return Stage{}, errf("--mount=type=bind inside prepare stages is not supported yet")
				}
			}
		}
		st.Commands = append(st.Commands, cmd)
	}
	return st, nil
}

// validItem reports whether s can be placed into a collector command as a
// single shell word without quoting.
func validItem(s string) bool {
	if s == "" || s[0] == '-' {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.ContainsRune("@%+=:,./_-", c):
		default:
			return false
		}
	}
	return true
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Graph renders the pipeline as a Mermaid flowchart (for `nomira graph` /
// the frontend's graph mode).
func (r *Recipe) Graph() string {
	var b strings.Builder
	b.WriteString("flowchart TD\n")
	b.WriteString(fmt.Sprintf("    base[\"base: %s\"]\n", r.Settings.Base))
	stages := make([]string, 0, len(r.Prepare))
	for name := range r.Prepare {
		stages = append(stages, name)
	}
	sort.Strings(stages)
	for _, name := range stages {
		b.WriteString(fmt.Sprintf("    prep_%s[\"prepare %s (from %s)\"]\n", graphID(name), name, r.Prepare[name].From))
	}
	prev := "base"
	for _, ph := range r.Phases {
		if ph.IsCollector() {
			id := "c_" + graphID(ph.Name)
			b.WriteString(fmt.Sprintf("    %s[/\"collect %s (%d items)\"/]\n    %s --> %s\n", id, ph.Name, len(ph.Items), prev, id))
			prev = id
			continue
		}
		names := ph.BuildGroupNames()
		merge := "merge_" + graphID(ph.Name)
		for _, g := range names {
			id := fmt.Sprintf("g_%s_%s", graphID(ph.Name), graphID(g))
			label := fmt.Sprintf("%s / %s", ph.Name, g)
			if !ph.Groups[g].HasRun() {
				// No run: the group does not see the state before it.
				b.WriteString(fmt.Sprintf("    %s([\"%s\"])\n", id, label))
			} else {
				b.WriteString(fmt.Sprintf("    %s[\"%s\"]\n    %s --> %s\n", id, label, prev, id))
			}
			b.WriteString(fmt.Sprintf("    %s --> %s\n", id, merge))
		}
		if len(names) == 0 {
			continue
		}
		b.WriteString(fmt.Sprintf("    %s{{\"merge %s (%s)\"}}\n    %s --> %s\n", merge, ph.Name, ph.Policy(r.Settings.Validation), prev, merge))
		prev = merge
	}
	// Edges that do not follow the order of the phases.
	for _, ph := range r.Phases {
		for _, g := range ph.GroupNames() {
			grp := ph.Groups[g]
			id := fmt.Sprintf("g_%s_%s", graphID(ph.Name), graphID(g))
			for _, c := range grp.Commands {
				if cc, ok := c.(CopyCmd); ok && cc.From != "" {
					b.WriteString(fmt.Sprintf("    prep_%s -. copy .-> %s\n", graphID(cc.From), id))
				}
			}
			if len(grp.Commands) == 0 {
				b.WriteString(fmt.Sprintf("    %s([\"%s / %s\"])\n", id, ph.Name, g))
			}
			for _, cname := range sortedContrib(grp.Contribute) {
				b.WriteString(fmt.Sprintf("    %s -. \"%s\" .-> c_%s\n", id, strings.Join(grp.Contribute[cname], " "), graphID(cname)))
			}
		}
	}
	return b.String()
}

// graphID turns a name into a Mermaid node id.
func graphID(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '_'
	}, s)
}

func sortedContrib(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
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

// resolveFrom checks the name in copy --from= or --mount=type=bind,from=
// and returns what the compiler looks up: a stage local to the group (by
// its qualified name), a prepare stage, or a phase that comes before.
func resolveFrom(name string, local map[string]string, stages map[string]rawStage, earlier, phases map[string]bool) (string, error) {
	if qual, ok := local[name]; ok {
		return qual, nil
	}
	if _, ok := stages[name]; ok {
		return name, nil
	}
	if earlier[name] {
		return name, nil
	}
	if phases[name] {
		return "", errf("from=%s: a phase can only use phases that come before it", name)
	}
	return "", errf("from=%s: no prepare stage or phase of that name", name)
}

// resolveSources resolves the stage and phase names in a command.
func resolveSources(cmd Command, resolve func(string) (string, error)) (Command, error) {
	switch c := cmd.(type) {
	case CopyCmd:
		if c.From != "" {
			from, err := resolve(c.From)
			if err != nil {
				return nil, err
			}
			c.From = from
		}
		return c, nil
	case RunCmd:
		mounts := make([]Mount, len(c.Mounts))
		copy(mounts, c.Mounts)
		for i, m := range mounts {
			if m.Type != "bind" {
				continue
			}
			from, err := resolve(m.From)
			if err != nil {
				return nil, err
			}
			mounts[i].From = from
		}
		c.Mounts = mounts
		return c, nil
	}
	return cmd, nil
}
