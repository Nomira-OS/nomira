// nomira-frontend is the BuildKit gateway frontend for nomira recipes.
//
// Modes:
//
//	(no args)        gateway frontend; run by BuildKit via `# syntax=` or
//	                 `--frontend gateway.v0 --opt source=<image>`
//	lint  [file|-]   parse + schema-validate a recipe; exit 1 with errors
//	graph [file|-]   print the pipeline as a Mermaid flowchart
//	inspect [file|-] print name, base and phase/group names as JSON
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

Recipes that use "include" must be given as a file path.
`)
}

// load parses the recipe named on the command line, or stdin for "-" or no
// argument. Includes need a file path: they resolve relative to it.
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
	type phase struct {
		Name   string   `json:"name"`
		Groups []string `json:"groups"`
	}
	out := struct {
		Name        string   `json:"name"`
		Description string   `json:"description,omitempty"`
		Base        string   `json:"base"`
		Platforms   []string `json:"platforms,omitempty"`
		Tags        []string `json:"tags,omitempty"`
		Phases      []phase  `json:"phases"`
	}{
		Name: r.Settings.Name, Description: r.Settings.Description, Base: r.Settings.Base,
		Platforms: r.Settings.Platforms, Tags: r.Settings.Tags,
	}
	for _, p := range r.Phases {
		out.Phases = append(out.Phases, phase{Name: p.Name, Groups: p.GroupNames()})
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
