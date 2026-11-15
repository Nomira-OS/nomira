// Package conflict checks write-set disjointness between the groups of one
// phase. It is pure (no BuildKit dependency): write sets are collected
// elsewhere and handed in as parsed entries.
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
// slash. Token identifies content+metadata (sha256:mode:uid:gid) for Write
// entries and is empty for deletions.
type Entry struct {
	Kind  Kind
	Path  string
	Token string
}

// ParseWriteSet parses the lister script's output:
//
//	W <token> <path>
//	D <path>        (file deletion)
//	D <path>/       (directory deletion → DeleteDir)
func ParseWriteSet(data string) ([]Entry, error) {
	var out []Entry
	for ln, line := range strings.Split(data, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "W "):
			rest := line[2:]
			tok, path, ok := strings.Cut(rest, " ")
			if !ok || path == "" {
				return nil, fmt.Errorf("writeset line %d: malformed %q", ln+1, line)
			}
			out = append(out, Entry{Kind: Write, Path: path, Token: tok})
		case strings.HasPrefix(line, "D "):
			path := line[2:]
			if path == "" {
				return nil, fmt.Errorf("writeset line %d: malformed %q", ln+1, line)
			}
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

// Policy values (mirrors recipe package to avoid the import).
const (
	Strict         = "strict"
	AllowIdentical = "allow-identical"
)

// Conflict describes one violation between two groups.
type Conflict struct {
	GroupA, GroupB string
	Path           string // path as written (dir deletions keep trailing /)
	Reason         string
}

// Check verifies pairwise disjointness of the groups' write sets under the
// given policy. shared is the phase's allowlist: entries are absolute or
// relative paths; a trailing "/" makes an entry a prefix.
func Check(phase, policy string, shared []string, sets map[string][]Entry) error {
	allow := newAllowlist(shared)

	groups := make([]string, 0, len(sets))
	for g := range sets {
		groups = append(groups, g)
	}
	sort.Strings(groups)

	var conflicts []Conflict
	for i := 0; i < len(groups); i++ {
		for j := i + 1; j < len(groups); j++ {
			conflicts = append(conflicts, checkPair(policy, allow, groups[i], sets[groups[i]], groups[j], sets[groups[j]])...)
		}
	}
	if len(conflicts) == 0 {
		return nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "phase %q: declared-independent groups are not disjoint (policy: %s):\n", phase, policy)
	max := len(conflicts)
	const cap = 20
	for k, c := range conflicts {
		if k == cap {
			fmt.Fprintf(&b, "  … and %d more\n", max-cap)
			break
		}
		fmt.Fprintf(&b, "  groups %q and %q: /%s (%s)\n", c.GroupA, c.GroupB, c.Path, c.Reason)
	}
	b.WriteString("  fix: serialize into one group/phase, or add the path to phases[].shared to allow it intentionally")
	return fmt.Errorf("%s", b.String())
}

func checkPair(policy string, allow allowlist, ga string, sa []Entry, gb string, sb []Entry) []Conflict {
	var out []Conflict

	// Index of exact paths in B.
	bByPath := map[string]Entry{}
	for _, e := range sb {
		bByPath[e.Path] = e
	}

	emit := func(path, reason string) {
		out = append(out, Conflict{GroupA: ga, GroupB: gb, Path: path, Reason: reason})
	}

	for _, ea := range sa {
		if allow.covers(ea.Path) {
			continue
		}
		// Exact-path collisions.
		if eb, ok := bByPath[ea.Path]; ok {
			switch {
			case ea.Kind == Write && eb.Kind == Write:
				if policy == AllowIdentical && ea.Token == eb.Token {
					// identical content+metadata → tolerated
				} else if policy == AllowIdentical {
					emit(ea.Path, "both write, contents differ")
				} else {
					emit(ea.Path, "both write")
				}
			case ea.Kind == Delete && eb.Kind == Delete:
				if policy != AllowIdentical {
					emit(ea.Path, "both delete")
				}
			case ea.Kind == DeleteDir && eb.Kind == DeleteDir:
				if policy != AllowIdentical {
					emit(ea.Path+"/", "both delete directory")
				}
			default:
				emit(ea.Path, fmt.Sprintf("%s vs %s", verb(ea.Kind), verb(eb.Kind)))
			}
		}
		// Directory deletion in A vs anything underneath in B (prefix).
		if ea.Kind == DeleteDir {
			prefix := ea.Path + "/"
			for _, eb := range sb {
				if eb.Path != ea.Path && strings.HasPrefix(eb.Path, prefix) && !allow.covers(eb.Path) {
					emit(eb.Path, fmt.Sprintf("group %q deletes directory /%s", ga, ea.Path))
				}
			}
		}
	}
	// Directory deletion in B vs anything underneath in A (prefix, other direction).
	for _, eb := range sb {
		if eb.Kind != DeleteDir || allow.covers(eb.Path) {
			continue
		}
		prefix := eb.Path + "/"
		for _, ea := range sa {
			if ea.Path != eb.Path && strings.HasPrefix(ea.Path, prefix) && !allow.covers(ea.Path) {
				out = append(out, Conflict{GroupA: ga, GroupB: gb, Path: ea.Path,
					Reason: fmt.Sprintf("group %q deletes directory /%s", gb, eb.Path)})
			}
		}
	}
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

// ---- allowlist ------------------------------------------------------------

type allowlist struct {
	exact    map[string]bool
	prefixes []string
}

func newAllowlist(shared []string) allowlist {
	a := allowlist{exact: map[string]bool{}}
	for _, s := range shared {
		s = strings.TrimPrefix(s, "/")
		if s == "" {
			continue
		}
		if strings.HasSuffix(s, "/") {
			a.prefixes = append(a.prefixes, s)
		} else {
			a.exact[s] = true
		}
	}
	return a
}

func (a allowlist) covers(path string) bool {
	if a.exact[path] {
		return true
	}
	for _, p := range a.prefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}
