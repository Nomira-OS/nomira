// Package conflict checks that the groups of one phase are independent:
// that merging their layers gives what running them one after the other
// would, in any order. It is pure (no BuildKit dependency): write sets
// are collected elsewhere and handed in as parsed entries.
//
// The check runs in two passes. Pass 1 lists what each group writes and
// deletes, without reading a file. Only the paths that more than one
// group writes (Overlaps) are hashed in pass 2, and their tokens are
// added with SetTokens. Check then allows exactly what is the same in
// every order:
//
//   - two groups write a path with the same content, mode and owner;
//   - two groups delete the same file, or the same directory;
//   - a group deletes something below a directory another one deletes.
//
// Everything else that two groups both touch is a conflict: different
// writes to one path, a write against a delete, a write below a directory
// another group deletes.
package conflict

import (
	"fmt"
	"sort"
	"strings"
)

// Kind of a write-set entry.
type Kind byte

const (
	Write     Kind = 'W' // file/symlink added or changed (content or metadata)
	Delete    Kind = 'D' // file/symlink deleted
	DeleteDir Kind = 'R' // directory deleted → prefix semantics for everything below
)

// Entry is one line of a group's write set. Path is relative, no leading
// slash. Token identifies content and metadata (sha256:mode:uid:gid) of a
// Write; it is only known for paths that were hashed in pass 2.
type Entry struct {
	Kind  Kind
	Path  string
	Token string
}

// ParseWriteSet parses the output of pass 1 (compile/lister.sh):
//
//	W <path>
//	D <path>        (file deletion)
//	D <path>/       (directory deletion → DeleteDir)
func ParseWriteSet(data string) ([]Entry, error) {
	var out []Entry
	for ln, line := range strings.Split(data, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		kind, path, ok := strings.Cut(line, " ")
		if !ok || path == "" {
			return nil, fmt.Errorf("writeset line %d: malformed %q", ln+1, line)
		}
		switch kind {
		case "W":
			out = append(out, Entry{Kind: Write, Path: path})
		case "D":
			if strings.HasSuffix(path, "/") {
				out = append(out, Entry{Kind: DeleteDir, Path: strings.TrimSuffix(path, "/")})
			} else {
				out = append(out, Entry{Kind: Delete, Path: path})
			}
		default:
			return nil, fmt.Errorf("writeset line %d: malformed %q", ln+1, line)
		}
	}
	return out, nil
}

// ParseHashes parses the output of pass 2 (compile/hash.sh), "H <token>
// <path>" per line, into path → token.
func ParseHashes(data string) (map[string]string, error) {
	out := map[string]string{}
	for ln, line := range strings.Split(data, "\n") {
		if line == "" {
			continue
		}
		rest, ok := strings.CutPrefix(line, "H ")
		tok, path, ok2 := strings.Cut(rest, " ")
		if !ok || !ok2 || path == "" {
			return nil, fmt.Errorf("hashes line %d: malformed %q", ln+1, line)
		}
		out[path] = tok
	}
	return out, nil
}

// Overlaps returns, for each group, the paths it writes that another
// group writes too, sorted: what pass 2 has to hash.
func Overlaps(sets map[string][]Entry) map[string][]string {
	writers := map[string]int{}
	for _, set := range sets {
		for _, e := range set {
			if e.Kind == Write {
				writers[e.Path]++
			}
		}
	}
	out := map[string][]string{}
	for g, set := range sets {
		for _, e := range set {
			if e.Kind == Write && writers[e.Path] > 1 {
				out[g] = append(out[g], e.Path)
			}
		}
		sort.Strings(out[g])
	}
	for g, paths := range out {
		if len(paths) == 0 {
			delete(out, g)
		}
	}
	return out
}

// SetTokens adds the tokens of pass 2 to the writes of one group.
func SetTokens(set []Entry, tokens map[string]string) {
	for i, e := range set {
		if e.Kind == Write {
			if tok, ok := tokens[e.Path]; ok {
				set[i].Token = tok
			}
		}
	}
}

// Conflict describes one violation between two groups.
type Conflict struct {
	GroupA, GroupB string
	Path           string // path as written (dir deletions keep trailing /)
	Reason         string
}

// Check verifies that the groups of a phase are independent, see the
// package documentation.
func Check(phase string, sets map[string][]Entry) error {
	groups := make([]string, 0, len(sets))
	for g := range sets {
		groups = append(groups, g)
	}
	sort.Strings(groups)

	var conflicts []Conflict
	for i := 0; i < len(groups); i++ {
		for j := i + 1; j < len(groups); j++ {
			conflicts = append(conflicts, checkPair(groups[i], sets[groups[i]], groups[j], sets[groups[j]])...)
		}
	}
	if len(conflicts) == 0 {
		return nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "phase %q: its groups are not independent:\n", phase)
	const cap = 20
	for k, c := range conflicts {
		if k == cap {
			fmt.Fprintf(&b, "  … and %d more\n", len(conflicts)-cap)
			break
		}
		fmt.Fprintf(&b, "  groups %q and %q: /%s (%s)\n", c.GroupA, c.GroupB, c.Path, c.Reason)
	}
	b.WriteString("  fix: make it one group, or move one of them into a phase of its own")
	return fmt.Errorf("%s", b.String())
}

func checkPair(ga string, sa []Entry, gb string, sb []Entry) []Conflict {
	var out []Conflict
	emit := func(path, reason string) {
		out = append(out, Conflict{GroupA: ga, GroupB: gb, Path: path, Reason: reason})
	}

	bByPath := map[string]Entry{}
	for _, e := range sb {
		bByPath[e.Path] = e
	}
	for _, ea := range sa {
		eb, ok := bByPath[ea.Path]
		if !ok {
			continue
		}
		switch {
		case ea.Kind == Write && eb.Kind == Write:
			if ea.Token == "" || ea.Token != eb.Token {
				emit(ea.Path, "both write, with different content, mode or owner")
			}
		case ea.Kind == eb.Kind:
			// The same deletion twice.
		default:
			emit(ea.Path, fmt.Sprintf("%s vs %s", verb(ea.Kind), verb(eb.Kind)))
		}
	}
	// A write below a directory the other group deletes. A deletion there
	// is the same in every order.
	below := func(deleter string, dels, writes []Entry) {
		for _, d := range dels {
			if d.Kind != DeleteDir {
				continue
			}
			for _, w := range writes {
				if w.Kind == Write && strings.HasPrefix(w.Path, d.Path+"/") {
					emit(w.Path, fmt.Sprintf("group %q deletes directory /%s", deleter, d.Path))
				}
			}
		}
	}
	below(ga, sa, sb)
	below(gb, sb, sa)
	return out
}

func verb(k Kind) string {
	switch k {
	case Write:
		return "write"
	case Delete:
		return "delete"
	case DeleteDir:
		return "delete directory"
	}
	return "?"
}
