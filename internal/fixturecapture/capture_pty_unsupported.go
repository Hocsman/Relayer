//go:build !aix && !darwin && !dragonfly && !freebsd && !illumos && !linux && !netbsd && !openbsd && !solaris

package fixturecapture

import (
	"context"
	"fmt"
	"io"
	"runtime"
)

func capturePTY(context.Context, Options) (captureResult, error) {
	return captureResult{}, fmt.Errorf("%w: %s", ErrUnsupported, runtime.GOOS)
}

func captureTmux(context.Context, Options) (captureResult, error) {
	return captureResult{}, fmt.Errorf("%w: %s", ErrUnsupported, runtime.GOOS)
}

func isTerminalCloseError(error) bool { return false }

func HelperMain(arguments []string, _ io.Writer) (bool, int) {
	if len(arguments) > 0 && arguments[0] == helperSubcommand {
		return true, 125
	}
	return false, 0
}

const helperSubcommand = "__relayer_fixture_capture_exec"
