package frontend

import (
	"context"
	"testing"

	"github.com/moby/buildkit/frontend/gateway/client"
)

type optsClient struct {
	client.Client
	opts map[string]string
	got  []client.CacheOptionsEntry
}

func (c *optsClient) BuildOpts() client.BuildOpts { return client.BuildOpts{Opts: c.opts} }

func (c *optsClient) Solve(_ context.Context, req client.SolveRequest) (*client.Result, error) {
	c.got = req.CacheImports
	return nil, nil
}

func TestCacheImports(t *testing.T) {
	raw := &optsClient{opts: map[string]string{keyCacheImports: `[{"Type":"local","Attrs":{"src":"/x","digest":"sha256:a"}},{"Type":"registry","Attrs":{"ref":"r/i:t"}}]`}}
	c, err := withCacheImports(raw)
	if err != nil {
		t.Fatal(err)
	}
	c.Solve(context.Background(), client.SolveRequest{})
	if len(raw.got) != 2 || raw.got[0].Type != "local" || raw.got[0].Attrs["digest"] != "sha256:a" || raw.got[1].Attrs["ref"] != "r/i:t" {
		t.Errorf("imports passed on: %+v", raw.got)
	}
	none := &optsClient{opts: map[string]string{}}
	if c, _ := withCacheImports(none); c != client.Client(none) {
		t.Error("wrapped without imports")
	}
	if _, err := withCacheImports(&optsClient{opts: map[string]string{keyCacheImports: "{"}}); err == nil {
		t.Error("bad JSON accepted")
	}
}
