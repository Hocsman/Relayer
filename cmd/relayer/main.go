// Command relayer starts the human-in-the-loop PTY orchestrator.
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
