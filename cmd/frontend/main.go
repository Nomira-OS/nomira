// nomira-frontend is the BuildKit gateway frontend for nomira recipes.
//
// Modes:
//
//	(no args)  gateway frontend; run by BuildKit via `# syntax=` or
//	           `--frontend gateway.v0 --opt source=<image>`
//	version    print the version of the frontend
//
// Linting and resolving a recipe go through BuildKit as well, with the
// build option mode (see package frontend): the frontend reads the files
// and imports, local and by URL, the same way as for a build.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/moby/buildkit/frontend/gateway/grpcclient"
	"github.com/moby/buildkit/util/appcontext"

	"github.com/Nomira-OS/nomira/internal/frontend"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version":
			fmt.Println(frontend.Version)
			return
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
  nomira-frontend          run as gateway frontend (BuildKit invokes this)
  nomira-frontend version  print the version

Build options (buildctl --opt):
  filename=FILE            the recipe in the "dockerfile" local (recipe.yml)
  mode=build|lint|resolve  build the image (default), only parse the
                           recipe, or output it with all imports merged
  image-resolve-mode=default|pull|local
  toolbox=IMAGE            the image the checks run in
`)
}
