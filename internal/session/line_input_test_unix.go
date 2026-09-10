//go:build !windows

package session

import (
	"os"
	"testing"
)

func checkPipeEmpty(t *testing.T, reader *os.File) {
	t.Helper()
}
