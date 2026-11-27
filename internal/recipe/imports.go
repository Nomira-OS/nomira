package recipe

import (
	"bytes"
	"fmt"
	"net/url"
	"path"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ReadFunc loads a recipe file by its slash-separated path relative to the
// recipe root (the directory of the entry file).
type ReadFunc func(name string) ([]byte, error)

// Imports
//
// A recipe file may start with `imports:`, a list of further recipe files:
// paths relative to the importing file, or URLs. An https URL may be
// pinned with #sha256=<hex>; an http URL must be, since nothing else
// vouches for what it serves. A file read from a URL imports relative to
// its URL.
// Each of them is a recipe of its own (with imports of its own) or a part
// of one. The files are merged into one document, in this order: the
// imports in the order they are listed, each with its own imports first,
// and the importing file last. Merging follows JSON Merge Patch (RFC 7386):
//
//   - mappings are merged key by key;
//   - every other value (strings, numbers, lists) is replaced by the later
//     file's;
//   - `key: null` removes the key.
//
// The first file that names a key decides its place; new keys come after
// the existing ones. Under `build` the order is the order of the phases,
// so there a new phase is placed after the phase that comes before it in
// its own file (before the next one it names, if it is first there), and
// two files that order the same phases differently are an error.

const keyImports = "imports"

// document is the merged recipe: the root mapping, and for every mapping
// key the files that set it.
type document struct {
	root   *yaml.Node
	from   map[*yaml.Node][]string
	files  []string // every file read, entry first
	syntax string   // the `# syntax=` line of the entry file, if any
	head   string   // the rest of the entry file's head comment
}

type loader struct {
	read  ReadFunc
	doc   *document
	stack []string
	seen  map[string]bool
}

// loadDocument reads entry and everything it imports and merges it.
func loadDocument(entry string, read ReadFunc) (*document, error) {
	l := &loader{read: read, doc: &document{from: map[*yaml.Node][]string{}}, seen: map[string]bool{}}
	root, err := l.load(entry)
	if err != nil {
		return nil, err
	}
	l.doc.root = root
	return l.doc, nil
}

func (l *loader) load(name string) (*yaml.Node, error) {
	for _, s := range l.stack {
		if s == name {
			return nil, fmt.Errorf("import cycle: %s -> %s", strings.Join(l.stack, " -> "), name)
		}
	}
	data, err := l.read(name)
	if err != nil {
		return nil, err
	}
	if !l.seen[name] {
		l.seen[name] = true
		l.doc.files = append(l.doc.files, name)
	}
	if len(l.stack) == 0 {
		if first, _, _ := strings.Cut(string(data), "\n"); strings.HasPrefix(first, "# syntax=") {
			l.doc.syntax = strings.TrimSpace(first)
		}
	}
	own, head, err := parseMapping(name, data, len(l.stack) == 0)
	if err != nil {
		return nil, err
	}
	if len(l.stack) == 0 {
		var keep []string
		for _, line := range strings.Split(head, "\n") {
			if !strings.HasPrefix(line, "# syntax=") {
				keep = append(keep, line)
			}
		}
		l.doc.head = strings.TrimSpace(strings.Join(keep, "\n"))
	}
	if err := l.mark(own, name); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}

	// Take the imports out of the file and merge them first.
	var imports []string
	for i := 0; i+1 < len(own.Content); i += 2 {
		if own.Content[i].Value != keyImports {
			continue
		}
		if err := own.Content[i+1].Decode(&imports); err != nil {
			return nil, fmt.Errorf("%s: line %d: imports: expected a list of files", name, own.Content[i].Line)
		}
		own.Content = append(own.Content[:i], own.Content[i+2:]...)
		break
	}
	l.stack = append(l.stack, name)
	defer func() { l.stack = l.stack[:len(l.stack)-1] }()
	merged := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for _, imp := range imports {
		p, err := resolveImport(name, imp)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		sub, err := l.load(p)
		if err != nil {
			return nil, fmt.Errorf("%s: import %q: %w", name, imp, err)
		}
		if err := l.merge(merged, sub, nil); err != nil {
			return nil, fmt.Errorf("%s: import %q: %w", name, imp, err)
		}
	}
	if err := l.merge(merged, own, nil); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return merged, nil
}

// parseMapping parses a recipe file and returns its root mapping and its
// head comment, which the root no longer carries.
//
// The comment on top of an imported file (entry false) describes that
// file; it is dropped rather than left on whatever key it ends up above.
func parseMapping(name string, data []byte, entry bool) (*yaml.Node, string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, "", fmt.Errorf("%s: %w", name, err)
	}
	if len(doc.Content) == 0 {
		return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}, "", nil
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, "", fmt.Errorf("%s: line %d: expected a mapping (imports, settings, stages, build)", name, root.Line)
	}
	head := strings.TrimSpace(doc.HeadComment + "\n" + root.HeadComment)
	root.HeadComment, doc.HeadComment = "", ""
	// yaml.v3 gives the comment on top of the file to the first key, unless
	// a blank line separates them. imports goes away, so its comment joins
	// the head; a syntax line is never left on a key.
	if len(root.Content) > 0 {
		k := root.Content[0]
		var keep []string
		for _, line := range strings.Split(k.HeadComment, "\n") {
			if !strings.HasPrefix(line, "# syntax=") {
				keep = append(keep, line)
			}
		}
		k.HeadComment = strings.TrimSpace(strings.Join(keep, "\n"))
		if k.Value == keyImports || !entry {
			head = strings.TrimSpace(head + "\n" + k.HeadComment)
			k.HeadComment = ""
		}
	}
	return root, head, nil
}

// mark records name as the source of every mapping key in n, and rejects
// keys that appear twice in one mapping.
func (l *loader) mark(n *yaml.Node, name string) error {
	switch n.Kind {
	case yaml.MappingNode:
		seen := map[string]bool{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			if seen[k.Value] {
				return fmt.Errorf("line %d: %q is defined twice", k.Line, k.Value)
			}
			seen[k.Value] = true
			l.doc.from[k] = []string{name}
			if err := l.mark(n.Content[i+1], name); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for _, c := range n.Content {
			if err := l.mark(c, name); err != nil {
				return err
			}
		}
	}
	return nil
}

// resolveImport returns the name of import imp of file from: a path
// relative to the recipe directory, or a URL. A relative import of a file
// read from a URL is resolved against that URL. A URL may end in
// #sha256=<hex>, which the reader checks.
func resolveImport(from, imp string) (string, error) {
	if IsURL(imp) {
		_, sum, err := SplitPin(imp)
		if err != nil {
			return "", err
		}
		if sum == "" && strings.HasPrefix(imp, "http://") {
			return "", fmt.Errorf("import %q: an http URL needs a #sha256= pin; or use https", imp)
		}
		return imp, nil
	}
	if imp == "" || strings.HasPrefix(imp, "/") {
		return "", fmt.Errorf("import %q must be a relative path or a URL", imp)
	}
	if IsURL(from) {
		base, _, _ := SplitPin(from)
		u, err := url.Parse(base)
		if err != nil {
			return "", err
		}
		ref, err := url.Parse(imp)
		if err != nil {
			return "", fmt.Errorf("import %q: %w", imp, err)
		}
		abs := u.ResolveReference(ref).String()
		if _, sum, err := SplitPin(abs); err != nil {
			return "", err
		} else if sum == "" && strings.HasPrefix(abs, "http://") {
			return "", fmt.Errorf("import %q: an http URL needs a #sha256= pin; or use https", imp)
		}
		return abs, nil
	}
	p := path.Clean(path.Join(path.Dir(from), imp))
	if p == ".." || strings.HasPrefix(p, "../") {
		return "", fmt.Errorf("import %q is outside the recipe directory", imp)
	}
	return p, nil
}

// IsURL reports whether an import names a file on the web.
func IsURL(name string) bool {
	return strings.HasPrefix(name, "https://") || strings.HasPrefix(name, "http://")
}

// SplitPin splits an import URL into the URL to fetch and the sha256 the
// content must have ("" when the URL has no #sha256=<hex>).
func SplitPin(name string) (u, sha256 string, err error) {
	u, frag, ok := strings.Cut(name, "#")
	if !ok {
		return name, "", nil
	}
	sum, ok := strings.CutPrefix(frag, "sha256=")
	if !ok || len(sum) != 64 || strings.Trim(sum, "0123456789abcdef") != "" {
		return "", "", fmt.Errorf("import %q: the fragment must be #sha256= and 64 lowercase hex digits", name)
	}
	return u, sum, nil
}

func isNull(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.Tag == "!!null"
}

// merge merges overlay into base (both mappings). keyPath is the path of
// base below the root.
func (l *loader) merge(base, overlay *yaml.Node, keyPath []string) error {
	if len(keyPath) == 1 && keyPath[0] == keyBuild {
		return l.mergeOrdered(base, overlay, keyPath)
	}
	for i := 0; i+1 < len(overlay.Content); i += 2 {
		k, v := overlay.Content[i], overlay.Content[i+1]
		j := indexOf(base, k.Value)
		switch {
		case isNull(v):
			if j >= 0 {
				base.Content = append(base.Content[:j], base.Content[j+2:]...)
			}
		case j < 0:
			base.Content = append(base.Content, k, v)
		default:
			if err := l.mergeValue(base, j, k, v, keyPath); err != nil {
				return err
			}
		}
	}
	return nil
}

// mergeValue merges v into the value at base.Content[j+1].
func (l *loader) mergeValue(base *yaml.Node, j int, k, v *yaml.Node, keyPath []string) error {
	bk, bv := base.Content[j], base.Content[j+1]
	if bv.Kind == yaml.MappingNode && v.Kind == yaml.MappingNode {
		l.doc.from[bk] = union(l.doc.from[bk], l.doc.from[k])
		return l.merge(bv, v, append(append([]string{}, keyPath...), k.Value))
	}
	// Replaced: the later file's key, with its comments and origin.
	base.Content[j], base.Content[j+1] = k, v
	return nil
}

// mergeOrdered merges the phases under build: their order matters.
func (l *loader) mergeOrdered(base, overlay *yaml.Node, keyPath []string) error {
	last := -1 // position in base of the previous phase of overlay
	for i := 0; i+1 < len(overlay.Content); i += 2 {
		k, v := overlay.Content[i], overlay.Content[i+1]
		j := indexOf(base, k.Value)
		if isNull(v) {
			if j >= 0 {
				base.Content = append(base.Content[:j], base.Content[j+2:]...)
				if j < last {
					last -= 2
				}
			}
			continue
		}
		if j >= 0 {
			if j < last {
				prev := base.Content[last].Value
				return fmt.Errorf("build: the phases %q and %q are in a different order in %s than in %s",
					prev, k.Value, strings.Join(l.doc.from[k], ", "), strings.Join(l.doc.from[base.Content[j]], ", "))
			}
			if err := l.mergeValue(base, j, k, v, keyPath); err != nil {
				return err
			}
			last = j
			continue
		}
		at := last + 2
		if last < 0 {
			// First in its file: before the next phase it names that is
			// already there, otherwise at the end.
			at = len(base.Content)
			for n := i + 2; n+1 < len(overlay.Content); n += 2 {
				if p := indexOf(base, overlay.Content[n].Value); p >= 0 {
					at = p
					break
				}
			}
		}
		base.Content = append(base.Content[:at], append([]*yaml.Node{k, v}, base.Content[at:]...)...)
		last = at
	}
	return nil
}

func indexOf(m *yaml.Node, key string) int {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return i
		}
	}
	return -1
}

func union(a, b []string) []string {
	out := append([]string{}, a...)
	for _, s := range b {
		found := false
		for _, t := range out {
			found = found || t == s
		}
		if !found {
			out = append(out, s)
		}
	}
	return out
}

// resolved renders the merged recipe: the syntax line, then the document.
// Settings, stages, phases and groups that come from other files than the
// entry carry a comment naming them.
func (d *document) resolved(entry string) ([]byte, error) {
	note := func(k, v *yaml.Node) {
		from := d.from[k]
		if len(from) == 1 && from[0] == entry || len(from) == 0 {
			return
		}
		// On the key when its value is a block below it, else behind the
		// value (yaml.v3 misplaces a key's comment before a flow value).
		if v.Kind == yaml.MappingNode || v.Kind == yaml.SequenceNode && v.Style&yaml.FlowStyle == 0 {
			k.LineComment = "from " + strings.Join(from, ", ")
		} else {
			v.LineComment = "from " + strings.Join(from, ", ")
		}
	}
	for i := 0; i+1 < len(d.root.Content); i += 2 {
		k, v := d.root.Content[i], d.root.Content[i+1]
		if v.Kind != yaml.MappingNode {
			continue
		}
		for j := 0; j+1 < len(v.Content); j += 2 {
			note(v.Content[j], v.Content[j+1])
			if k.Value != keyBuild {
				continue
			}
			if ph := v.Content[j+1]; ph.Kind == yaml.MappingNode {
				for n := 0; n+1 < len(ph.Content); n += 2 {
					note(ph.Content[n], ph.Content[n+1])
				}
			}
		}
	}
	blockStyle(d.root)
	canonicalOrder(d.root)
	var buf bytes.Buffer
	if d.syntax != "" {
		buf.WriteString(d.syntax + "\n")
	}
	files := append([]string{}, d.files...)
	sort.Strings(files[1:])
	fmt.Fprintf(&buf, "# Resolved by nomira from %s.\n", strings.Join(files, ", "))
	if d.head != "" {
		buf.WriteString(d.head + "\n")
	}
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(d.root); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

const (
	keyStages = "stages"
	keyBuild  = "build"
)

// blockStyle writes every mapping in block style: comments only work
// there, and one style for every recipe keeps diffs between resolved
// recipes small. Lists keep the style they were written in.
func blockStyle(n *yaml.Node) {
	if n.Kind == yaml.MappingNode {
		n.Style &^= yaml.FlowStyle
	}
	for _, c := range n.Content {
		blockStyle(c)
	}
}

// canonicalOrder puts the top-level keys in the order a recipe is read:
// settings, stages, build. Below them the order of the files is kept.
func canonicalOrder(root *yaml.Node) {
	rank := map[string]int{"settings": 0, keyStages: 1, keyBuild: 2}
	type pair struct{ k, v *yaml.Node }
	var pairs []pair
	for i := 0; i+1 < len(root.Content); i += 2 {
		pairs = append(pairs, pair{root.Content[i], root.Content[i+1]})
	}
	sort.SliceStable(pairs, func(a, b int) bool {
		return rank[pairs[a].k.Value] < rank[pairs[b].k.Value]
	})
	root.Content = root.Content[:0]
	for _, p := range pairs {
		root.Content = append(root.Content, p.k, p.v)
	}
}
