package recipe

import (
	"bytes"
	"fmt"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

// ReadFunc loads a recipe file by its slash-separated path relative to the
// recipe root (the directory of the entry file).
type ReadFunc func(name string) ([]byte, error)

// One rule for include: `include: <file>` stands in place of a phase, a
// group or a command list and is replaced by the content of the file.
// Included files may include further files.

// cmdList is a command list in the YAML: either a sequence of command
// strings, or `include: <file>` pointing at a file that holds such a
// sequence.
type cmdList struct {
	Lines   []string
	Include string
}

func (c *cmdList) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.SequenceNode:
		return n.Decode(&c.Lines)
	case yaml.MappingNode:
		var m struct {
			Include string `yaml:"include"`
		}
		if err := decodeStrict(n, &m); err != nil {
			return err
		}
		if m.Include == "" {
			return fmt.Errorf("line %d: expected a command list or `include: <file>`", n.Line)
		}
		c.Include = m.Include
		return nil
	}
	return fmt.Errorf("line %d: expected a command list or `include: <file>`", n.Line)
}

// Keys with a fixed meaning in the long form of a group. Every other key
// is a contribution to the collector phase of that name.
const (
	keyCommands = "commands"
	keyPrepare  = "prepare"
	keyExpects  = "expects"
	keyInclude  = "include"
)

// ReservedGroupKey reports whether name has a fixed meaning in a group and
// therefore cannot name a collector phase.
func ReservedGroupKey(name string) bool {
	switch name {
	case keyCommands, keyPrepare, keyExpects, keyInclude:
		return true
	}
	return false
}

// rawGroup is a group in the YAML. Short form: a sequence of commands.
// Long form: a mapping with commands, prepare, expects and contributions,
// or with nothing but `include: <file>`.
type rawGroup struct {
	Commands   cmdList
	Prepare    map[string]rawStage
	Expects    []string
	Contribute map[string][]string
	Include    string
}

func (g *rawGroup) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.SequenceNode:
		return n.Decode(&g.Commands.Lines)
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			var err error
			switch k.Value {
			case keyInclude:
				err = v.Decode(&g.Include)
				if err == nil && g.Include == "" {
					err = fmt.Errorf("include needs a file name")
				}
			case keyCommands:
				err = v.Decode(&g.Commands)
			case keyPrepare:
				g.Prepare = map[string]rawStage{}
				if v.Kind != yaml.MappingNode {
					err = fmt.Errorf("expected a mapping of stage names")
					break
				}
				for j := 0; j+1 < len(v.Content) && err == nil; j += 2 {
					var st rawStage
					if err = decodeStrict(v.Content[j+1], &st); err == nil {
						g.Prepare[v.Content[j].Value] = st
					}
				}
			case keyExpects:
				err = v.Decode(&g.Expects)
			default:
				var items []string
				if err = v.Decode(&items); err != nil {
					err = fmt.Errorf("a contribution must be a list of strings")
					break
				}
				if g.Contribute == nil {
					g.Contribute = map[string][]string{}
				}
				g.Contribute[k.Value] = items
			}
			if err != nil {
				return fmt.Errorf("line %d: %s: %w", k.Line, k.Value, err)
			}
		}
		if g.Include != "" && len(n.Content) != 2 {
			return fmt.Errorf("line %d: include cannot be combined with other keys", n.Line)
		}
		return nil
	}
	return fmt.Errorf("line %d: expected a command list or a group mapping", n.Line)
}

// decodeStrict decodes a node rejecting unknown keys (Node.Decode does not
// inherit the decoder's KnownFields setting).
func decodeStrict(n *yaml.Node, out any) error {
	buf, err := yaml.Marshal(n)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(buf))
	dec.KnownFields(true)
	return dec.Decode(out)
}

// flatStage, flatGroup and flatPhase are the shapes with all includes
// resolved.
type flatStage struct {
	From  string
	Lines []string
}

type flatGroup struct {
	Lines      []string
	Prepare    map[string]flatStage
	Expects    []string
	Contribute map[string][]string
}

type flatPhase struct {
	Name       string
	Validation string
	Shared     []string
	Env        map[string]string
	Collect    string
	Groups     map[string]flatGroup
}

// loader resolves includes. Paths are relative to the including file, must
// stay inside the recipe root, and must not form a cycle.
type loader struct {
	read  ReadFunc
	stack []string
}

func (l *loader) resolve(from, inc string) (string, error) {
	if strings.HasPrefix(inc, "/") {
		return "", fmt.Errorf("%s: include %q must be a relative path", from, inc)
	}
	p := path.Clean(path.Join(path.Dir(from), inc))
	if p == ".." || strings.HasPrefix(p, "../") {
		return "", fmt.Errorf("%s: include %q escapes the recipe directory", from, inc)
	}
	return p, nil
}

// open reads an included file and runs fn with it on the include stack.
func (l *loader) open(from, inc string, fn func(name string, data []byte) error) error {
	name, err := l.resolve(from, inc)
	if err != nil {
		return err
	}
	for _, s := range l.stack {
		if s == name {
			return fmt.Errorf("%s: include cycle: %s -> %s", from, strings.Join(l.stack, " -> "), name)
		}
	}
	data, err := l.read(name)
	if err != nil {
		return fmt.Errorf("%s: include %q: %w", from, inc, err)
	}
	l.stack = append(l.stack, name)
	defer func() { l.stack = l.stack[:len(l.stack)-1] }()
	return fn(name, data)
}

// commands flattens a command list, following includes.
func (l *loader) commands(from string, c cmdList) ([]string, error) {
	if c.Include == "" {
		return c.Lines, nil
	}
	var out []string
	err := l.open(from, c.Include, func(name string, data []byte) error {
		var inner cmdList
		if err := yaml.Unmarshal(data, &inner); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		lines, err := l.commands(name, inner)
		out = lines
		return err
	})
	return out, err
}

// stage flattens a prepare stage.
func (l *loader) stage(from string, rs rawStage) (flatStage, error) {
	lines, err := l.commands(from, rs.Commands)
	return flatStage{From: rs.From, Lines: lines}, err
}

// group flattens a group. An included file holds a group in either form,
// so a feature file can bring its own stages, expectations and
// contributions.
func (l *loader) group(from string, g rawGroup) (flatGroup, error) {
	if g.Include != "" {
		var out flatGroup
		err := l.open(from, g.Include, func(name string, data []byte) error {
			var inner rawGroup
			if err := yaml.Unmarshal(data, &inner); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			fg, err := l.group(name, inner)
			out = fg
			return err
		})
		return out, err
	}
	lines, err := l.commands(from, g.Commands)
	if err != nil {
		return flatGroup{}, err
	}
	fg := flatGroup{Lines: lines, Expects: g.Expects, Contribute: g.Contribute, Prepare: map[string]flatStage{}}
	for name, rs := range g.Prepare {
		st, err := l.stage(from, rs)
		if err != nil {
			return flatGroup{}, fmt.Errorf("prepare.%s: %w", name, err)
		}
		fg.Prepare[name] = st
	}
	return fg, nil
}

// phases flattens a phase list. An item `- include: <file>` is replaced by
// the phases of that file, which holds either one phase or a list of phases.
func (l *loader) phases(from string, in []rawPhase) ([]flatPhase, error) {
	var out []flatPhase
	for i, rp := range in {
		if rp.Include != "" {
			if rp.Name != "" || rp.Validation != "" || rp.Collect != "" || len(rp.Shared) > 0 || len(rp.Env) > 0 || len(rp.Groups) > 0 {
				return nil, fmt.Errorf("%s: phases[%d]: include cannot be combined with other keys", from, i)
			}
			err := l.open(from, rp.Include, func(name string, data []byte) error {
				var doc yaml.Node
				if err := yaml.Unmarshal(data, &doc); err != nil {
					return fmt.Errorf("%s: %w", name, err)
				}
				var list []rawPhase
				if len(doc.Content) == 1 && doc.Content[0].Kind == yaml.MappingNode {
					var one rawPhase
					if err := decodeStrict(doc.Content[0], &one); err != nil {
						return fmt.Errorf("%s: %w", name, err)
					}
					list = []rawPhase{one}
				} else if len(doc.Content) == 1 {
					if err := decodeStrict(doc.Content[0], &list); err != nil {
						return fmt.Errorf("%s: %w", name, err)
					}
				}
				if len(list) == 0 {
					return fmt.Errorf("%s: expected a phase or a list of phases", name)
				}
				sub, err := l.phases(name, list)
				out = append(out, sub...)
				return err
			})
			if err != nil {
				return nil, err
			}
			continue
		}
		fp := flatPhase{Name: rp.Name, Validation: rp.Validation, Shared: rp.Shared, Env: rp.Env, Collect: rp.Collect, Groups: map[string]flatGroup{}}
		for g, rg := range rp.Groups {
			fg, err := l.group(from, rg)
			if err != nil {
				return nil, fmt.Errorf("%s: group %q: %w", from, g, err)
			}
			fp.Groups[g] = fg
		}
		out = append(out, fp)
	}
	return out, nil
}
