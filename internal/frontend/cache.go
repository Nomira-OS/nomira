package frontend

import (
	"context"
	"encoding/json"
	"fmt"

	controlapi "github.com/moby/buildkit/api/services/control"
	"github.com/moby/buildkit/frontend/gateway/client"
)

// keyCacheImports is the build option in which BuildKit hands a frontend
// the caches the client imports (buildctl --import-cache), as JSON.
const keyCacheImports = "cache-imports"

// cacheClient passes the imported caches to every solve, as BuildKit's own
// frontends do: BuildKit consults an imported cache only for the solves
// that name it, so without this a build would never take a step from a
// cache in a registry or a directory.
type cacheClient struct {
	client.Client
	imports []client.CacheOptionsEntry
}

func (c cacheClient) Solve(ctx context.Context, req client.SolveRequest) (*client.Result, error) {
	if len(req.CacheImports) == 0 {
		req.CacheImports = c.imports
	}
	return c.Client.Solve(ctx, req)
}

// withCacheImports wraps c when the client imports caches.
func withCacheImports(c client.Client) (client.Client, error) {
	s := c.BuildOpts().Opts[keyCacheImports]
	if s == "" {
		return c, nil
	}
	var entries []*controlapi.CacheOptionsEntry
	if err := json.Unmarshal([]byte(s), &entries); err != nil {
		return nil, fmt.Errorf("build option %s: %w", keyCacheImports, err)
	}
	cc := cacheClient{Client: c}
	for _, e := range entries {
		cc.imports = append(cc.imports, client.CacheOptionsEntry{Type: e.Type, Attrs: e.Attrs})
	}
	return cc, nil
}
