package frontend

import (
	"encoding/json"
	"testing"
)

func TestImageConfig(t *testing.T) {
	base := []byte(`{"architecture":"amd64","os":"linux",
		"config":{"Cmd":["/sbin/init"],"Labels":{"containers.bootc":"1","a":"old"}},
		"rootfs":{"type":"layers","diff_ids":["sha256:x"]},"history":[{"created_by":"x"}]}`)
	out, err := imageConfig(base, map[string]string{"a": "new", "b": "2"}, ptr(86400))
	if err != nil {
		t.Fatal(err)
	}
	var img map[string]any
	if err := json.Unmarshal(out, &img); err != nil {
		t.Fatal(err)
	}
	if _, ok := img["rootfs"]; ok {
		t.Error("rootfs must be dropped")
	}
	if _, ok := img["history"]; ok {
		t.Error("history must be dropped")
	}
	if img["architecture"] != "amd64" {
		t.Error("architecture lost")
	}
	labels := img["config"].(map[string]any)["Labels"].(map[string]any)
	want := map[string]string{"containers.bootc": "1", "a": "new", "b": "2"}
	for k, v := range want {
		if labels[k] != v {
			t.Errorf("label %s = %v, want %s", k, labels[k], v)
		}
	}
	if img["created"] != "1970-01-02T00:00:00Z" {
		t.Errorf("created = %v, want the epoch", img["created"])
	}
	// No labels requested and none in the base: only the time is set.
	out, err = imageConfig([]byte(`{"os":"linux"}`), nil, ptr(0))
	if err != nil || string(out) != `{"created":"1970-01-01T00:00:00Z","os":"linux"}` {
		t.Errorf("got %s, %v", out, err)
	}
}

func ptr(v int64) *int64 { return &v }

func TestImageConfigWithoutEpoch(t *testing.T) {
	out, err := imageConfig([]byte(`{"created":"2026-01-01T00:00:00Z","os":"linux"}`), nil, nil)
	if err != nil || string(out) != `{"os":"linux"}` {
		t.Errorf("got %s, %v: the exporter should set the time", out, err)
	}
}

func TestSourceDateEpoch(t *testing.T) {
	set := int64(42)
	base := []byte(`{"created":"2026-11-10T00:00:00Z"}`)
	if got := sourceDateEpoch(&set, base); got != 42 {
		t.Errorf("setting: got %d", got)
	}
	if got := sourceDateEpoch(nil, base); got != 1794268800 {
		t.Errorf("base: got %d", got)
	}
	for _, b := range []string{`{}`, `{"created":"1970-01-01T00:00:00Z"}`, `x`} {
		if got := sourceDateEpoch(nil, []byte(b)); got != 0 {
			t.Errorf("%s: got %d, want 0", b, got)
		}
	}
}
