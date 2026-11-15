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

func TestDisjointOK(t *testing.T) {
	sets := map[string][]Entry{
		"a": ws(t, "W h1:644:0:0 etc/kmscon/kmscon.conf\n"),
		"b": ws(t, "W h2:644:0:0 etc/NetworkManager/conf.d/dns.conf\n"),
	}
	if err := Check("configs", Strict, nil, sets); err != nil {
		t.Fatal(err)
	}
}

func TestStrictFileConflict(t *testing.T) {
	sets := map[string][]Entry{
		"a": ws(t, "W h1:644:0:0 etc/foo\n"),
		"b": ws(t, "W h1:644:0:0 etc/foo\n"),
	}
	err := Check("p", Strict, nil, sets)
	if err == nil || !strings.Contains(err.Error(), `"a" and "b"`) {
		t.Fatalf("expected conflict, got %v", err)
	}
}

func TestAllowIdentical(t *testing.T) {
	same := map[string][]Entry{
		"a": ws(t, "W h1:644:0:0 etc/foo\n"),
		"b": ws(t, "W h1:644:0:0 etc/foo\n"),
	}
	if err := Check("p", AllowIdentical, nil, same); err != nil {
		t.Fatalf("identical content should pass: %v", err)
	}
	diffMeta := map[string][]Entry{
		"a": ws(t, "W h1:644:0:0 etc/foo\n"),
		"b": ws(t, "W h1:600:0:0 etc/foo\n"),
	}
	if err := Check("p", AllowIdentical, nil, diffMeta); err == nil {
		t.Fatal("differing metadata should fail even under allow-identical")
	}
}

func TestSharedAllowlist(t *testing.T) {
	sets := map[string][]Entry{
		"a": ws(t, "W h1:644:0:0 usr/lib/tmpfiles.d/x.conf\nW h3:644:0:0 etc/exact\n"),
		"b": ws(t, "W h2:644:0:0 usr/lib/tmpfiles.d/x.conf\nW h4:644:0:0 etc/exact\n"),
	}
	// prefix covers the tmpfiles conflict, exact path covers the other
	if err := Check("p", Strict, []string{"/usr/lib/tmpfiles.d/", "/etc/exact"}, sets); err != nil {
		t.Fatal(err)
	}
	// without allowlist both conflict
	if err := Check("p", Strict, nil, sets); err == nil {
		t.Fatal("expected conflicts")
	}
}

func TestDirDeletePrefix(t *testing.T) {
	sets := map[string][]Entry{
		"a": ws(t, "D etc/foo/\n"),
		"b": ws(t, "W h1:644:0:0 etc/foo/bar.conf\n"),
	}
	err := Check("p", Strict, nil, sets)
	if err == nil || !strings.Contains(err.Error(), "deletes directory /etc/foo") {
		t.Fatalf("expected prefix conflict, got %v", err)
	}
	// and in the other direction
	sets2 := map[string][]Entry{
		"a": ws(t, "W h1:644:0:0 etc/foo/bar.conf\n"),
		"b": ws(t, "D etc/foo/\n"),
	}
	if err := Check("p", Strict, nil, sets2); err == nil {
		t.Fatal("expected prefix conflict (reverse direction)")
	}
}

func TestFileDeleteVsWrite(t *testing.T) {
	sets := map[string][]Entry{
		"a": ws(t, "D etc/foo\n"),
		"b": ws(t, "W h1:644:0:0 etc/foo\n"),
	}
	if err := Check("p", AllowIdentical, nil, sets); err == nil {
		t.Fatal("delete vs write must conflict under any policy")
	}
}

func TestSharedDirsNoConflict(t *testing.T) {
	// Both groups create files under the same directory — never a conflict.
	sets := map[string][]Entry{
		"a": ws(t, "W h1:644:0:0 etc/systemd/system/a.service\n"),
		"b": ws(t, "W h2:644:0:0 etc/systemd/system/b.service\n"),
	}
	if err := Check("p", Strict, nil, sets); err != nil {
		t.Fatal(err)
	}
}

func TestParseWriteSetSpacesAndErrors(t *testing.T) {
	e := ws(t, "W h:644:0:0 etc/my config/file.conf\nD opt/old dir/\n")
	if e[0].Path != "etc/my config/file.conf" {
		t.Errorf("spaces in path lost: %q", e[0].Path)
	}
	if e[1].Kind != DeleteDir || e[1].Path != "opt/old dir" {
		t.Errorf("dir delete mis-parsed: %+v", e[1])
	}
	if _, err := ParseWriteSet("X nope\n"); err == nil {
		t.Error("malformed line should fail")
	}
}
