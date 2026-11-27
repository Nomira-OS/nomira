package recipe

import (
	"fmt"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

// Parse parses a single self-contained recipe; use ParseFile for recipes
// that use `imports`.
func Parse(data []byte) (*Recipe, error) {
	const entry = "recipe.yml"
	return ParseFile(entry, func(name string) ([]byte, error) {
		if name == entry {
			return data, nil
		}
		return nil, errf("imports are not available when the recipe is read from a single stream")
	})
}

// ParseFile parses the recipe at entry, merging its imports, which it
// reads through read. Import paths are relative to the importing file.
func ParseFile(entry string, read ReadFunc) (*Recipe, error) {
	entry = path.Clean(entry)
	doc, err := loadDocument(entry, read)
	if err != nil {
		return nil, err
	}
	d := &decoder{doc: doc}
	r, err := d.recipe()
	if err != nil {
		return nil, err
	}
	if r.Resolved, err = doc.resolved(entry); err != nil {
		return nil, err
	}
	r.Files = doc.files
	return r, nil
}

// EpochVar is the variable every run sees with settings.source_date_epoch.
const EpochVar = "SOURCE_DATE_EPOCH"

const (
	itemsRef   = "${items}"
	keyCollect = "collect"
	keyEnv     = "env"
)

// decoder turns the merged document into a Recipe, with the file and line
// of the offending key in every error.
type decoder struct {
	doc *document
}

// at names where key k was written: file:line.
func (d *decoder) at(k *yaml.Node) string {
	from := d.doc.from[k]
	if len(from) == 0 {
		return fmt.Sprintf("line %d", k.Line)
	}
	return fmt.Sprintf("%s:%d", from[len(from)-1], k.Line)
}

// keys checks that n is a mapping and, with known, that it has only those
// keys. where names n in messages, key the node that holds n.
func (d *decoder) keys(n, key *yaml.Node, where string, known ...string) error {
	if n.Kind != yaml.MappingNode {
		return errf("%s: %s: expected a mapping", d.at(key), where)
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k := n.Content[i]
		if known != nil && !contains(known, k.Value) {
			return errf("%s: %s: unknown key %q (known: %s)", d.at(k), where, k.Value, strings.Join(known, ", "))
		}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// decode decodes v (the value of key k) into out, naming the key on error.
func (d *decoder) decode(k, v *yaml.Node, where string, out any) error {
	if err := v.Decode(out); err != nil {
		msg := err.Error()
		if te, ok := err.(*yaml.TypeError); ok {
			msg = strings.Join(te.Errors, "; ")
		}
		return errf("%s: %s: %s", d.at(k), where, msg)
	}
	return nil
}

// pairs iterates over a mapping.
func pairs(n *yaml.Node, fn func(k, v *yaml.Node) error) error {
	for i := 0; i+1 < len(n.Content); i += 2 {
		if err := fn(n.Content[i], n.Content[i+1]); err != nil {
			return err
		}
	}
	return nil
}

func (d *decoder) recipe() (*Recipe, error) {
	root := d.doc.root
	rootKey := &yaml.Node{Line: 1}
	d.doc.from[rootKey] = []string{d.doc.files[0]}
	if err := d.keys(root, rootKey, "recipe", "settings", "stages", "build"); err != nil {
		return nil, err
	}
	r := &Recipe{Stages: map[string]Stage{}}
	var buildKey, buildNode *yaml.Node
	err := pairs(root, func(k, v *yaml.Node) error {
		switch k.Value {
		case "settings":
			return d.settings(k, v, &r.Settings)
		case "stages":
			return d.stages(k, v, r)
		case "build":
			buildKey, buildNode = k, v
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if r.Settings.Base == "" {
		return nil, errf("settings.base is required")
	}
	if buildNode == nil || len(buildNode.Content) == 0 {
		return nil, errf("build must not be empty")
	}
	if err := d.build(buildKey, buildNode, r); err != nil {
		return nil, err
	}
	return r, nil
}

func (d *decoder) settings(key, n *yaml.Node, s *Settings) error {
	if err := d.keys(n, key, "settings", "base", "labels", "env", "mounts", "source_date_epoch", "record"); err != nil {
		return err
	}
	s.Record = true
	return pairs(n, func(k, v *yaml.Node) error {
		where := "settings." + k.Value
		switch k.Value {
		case "base":
			return d.decode(k, v, where, &s.Base)
		case "labels":
			return d.decode(k, v, where, &s.Labels)
		case "env":
			if err := d.decode(k, v, where, &s.Env); err != nil {
				return err
			}
			if err := checkEnvNames(s.Env); err != nil {
				return errf("%s: %s: %w", d.at(k), where, err)
			}
			if _, ok := s.Env[EpochVar]; ok {
				return errf("%s: %s: set %s with settings.source_date_epoch", d.at(k), where, EpochVar)
			}
			return nil
		case "mounts":
			return d.mounts(k, v, s)
		case "source_date_epoch":
			switch {
			case v.Tag == "!!bool":
				var on bool
				if err := v.Decode(&on); err != nil {
					return err
				}
				s.EpochOff = !on
			case v.Tag == "!!int":
				var e int64
				if err := v.Decode(&e); err != nil {
					return err
				}
				if e < 0 {
					return errf("%s: %s: %d is before 1970", d.at(k), where, e)
				}
				s.SourceDateEpoch = &e
			default:
				return errf("%s: %s: expected seconds since 1970, true or false", d.at(k), where)
			}
		case "record":
			return d.decode(k, v, where, &s.Record)
		}
		return nil
	})
}

// mounts decodes settings.mounts: target → the options of --mount.
func (d *decoder) mounts(key, n *yaml.Node, s *Settings) error {
	if n.Kind != yaml.MappingNode {
		return errf("%s: settings.mounts: expected a mapping of target: options", d.at(key))
	}
	return pairs(n, func(k, v *yaml.Node) error {
		where := "settings.mounts." + k.Value
		var spec string
		if err := d.decode(k, v, where, &spec); err != nil {
			return err
		}
		target := k.Value
		if strings.Contains(spec, "target=") {
			return errf("%s: %s: the target is the key", d.at(k), where)
		}
		if !strings.HasPrefix(target, "/") || target == "/" || path.Clean(target) != target {
			return errf("%s: %s: the key must be a clean absolute path below /", d.at(k), where)
		}
		m, err := parseMount(spec + ",target=" + target)
		if err != nil {
			return errf("%s: %s: %w", d.at(k), where, err)
		}
		if m.Type != "cache" && m.Type != "tmpfs" {
			return errf("%s: %s: only type=cache and type=tmpfs apply to every run", d.at(k), where)
		}
		s.Mounts = append(s.Mounts, m)
		return nil
	})
}

func (d *decoder) stages(key, n *yaml.Node, r *Recipe) error {
	if err := d.keys(n, key, "stages"); err != nil {
		return err
	}
	return pairs(n, func(k, v *yaml.Node) error {
		where := "stages." + k.Value
		if k.Value == "context" || strings.Contains(k.Value, "/") {
			return errf("%s: stage name %q is not allowed", d.at(k), k.Value)
		}
		if err := d.keys(v, k, where, "from", "commands"); err != nil {
			return err
		}
		var raw struct {
			From     string   `yaml:"from"`
			Commands []string `yaml:"commands"`
		}
		if err := d.decode(k, v, where, &raw); err != nil {
			return err
		}
		if raw.From == "" {
			return errf("%s: %s: from is required", d.at(k), where)
		}
		if len(raw.Commands) == 0 {
			return errf("%s: %s: commands must not be empty", d.at(k), where)
		}
		st := Stage{From: raw.From}
		for i, line := range raw.Commands {
			cmd, err := ParseCommand(line)
			if err != nil {
				return errf("%s: %s: command %d: %w", d.at(k), where, i+1, err)
			}
			switch c := cmd.(type) {
			case CopyCmd:
				if c.From != "" {
					return errf("%s: %s: copy --from is not supported in stages", d.at(k), where)
				}
			case RunCmd:
				for _, m := range c.Mounts {
					if m.Type == "bind" {
						return errf("%s: %s: --mount=type=bind is not supported in stages", d.at(k), where)
					}
				}
			}
			st.Commands = append(st.Commands, cmd)
		}
		r.Stages[k.Value] = st
		return nil
	})
}

// rawGroup is a group before its commands are parsed.
type rawGroup struct {
	key        *yaml.Node
	commands   []string
	contribute map[string][]string
	collect    string            // collect: run …
	fan        map[string]string // collect: {resolve, each, then}
}

func (g *rawGroup) isCollector() bool { return g.collect != "" || g.fan != nil }

// collectValue decodes the value of collect: a run command, or the
// mapping of a collector that fans out.
func (d *decoder) collectValue(k, v *yaml.Node, where string, g *rawGroup) error {
	if v.Kind != yaml.MappingNode {
		return d.decode(k, v, where, &g.collect)
	}
	if err := d.keys(v, k, where, "resolve", "each", "then"); err != nil {
		return err
	}
	if err := d.decode(k, v, where, &g.fan); err != nil {
		return err
	}
	if g.fan["resolve"] == "" || g.fan["each"] == "" {
		return errf("%s: %s: resolve and each are required", d.at(k), where)
	}
	return nil
}

// build decodes the phases. A phase is a mapping of groups, with env as
// the one key of its own; or, with collect, a single collector named
// after the phase.
func (d *decoder) build(key, n *yaml.Node, r *Recipe) error {
	if err := d.keys(n, key, "build"); err != nil {
		return err
	}
	type rawPhase struct {
		key    *yaml.Node
		env    map[string]string
		groups map[string]*rawGroup
		order  []string
	}
	var phases []rawPhase
	collectors := map[string]*yaml.Node{} // collector name → where it is defined
	err := pairs(n, func(k, v *yaml.Node) error {
		where := "build." + k.Value
		if k.Value == "" || strings.Contains(k.Value, "/") {
			return errf("%s: phase name %q is not allowed", d.at(k), k.Value)
		}
		if _, ok := r.Stages[k.Value]; ok {
			return errf("%s: %q is both a stage and a phase", d.at(k), k.Value)
		}
		if v.Kind == 0 || isNull(v) {
			v = &yaml.Node{Kind: yaml.MappingNode}
		}
		if err := d.keys(v, k, where); err != nil {
			return err
		}
		rp := rawPhase{key: k, groups: map[string]*rawGroup{}}
		if indexOf(v, keyCollect) >= 0 {
			// A phase that is one collector, named after the phase.
			if err := d.keys(v, k, where, keyCollect, keyEnv); err != nil {
				return err
			}
			g := &rawGroup{key: k}
			err := pairs(v, func(ck, cv *yaml.Node) error {
				if ck.Value == keyEnv {
					return d.decode(ck, cv, where+".env", &rp.env)
				}
				return d.collectValue(ck, cv, where+".collect", g)
			})
			if err != nil {
				return err
			}
			if prev, ok := collectors[k.Value]; ok {
				return errf("%s: a collector %q is already defined at %s", d.at(k), k.Value, d.at(prev))
			}
			collectors[k.Value] = k
			rp.groups[k.Value] = g
			rp.order = append(rp.order, k.Value)
			phases = append(phases, rp)
			return nil
		}
		err := pairs(v, func(gk, gv *yaml.Node) error {
			if gk.Value == keyEnv {
				return d.decode(gk, gv, where+".env", &rp.env)
			}
			g, err := d.group(gk, gv, where+"."+gk.Value)
			if err != nil {
				return err
			}
			if g.isCollector() {
				if prev, ok := collectors[gk.Value]; ok {
					return errf("%s: a collector %q is already defined at %s", d.at(gk), gk.Value, d.at(prev))
				}
				collectors[gk.Value] = gk
			}
			rp.groups[gk.Value] = g
			rp.order = append(rp.order, gk.Value)
			return nil
		})
		if err != nil {
			return err
		}
		if len(rp.order) == 0 {
			return errf("%s: %s: a phase needs groups or collect", d.at(k), where)
		}
		phases = append(phases, rp)
		return nil
	})
	if err != nil {
		return err
	}

	// Commands, contributions, then the collectors' items.
	items := map[string]map[string]bool{}
	earlier := map[string]bool{}
	for _, rp := range phases {
		if err := checkEnvNames(rp.env); err != nil {
			return errf("%s: build.%s.env: %w", d.at(rp.key), rp.key.Value, err)
		}
		ph := Phase{Name: rp.key.Value, Env: rp.env, Groups: map[string]Group{}}
		resolve := func(name string) (string, error) {
			if _, ok := r.Stages[name]; ok || earlier[name] {
				return name, nil
			}
			for _, p := range phases {
				if p.key.Value == name {
					return "", errf("from=%s: a phase can only use phases that come before it", name)
				}
			}
			return "", errf("from=%s: no stage or phase of that name", name)
		}
		for _, gname := range rp.order {
			rg := rp.groups[gname]
			where := fmt.Sprintf("%s: build.%s.%s", d.at(rg.key), ph.Name, gname)
			if gname == ph.Name && rg.isCollector() {
				where = fmt.Sprintf("%s: build.%s", d.at(rg.key), ph.Name)
			}
			grp := Group{}
			if rg.collect != "" {
				if !strings.Contains(rg.collect, itemsRef) {
					return errf("%s: collect must contain %s", where, itemsRef)
				}
				cmd, err := ParseCommand(rg.collect)
				if err != nil {
					return errf("%s: collect: %w", where, err)
				}
				run, ok := cmd.(RunCmd)
				if !ok {
					return errf("%s: collect must be a run command", where)
				}
				resolved, err := resolveSources(run, resolve)
				if err != nil {
					return errf("%s: collect: %w", where, err)
				}
				run = resolved.(RunCmd)
				grp.Collect = &run
			}
			if rg.fan != nil {
				fan := &Fanout{}
				for _, part := range []struct {
					key string
					dst *RunCmd
				}{{"resolve", &fan.Resolve}, {"each", &fan.Each}, {"then", &fan.Then}} {
					line := rg.fan[part.key]
					if line == "" {
						continue
					}
					if part.key == "resolve" && !strings.Contains(line, itemsRef) {
						return errf("%s: collect.resolve must contain %s", where, itemsRef)
					}
					if part.key != "resolve" && strings.Contains(line, itemsRef) {
						return errf("%s: collect.%s: %s is only for resolve", where, part.key, itemsRef)
					}
					cmd, err := ParseCommand(line)
					if err != nil {
						return errf("%s: collect.%s: %w", where, part.key, err)
					}
					run, ok := cmd.(RunCmd)
					if !ok {
						return errf("%s: collect.%s must be a run command", where, part.key)
					}
					resolved, err := resolveSources(run, resolve)
					if err != nil {
						return errf("%s: collect.%s: %w", where, part.key, err)
					}
					*part.dst = resolved.(RunCmd)
				}
				grp.Fanout = fan
			}
			for i, line := range rg.commands {
				cmd, err := ParseCommand(line)
				if err != nil {
					return errf("%s: command %d: %w", where, i+1, err)
				}
				if cmd, err = resolveSources(cmd, resolve); err != nil {
					return errf("%s: command %d: %w", where, i+1, err)
				}
				grp.Commands = append(grp.Commands, cmd)
			}
			for cname, list := range rg.contribute {
				if collectors[cname] == nil {
					return errf("%s: unknown key %q: no collector of that name (collectors: %s)", where, cname, collectorNames(collectors))
				}
				for _, it := range list {
					if !validItem(it) {
						return errf("%s: %s: %q is not a valid item (letters, digits and @%%+=:,./_- only, not starting with -)", where, cname, it)
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
		earlier[ph.Name] = true
	}
	// The items are sorted and unique, so that the command, and with it
	// the cache key, does not depend on the order of groups or files.
	for pi := range r.Phases {
		for gname, grp := range r.Phases[pi].Groups {
			if grp.Collect == nil && grp.Fanout == nil {
				continue
			}
			list := sortedKeys(items[gname])
			grp.Items = list
			if grp.Fanout != nil {
				if len(list) > 0 {
					f := *grp.Fanout
					f.Resolve.Script = strings.ReplaceAll(f.Resolve.Script, itemsRef, strings.Join(list, " "))
					f.Resolve.raw = strings.ReplaceAll(f.Resolve.raw, itemsRef, strings.Join(list, " "))
					grp.Fanout = &f
				}
			} else if len(list) > 0 {
				run := *grp.Collect
				run.Script = strings.ReplaceAll(run.Script, itemsRef, strings.Join(list, " "))
				run.raw = strings.ReplaceAll(run.raw, itemsRef, strings.Join(list, " "))
				grp.Commands = []Command{run}
			}
			r.Phases[pi].Groups[gname] = grp
		}
	}
	return nil
}

func collectorNames(m map[string]*yaml.Node) string {
	set := map[string]bool{}
	for k := range m {
		set[k] = true
	}
	if len(set) == 0 {
		return "none"
	}
	return joinSorted(set)
}

// group decodes one group: a list of commands, or a mapping with
// commands, collect and contributions.
func (d *decoder) group(k, v *yaml.Node, where string) (*rawGroup, error) {
	g := &rawGroup{key: k}
	switch v.Kind {
	case yaml.SequenceNode:
		return g, d.decode(k, v, where, &g.commands)
	case yaml.MappingNode:
	default:
		return nil, errf("%s: %s: expected a list of commands or a group mapping", d.at(k), where)
	}
	if err := d.keys(v, k, where); err != nil {
		return nil, err
	}
	err := pairs(v, func(gk, gv *yaml.Node) error {
		switch gk.Value {
		case "commands":
			return d.decode(gk, gv, where+".commands", &g.commands)
		case keyCollect:
			return d.collectValue(gk, gv, where+".collect", g)
		case keyEnv:
			return errf("%s: %s: env is a key of the phase; in a group use the env command", d.at(gk), where)
		}
		var list []string
		if err := gv.Decode(&list); err != nil {
			return errf("%s: %s.%s: a contribution must be a list of strings", d.at(gk), where, gk.Value)
		}
		if g.contribute == nil {
			g.contribute = map[string][]string{}
		}
		g.contribute[gk.Value] = list
		return nil
	})
	if err != nil {
		return nil, err
	}
	if g.isCollector() && (len(g.commands) > 0 || len(g.contribute) > 0) {
		return nil, errf("%s: %s: a collector has collect and nothing else", d.at(k), where)
	}
	if !g.isCollector() && len(g.commands) == 0 && len(g.contribute) == 0 {
		return nil, errf("%s: %s: needs commands, collect or a contribution to a collector", d.at(k), where)
	}
	return g, nil
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

func checkEnvNames(env map[string]string) error {
	for k := range env {
		if !validEnvName(k) {
			return errf("%q is not a valid variable name", k)
		}
	}
	return nil
}

// resolveSources checks the stage and phase names in a command.
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
