// nomira-frontend is the BuildKit gateway frontend for nomira recipes.
//
// Modes:
//
//	(no args)        gateway frontend; run by BuildKit via `# syntax=` or
//	                 `--frontend gateway.v0 --opt source=<image>`
//	lint  [file|-]   parse + schema-validate a recipe; exit 1 with errors
//	graph [file|-]   print the pipeline as a Mermaid flowchart
//
// lint/graph exist so a thin CLI (in any language) can reuse the Go parser
// by running this image via podman instead of reimplementing the schema.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/moby/buildkit/frontend/gateway/grpcclient"
	"github.com/moby/buildkit/util/appcontext"

	"github.com/Leaced/nomira/internal/frontend"
	"github.com/Leaced/nomira/internal/recipe"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "lint":
			os.Exit(lint(fileArg()))
		case "graph":
			os.Exit(graph(fileArg()))
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
`)
}

func fileArg() io.Reader {
	if len(os.Args) > 2 && os.Args[2] != "-" {
		f, err := os.Open(os.Args[2])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		return f
	}
	return os.Stdin
}

func parseFrom(in io.Reader) (*recipe.Recipe, error) {
	data, err := io.ReadAll(in)
	if err != nil {
		return nil, err
	}
	return recipe.Parse(data, nil)
}

func lint(in io.Reader) int {
	if _, err := parseFrom(in); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println("ok")
	return 0
}

func graph(in io.Reader) int {
	r, err := parseFrom(in)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Print(r.Graph())
	return 0
}
