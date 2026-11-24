package compile

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/solver/pb"

	"github.com/Nomira-OS/nomira/internal/recipe"
)

const src = `
settings: {base: docker.io/library/alpine:3.22}
phases:
  - name: packages
    collect: run apk add ${items}
  - name: features
    groups:
      copies:
        - copy files/a /etc/a
      runs:
        tools: [curl]
        commands:
          - run echo b > /etc/b
      only-contributes:
        tools: [jq]
  - name: tools
    collect: run apk add ${items}
  - name: single
    groups:
      alone:
        - copy files/c /etc/c
`

// ops returns the kinds of all ops a state depends on, and whether the
// given image is among its sources.
func ops(t *testing.T, st llb.State, image string) (kinds map[string]int, usesImage bool) {
	t.Helper()
	def, err := st.Marshal(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	kinds = map[string]int{}
	for _, dt := range def.Def {
		var op pb.Op
		if err := op.UnmarshalVT(dt); err != nil {
			t.Fatal(err)
		}
		switch o := op.Op.(type) {
		case *pb.Op_Exec:
			kinds["exec"]++
		case *pb.Op_File:
			kinds["file"]++
		case *pb.Op_Merge:
			kinds["merge"]++
		case *pb.Op_Diff:
			kinds["diff"]++
		case *pb.Op_Source:
			kinds["source"]++
			if strings.Contains(o.Source.Identifier, image) {
				usesImage = true
			}
		}
	}
	return kinds, usesImage
}

func TestCompileShapes(t *testing.T) {
	r, err := recipe.Parse([]byte(src), nil)
	if err != nil {
		t.Fatal(err)
	}
	asked := map[string]bool{}
	done := 0
	b, err := Compile(r, Hooks{
		Decide: func(ph recipe.Phase, lower llb.State, alone map[string]llb.State) (map[string]string, error) {
			for g := range alone {
				asked[ph.Name+"/"+g] = true
			}
			return nil, nil
		},
		PhaseDone: func(PhasePlan) error { done++; return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if done != 4 {
		t.Errorf("PhaseDone calls: %d", done)
	}
	// Only groups without run are candidates.
	if !asked["features/copies"] || !asked["single/alone"] || asked["features/runs"] || len(asked) != 2 {
		t.Errorf("Decide asked for: %v", asked)
	}

	// The first collector has no items and adds nothing.
	if k, _ := ops(t, b.Phases[0].Output, ""); k["exec"] != 0 {
		t.Errorf("empty collector ran something: %v", k)
	}

	feat := b.Phases[1]
	if !feat.Validate || !feat.Isolated["copies"] || feat.Isolated["runs"] {
		t.Errorf("features plan: validate=%v isolated=%v", feat.Validate, feat.Isolated)
	}
	if _, ok := feat.Uppers["only-contributes"]; ok {
		t.Error("a group without commands was built")
	}
	// The isolated group does not depend on the base image at all.
	if k, base := ops(t, feat.Uppers["copies"], "alpine"); base || k["exec"] != 0 || k["file"] != 1 {
		t.Errorf("isolated group: ops=%v usesBase=%v", k, base)
	}
	if _, base := ops(t, feat.Uppers["runs"], "alpine"); !base {
		t.Error("group with run does not build on the base")
	}
	// Both groups have a layer; the isolated one is its own layer.
	if len(feat.Layers) != 2 {
		t.Errorf("layers: %d", len(feat.Layers))
	}
	if k, base := ops(t, feat.Layers["copies"], "alpine"); base || k["diff"] != 0 {
		t.Errorf("layer of the isolated group: ops=%v usesBase=%v", k, base)
	}
	if k, _ := ops(t, feat.Layers["runs"], ""); k["diff"] != 1 {
		t.Errorf("layer of the group with run: %v", k)
	}
	// The lister of the isolated group sees nothing of the base either.
	if _, base := ops(t, WriteSetState("docker.io/library/busybox:1", nil, nil, feat.Layers["copies"], "features", "copies"), "alpine"); base {
		t.Error("write set of an isolated group depends on the base")
	}
	// One diff (for the group with run), one merge.
	if k, _ := ops(t, feat.Output, ""); k["merge"] != 1 || k["diff"] != 1 {
		t.Errorf("features output: %v", k)
	}

	// The second collector runs once with both items.
	tools := b.Phases[2]
	if strings.Join(tools.Phase.Items, " ") != "curl jq" {
		t.Errorf("items: %v", tools.Phase.Items)
	}

	// A single isolated group is merged rather than run on the input.
	single := b.Phases[3]
	if single.Validate || !single.Isolated["alone"] {
		t.Errorf("single plan: %+v", single.Isolated)
	}
	if k, _ := ops(t, single.Output, ""); k["merge"] != 2 {
		t.Errorf("single output: %v", k)
	}
}

func TestDecideFallsBack(t *testing.T) {
	r, err := recipe.Parse([]byte(src), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Compile(r, Hooks{
		Decide: func(_ recipe.Phase, _ llb.State, alone map[string]llb.State) (map[string]string, error) {
			rejected := map[string]string{}
			for g := range alone {
				rejected[g] = "/etc/x is a symbolic link"
			}
			return rejected, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	feat := b.Phases[1]
	if feat.Isolated["copies"] {
		t.Error("group stayed isolated although Decide said no")
	}
	if _, base := ops(t, feat.Uppers["copies"], "alpine"); !base {
		t.Error("fallback does not build on the base")
	}
	// Single group on the input: no merge of its own.
	if k, _ := ops(t, b.Phases[3].Output, ""); k["merge"] != 1 {
		t.Errorf("single fallback output: %v", k)
	}
}

func TestWithUKI(t *testing.T) {
	def, err := WithUKI(llb.Image("localhost/sys:1")).Marshal(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, m := range def.Metadata {
		if n := m.Description["llb.customname"]; strings.HasPrefix(n, "[uki]") {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	want := "[uki] add the unified kernel image to /boot,[uki] build the unified kernel image,[uki] empty ukify.conf,[uki] move kernel and initramfs out of the image"
	if got := strings.Join(names, ","); got != want {
		t.Errorf("steps: %s", got)
	}
}
