package compile

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/moby/buildkit/client/llb"
	digest "github.com/opencontainers/go-digest"

	"github.com/Nomira-OS/nomira/internal/recipe"
)

// A collector that fans out (recipe.Fanout) runs in four steps, all on
// the state S the phase starts from:
//
//	resolve  prints the files to fetch                 → FanoutList
//	fetch    BuildKit downloads each file, by checksum (parallel)
//	each     one run per bucket, its files in FanoutIn  (parallel)
//	then     on Merge(S, buckets…), all files in FanoutIn
//
// A file's bucket depends on its key alone (Bucket), so a bucket only
// changes when one of its own files does; an unchanged bucket is neither
// unpacked nor exported again.
const (
	FanoutOut  = "/nomira/out"
	FanoutIn   = "/nomira/in"
	FanoutList = "/files"
	// Buckets is how many layers a collector that fans out makes at most.
	Buckets = 64
)

// FanItem is one line that resolve printed.
type FanItem struct {
	Name, URL, File, Key string
	Digest               digest.Digest
}

// ParseFanList parses what resolve printed: per line name, URL,
// sha256:<hex> and optionally a key (default: the name).
func ParseFanList(data []byte) ([]FanItem, error) {
	var items []FanItem
	seen := map[string]bool{}
	for i, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if len(f) != 3 && len(f) != 4 {
			return nil, fmt.Errorf("resolve, line %d: %q: expected name, URL, sha256:<hex> and optionally a key", i+1, line)
		}
		it := FanItem{Name: f[0], URL: f[1], Key: f[0]}
		if len(f) == 4 {
			it.Key = f[3]
		}
		u, err := url.Parse(it.URL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return nil, fmt.Errorf("resolve, line %d: %q is not an http(s) URL", i+1, it.URL)
		}
		it.File = path.Base(u.Path)
		if it.File == "." || it.File == "/" || it.File == "" {
			return nil, fmt.Errorf("resolve, line %d: %q names no file", i+1, it.URL)
		}
		hex, ok := strings.CutPrefix(f[2], "sha256:")
		if !ok || len(hex) != 64 || strings.Trim(hex, "0123456789abcdef") != "" {
			return nil, fmt.Errorf("resolve, line %d: %q is not sha256:<64 lowercase hex digits>", i+1, f[2])
		}
		it.Digest = digest.NewDigestFromEncoded(digest.SHA256, hex)
		if seen[it.File] {
			return nil, fmt.Errorf("resolve, line %d: the file %s comes twice", i+1, it.File)
		}
		seen[it.File] = true
		items = append(items, it)
	}
	return items, nil
}

// Bucket returns the bucket of key among n, by rendezvous hashing: the
// bucket whose hash with the key is highest. It depends on nothing but
// the key and n, and a different n moves only the keys that must move.
func Bucket(key string, n int) int {
	best, bestHash := 0, uint64(0)
	for b := 0; b < n; b++ {
		h := sha256.Sum256([]byte(key + "\x00" + strconv.Itoa(b)))
		if v := binary.BigEndian.Uint64(h[:8]); b == 0 || v > bestHash {
			best, bestHash = b, v
		}
	}
	return best
}

func fanout(cur llb.State, f *recipe.Fanout, env map[string]string, sources map[string]llb.State, scope string, mounts []recipe.Mount, hooks Hooks) (llb.State, error) {
	if hooks.Resolve == nil {
		return cur, fmt.Errorf("%s: a collector that fans out needs a resolver", scope)
	}
	res := f.Resolve
	res.Script = "{\n" + res.Script + "\n} > " + FanoutOut + FanoutList
	opts, err := runOptions(res, mergeEnv(env), sources, scope, mounts)
	if err != nil {
		return cur, err
	}
	opts = append(opts, llb.WithCustomNamef("[%s] resolve %s", scope, firstLine(f.Resolve.Script)))
	list := cur.Run(opts...).AddMount(FanoutOut, llb.Scratch())
	data, err := hooks.Resolve(list)
	if err != nil {
		return cur, fmt.Errorf("%s: resolve: %w", scope, err)
	}
	items, err := ParseFanList(data)
	if err != nil {
		return cur, fmt.Errorf("%s: %w", scope, err)
	}
	if len(items) == 0 {
		return cur, nil
	}

	fetch := func(it FanItem) llb.State {
		return llb.HTTP(it.URL, llb.Checksum(it.Digest), llb.Filename(it.File),
			llb.WithCustomNamef("[%s] fetch %s", scope, it.File))
	}
	files := func(its []FanItem, name string) llb.State {
		if len(its) == 1 {
			return fetch(its[0])
		}
		states := make([]llb.State, len(its))
		for i, it := range its {
			states[i] = fetch(it)
		}
		return llb.Merge(states, llb.WithCustomNamef("[%s] %s", scope, name))
	}

	byBucket := map[int][]FanItem{}
	for _, it := range items {
		b := Bucket(it.Key, Buckets)
		byBucket[b] = append(byBucket[b], it)
	}
	numbers := make([]int, 0, len(byBucket))
	for b := range byBucket {
		numbers = append(numbers, b)
	}
	sort.Ints(numbers)
	layers := map[string]llb.State{}
	merge := []llb.State{cur}
	for _, b := range numbers {
		its := byBucket[b]
		sort.Slice(its, func(i, j int) bool { return its[i].File < its[j].File })
		name := fmt.Sprintf("bucket %02d", b)
		opts, err := runOptions(f.Each, mergeEnv(env), sources, scope, mounts)
		if err != nil {
			return cur, err
		}
		names := make([]string, len(its))
		for i, it := range its {
			names[i] = it.Name
		}
		opts = append(opts,
			llb.AddMount(FanoutIn, files(its, name+" files"), llb.Readonly),
			llb.WithCustomNamef("[%s] each %s: %s", scope, name, firstLine(strings.Join(names, " "))))
		layer := cur.Run(opts...).AddMount(FanoutOut, llb.Scratch())
		layers[name] = layer
		merge = append(merge, layer)
	}
	if hooks.CheckBuckets != nil && len(layers) > 1 {
		if err := hooks.CheckBuckets(scope, layers); err != nil {
			return cur, err
		}
	}
	merged := llb.Merge(merge, llb.WithCustomNamef("[%s] merge %d buckets", scope, len(layers)))
	if f.Then.Script == "" {
		return merged, nil
	}
	opts, err = runOptions(f.Then, mergeEnv(env), sources, scope, mounts)
	if err != nil {
		return cur, err
	}
	opts = append(opts,
		llb.AddMount(FanoutIn, files(items, "all files"), llb.Readonly),
		llb.WithCustomNamef("[%s] then %s", scope, firstLine(f.Then.Script)))
	return merged.Run(opts...).Root(), nil
}
