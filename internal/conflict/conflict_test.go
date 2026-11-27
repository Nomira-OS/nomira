package conflict

import (
	"strings"
	"testing"
)

func ws(t *testing.T, s string) []Entry {
	t.Helper()
	e, err := ParseWriteSet(s)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// hashed runs pass 2 on sets with the given tokens per group.
func hashed(sets map[string][]Entry, tokens map[string]map[string]string) map[string][]Entry {
	for g := range Overlaps(sets) {
		SetTokens(sets[g], tokens[g])
	}
	return sets
}

func TestDisjointOK(t *testing.T) {
	sets := map[string][]Entry{
		"a": ws(t, "W etc/kmscon/kmscon.conf\n"),
		"b": ws(t, "W etc/NetworkManager/conf.d/dns.conf\nW etc/systemd/system/b.service\n"),
	}
	if o := Overlaps(sets); len(o) != 0 {
		t.Errorf("overlaps: %v", o)
	}
	if err := Check("configs", sets); err != nil {
		t.Fatal(err)
	}
}

func TestOverlapsAreHashedAndIdenticalPasses(t *testing.T) {
	sets := map[string][]Entry{
		"a": ws(t, "W etc/foo\nW etc/a\n"),
		"b": ws(t, "W etc/foo\nW etc/b\n"),
		"c": ws(t, "W etc/c\n"),
	}
	o := Overlaps(sets)
	if len(o) != 2 || strings.Join(o["a"], ",") != "etc/foo" || strings.Join(o["b"], ",") != "etc/foo" {
		t.Fatalf("overlaps: %v", o)
	}
	same := hashed(sets, map[string]map[string]string{
		"a": {"etc/foo": "h1:644:0:0"}, "b": {"etc/foo": "h1:644:0:0"},
	})
	if err := Check("p", same); err != nil {
		t.Fatalf("identical writes must pass: %v", err)
	}
	for name, tok := range map[string]string{"content": "h2:644:0:0", "mode": "h1:600:0:0", "owner": "h1:644:1:0"} {
		sets := map[string][]Entry{"a": ws(t, "W etc/foo\n"), "b": ws(t, "W etc/foo\n")}
		hashed(sets, map[string]map[string]string{"a": {"etc/foo": "h1:644:0:0"}, "b": {"etc/foo": tok}})
		err := Check("p", sets)
		if err == nil || !strings.Contains(err.Error(), `"a" and "b": /etc/foo`) {
			t.Errorf("%s differs: %v", name, err)
		}
	}
	// Without pass 2 a shared write is never taken as identical.
	unhashed := map[string][]Entry{"a": ws(t, "W etc/foo\n"), "b": ws(t, "W etc/foo\n")}
	if err := Check("p", unhashed); err == nil {
		t.Error("unhashed overlap passed")
	}
}

func TestDeletes(t *testing.T) {
	ok := map[string]map[string][]Entry{
		"same file":        {"a": ws(t, "D etc/foo\n"), "b": ws(t, "D etc/foo\n")},
		"same dir":         {"a": ws(t, "D etc/foo/\n"), "b": ws(t, "D etc/foo/\n")},
		"delete below dir": {"a": ws(t, "D etc/foo/\nD etc/foo/x\n"), "b": ws(t, "D etc/foo/x\n")},
	}
	for name, sets := range ok {
		if err := Check("p", sets); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	bad := map[string]map[string][]Entry{
		"write vs delete":     {"a": ws(t, "D etc/foo\n"), "b": ws(t, "W etc/foo\n")},
		"write below deleted": {"a": ws(t, "D etc/foo/\n"), "b": ws(t, "W etc/foo/bar.conf\n")},
		"reverse direction":   {"a": ws(t, "W etc/foo/bar.conf\n"), "b": ws(t, "D etc/foo/\n")},
		"file vs dir":         {"a": ws(t, "D etc/foo/\n"), "b": ws(t, "D etc/foo\n")},
	}
	for name, sets := range bad {
		err := Check("p", sets)
		if err == nil {
			t.Errorf("%s: passed", name)
		} else if strings.HasPrefix(name, "write below") && !strings.Contains(err.Error(), "deletes directory /etc/foo") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestParse(t *testing.T) {
	e := ws(t, "W etc/my config/file.conf\nD opt/old dir/\nD etc/x\n")
	if e[0] != (Entry{Kind: Write, Path: "etc/my config/file.conf"}) || e[1] != (Entry{Kind: DeleteDir, Path: "opt/old dir"}) || e[2].Kind != Delete {
		t.Errorf("parsed: %+v", e)
	}
	if _, err := ParseWriteSet("X nope\n"); err == nil {
		t.Error("malformed line should fail")
	}
	h, err := ParseHashes("H abc:644:0:0 etc/my file\n")
	if err != nil || h["etc/my file"] != "abc:644:0:0" {
		t.Errorf("hashes: %v %v", h, err)
	}
	if _, err := ParseHashes("W x y\n"); err == nil {
		t.Error("malformed hash line should fail")
	}
}

func TestCheckStrict(t *testing.T) {
	if err := CheckStrict("p", map[string][]Entry{"b1": ws(t, "W usr/a\n"), "b2": ws(t, "W usr/b\n")}); err != nil {
		t.Fatal(err)
	}
	err := CheckStrict("p", map[string][]Entry{"b1": ws(t, "W usr/a\n"), "b2": ws(t, "W usr/a\n")})
	if err == nil || !strings.Contains(err.Error(), "/usr/a (b1, b2)") {
		t.Errorf("same file in two buckets: %v", err)
	}
}
