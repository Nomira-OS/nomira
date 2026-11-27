// nomira-frontend is the BuildKit gateway frontend for nomira recipes.
//
// Modes:
//
//	(no args)        gateway frontend; run by BuildKit via `# syntax=` or
//	                 `--frontend gateway.v0 --opt source=<image>`
//	lint  [file|-]   parse + schema-validate a recipe; exit 1 with errors
//	graph [file|-]   print the pipeline as a Mermaid flowchart
//	inspect [file|-] print name, base and phase/group names as JSON
//	resolve [file|-] print the recipe with all imports merged, as the
//	                 image records it in its label org.nomira.recipe
//
// lint/graph exist so a thin CLI (in any language) can reuse the Go parser
// by running this image via podman instead of reimplementing the schema.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/moby/buildkit/frontend/gateway/grpcclient"
	"github.com/moby/buildkit/util/appcontext"

	"github.com/Nomira-OS/nomira/internal/frontend"
	"github.com/Nomira-OS/nomira/internal/recipe"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "lint":
			os.Exit(lint())
		case "inspect":
			os.Exit(inspect())
		case "graph":
			os.Exit(graph())
		case "resolve":
			os.Exit(resolve())
		case "--help", "-h", "help":
			usage(os.Stdout)
			return
		default:
			fmt.Fprintf(os.Stderr, "unknown mode %q\n", os.Args[1])
			usage(os.Stderr)
			os.Exit(2)
		}
	}
	if err := grpcclient.RunFromEnvironment(appcontext.Context(), frontend.Build); err != nil {
		fmt.Fprintln(os.Stderr, "nomira-frontend:", err)
		os.Exit(1)
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `nomira-frontend — BuildKit gateway frontend for nomira recipes

usage:
  nomira-frontend              run as gateway frontend (BuildKit invokes this)
  nomira-frontend lint  [f|-]  parse + validate a recipe (default: stdin)
  nomira-frontend graph [f|-]  print pipeline as Mermaid (default: stdin)
  nomira-frontend inspect [f|-] print name, base, phases and groups as JSON
  nomira-frontend resolve [f|-] print the recipe with all imports merged

Recipes that use "imports" must be given as a file path.
`)
}

// load parses the recipe named on the command line, or stdin for "-" or no
// argument. Imports need a file path: they resolve relative to it.
func load() (*recipe.Recipe, error) {
	if len(os.Args) > 2 && os.Args[2] != "-" {
		dir, entry := filepath.Split(os.Args[2])
		if dir == "" {
			dir = "."
		}
		return recipe.ParseFile(entry, func(name string) ([]byte, error) {
			return os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
		}, nil)
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return nil, err
	}
	return recipe.Parse(data, nil)
}

func lint() int {
	if _, err := load(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println("ok")
	return 0
}

func resolve() int {
	r, err := load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	os.Stdout.Write(r.Resolved)
	return 0
}

func graph() int {
	r, err := load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Print(r.Graph())
	return 0
}

func inspect() int {
	r, err := load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	type group struct {
		Name string `json:"name"`
		// Run is false for a group that only copies: it does not depend on
		// the base image or on earlier phases.
		Run        bool                `json:"run"`
		Expects    []string            `json:"expects,omitempty"`
		Contribute map[string][]string `json:"contribute,omitempty"`
	}
	type phase struct {
		Name   string   `json:"name"`
		Groups []string `json:"groups"`
		// Collector phases have items instead of groups.
		Collect string   `json:"collect,omitempty"`
		Items   []string `json:"items,omitempty"`
		Detail  []group  `json:"detail,omitempty"`
	}
	out := struct {
		Name        string            `json:"name"`
		Description string            `json:"description,omitempty"`
		Base        string            `json:"base"`
		Platforms   []string          `json:"platforms,omitempty"`
		Tags        []string          `json:"tags,omitempty"`
		Compression string            `json:"compression"`
		Secrets     map[string]string `json:"secrets,omitempty"`
		// "off", "base" (the base image's creation time) or the seconds.
		SourceDateEpoch string   `json:"source_date_epoch"`
		Record          bool     `json:"record"`
		Volatile        []string `json:"volatile"`
		Phases          []phase  `json:"phases"`
	}{
		Name: r.Settings.Name, Description: r.Settings.Description, Base: r.Settings.Base,
		Platforms: r.Settings.Platforms, Tags: r.Settings.Tags,
		Compression: r.Settings.Compression, Secrets: r.Settings.Secrets,
		SourceDateEpoch: "base", Record: r.Settings.Record, Volatile: r.Settings.Volatile,
	}
	switch {
	case r.Settings.EpochOff:
		out.SourceDateEpoch = "off"
	case r.Settings.SourceDateEpoch != nil:
		out.SourceDateEpoch = strconv.FormatInt(*r.Settings.SourceDateEpoch, 10)
	}
	for _, p := range r.Phases {
		ph := phase{Name: p.Name, Groups: p.GroupNames(), Items: p.Items}
		if p.IsCollector() {
			ph.Collect = p.Collect.Raw()
		}
		for _, g := range ph.Groups {
			grp := p.Groups[g]
			ph.Detail = append(ph.Detail, group{Name: g, Run: grp.HasRun(), Expects: grp.Expects, Contribute: grp.Contribute})
		}
		out.Phases = append(out.Phases, ph)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
