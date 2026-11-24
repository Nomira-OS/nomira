package frontend

import "testing"

func TestWantUKI(t *testing.T) {
	bootc := []byte(`{"config":{"Labels":{"containers.bootc":"1"}}}`)
	plain := []byte(`{"config":{}}`)
	yes, no := true, false
	for _, c := range []struct {
		opt     string
		setting *bool
		base    []byte
		want    bool
	}{
		{"", nil, bootc, true},
		{"", nil, plain, false},
		{"", &no, bootc, false},
		{"", &yes, plain, true},
		{"false", &yes, bootc, false},
		{"true", &no, plain, true},
	} {
		got, err := wantUKI(c.opt, c.setting, c.base)
		if err != nil || got != c.want {
			t.Errorf("wantUKI(%q, %v, %s) = %v, %v; want %v", c.opt, c.setting, c.base, got, err, c.want)
		}
	}
	if _, err := wantUKI("maybe", nil, bootc); err == nil {
		t.Error("uki=maybe accepted")
	}
}
