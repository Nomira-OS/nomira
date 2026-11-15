package frontend

import (
	"encoding/json"
	"testing"
)

func TestImageConfig(t *testing.T) {
	base := []byte(`{"architecture":"amd64","os":"linux",
		"config":{"Cmd":["/sbin/init"],"Labels":{"containers.bootc":"1","a":"old"}},
		"rootfs":{"type":"layers","diff_ids":["sha256:x"]},"history":[{"created_by":"x"}]}`)
	out, err := imageConfig(base, "my system", map[string]string{"a": "new", "b": "2"})
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
	want := map[string]string{"containers.bootc": "1", "a": "new", "b": "2", descriptionLabel: "my system"}
	for k, v := range want {
		if labels[k] != v {
			t.Errorf("label %s = %v, want %s", k, labels[k], v)
		}
	}
	// No labels requested and none in the base: config stays untouched.
	out, err = imageConfig([]byte(`{"os":"linux"}`), "", nil)
	if err != nil || string(out) != `{"os":"linux"}` {
		t.Errorf("got %s, %v", out, err)
	}
}
