package compile

import (
	"fmt"
	"strings"
	"testing"

	"github.com/moby/buildkit/client/llb"

	"github.com/Nomira-OS/nomira/internal/recipe"
)

func TestBucketStable(t *testing.T) {
	keys := make([]string, 600)
	for i := range keys {
		keys[i] = fmt.Sprintf("pkg-%d", i)
	}
	used := map[int]bool{}
	moved := 0
	for _, k := range keys {
		b := Bucket(k, 64)
		if b < 0 || b >= 64 || Bucket(k, 64) != b {
			t.Fatalf("%s: bucket %d", k, b)
		}
		used[b] = true
		// One bucket more moves a key only into the new bucket.
		if b65 := Bucket(k, 65); b65 != b {
			if b65 != 64 {
				t.Errorf("%s moved from %d to %d", k, b, b65)
			}
			moved++
		}
	}
	if len(used) < 60 {
		t.Errorf("only %d buckets used", len(used))
	}
	if moved == 0 || moved > 30 {
		t.Errorf("%d of 600 keys moved for one bucket more", moved)
	}
}

func TestParseFanList(t *testing.T) {
	sum := strings.Repeat("ab", 32)
	items, err := ParseFanList([]byte("vim https://m.example/core/os/x86_64/vim-9.1-1-x86_64.pkg.tar.zst sha256:" + sum + "\n\nsystemd-libs http://m/systemd-libs-258-1.pkg.tar.zst sha256:" + sum + " systemd\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Key != "vim" || items[0].File != "vim-9.1-1-x86_64.pkg.tar.zst" || items[1].Key != "systemd" || items[1].Digest.Encoded() != sum {
		t.Errorf("%+v", items)
	}
	for _, bad := range []string{
		"vim https://m/vim.pkg",
		"vim ftp://m/vim.pkg sha256:" + sum,
		"vim https://m/vim.pkg md5:" + sum,
		"vim https://m/vim.pkg sha256:abc",
		"vim https://m/ sha256:" + sum,
		"a https://m/x sha256:" + sum + "\nb https://n/x sha256:" + sum,
		"a https://m/x sha256:" + sum + " key extra",
	} {
		if _, err := ParseFanList([]byte(bad)); err == nil {
			t.Errorf("%q: accepted", bad)
		}
	}
}

func TestCompileFanout(t *testing.T) {
	r, err := recipe.Parse([]byte(`
settings: {base: docker.io/library/alpine:3.22}
build:
  packages:
    collect:
      resolve: run resolve ${items}
      each: run each
      then: run then
  features:
    a: {packages: [one, two, three]}
`))
	if err != nil {
		t.Fatal(err)
	}
	sum := strings.Repeat("0", 64)
	var list strings.Builder
	for _, n := range []string{"one", "two", "three"} {
		fmt.Fprintf(&list, "%s https://m/%s.pkg sha256:%s\n", n, n, sum)
	}
	var checked map[string]llb.State
	b, err := Compile(r, Hooks{
		Resolve: func(llb.State) ([]byte, error) { return []byte(list.String()), nil },
		CheckBuckets: func(scope string, buckets map[string]llb.State) error {
			checked = buckets
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	buckets := map[int]bool{}
	for _, n := range []string{"one", "two", "three"} {
		buckets[Bucket(n, Buckets)] = true
	}
	k, _ := ops(t, b.Final, "")
	// One each per bucket, then; resolve is solved apart.
	if k["exec"] != 1+len(buckets) || k["source"] < 4 {
		t.Errorf("ops: %v (buckets %d)", k, len(buckets))
	}
	if len(buckets) > 1 && len(checked) != len(buckets) {
		t.Errorf("checked %d buckets, want %d", len(checked), len(buckets))
	}
	if _, err := Compile(r, Hooks{}); err == nil {
		t.Error("no resolver: accepted")
	}
}
