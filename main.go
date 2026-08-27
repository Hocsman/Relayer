// Command relayer is kept at the repository root for compatibility with the
// original `go build -o relayer main.go` installation command. New builds
// should use the canonical ./cmd/relayer entrypoint.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/Hocsman/Relayer/internal/app"
	"github.com/Hocsman/Relayer/internal/tmuxbackend"
)

func main() {
	if handled, exitCode := tmuxbackend.HelperMain(os.Args[1:], os.Stderr); handled {
		os.Exit(exitCode)
	}
	if err := app.RunWithOutput(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintf(os.Stderr, "relayer: %v\n", err)
		os.Exit(1)
	}
}
