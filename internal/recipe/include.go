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

// flatPhase is a phase with all includes resolved.
type flatPhase struct {
	Name       string
	Validation string
	Shared     []string
	Groups     map[string][]string
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

// phases flattens a phase list. An item `- include: <file>` is replaced by
// the phases of that file, which holds either one phase or a list of phases.
func (l *loader) phases(from string, in []rawPhase) ([]flatPhase, error) {
	var out []flatPhase
	for i, rp := range in {
		if rp.Include != "" {
			if rp.Name != "" || rp.Validation != "" || len(rp.Shared) > 0 || len(rp.Groups) > 0 {
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
		fp := flatPhase{Name: rp.Name, Validation: rp.Validation, Shared: rp.Shared, Groups: map[string][]string{}}
		for g, cl := range rp.Groups {
			lines, err := l.commands(from, cl)
			if err != nil {
				return nil, err
			}
			fp.Groups[g] = lines
		}
		out = append(out, fp)
	}
	return out, nil
}
