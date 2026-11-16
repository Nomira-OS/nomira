package frontend

import (
	"strings"
	"testing"
)

func TestGuardFindings(t *testing.T) {
	got := guardFindings("T usr/local\nM 750.0.0:755.0.0 etc/sudoers.d\nT etc/dir with space\n")
	want := []string{
		"/usr/local is a symbolic link or of another type there",
		"/etc/sudoers.d has mode.uid.gid 750.0.0 there, the group would create it as 755.0.0",
		"/etc/dir with space is a symbolic link or of another type there",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %q", got)
	}
	if len(guardFindings("")) != 0 || len(guardFindings("\n")) != 0 {
		t.Error("empty output produced findings")
	}
}
