//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package tmuxbackend

import "os"

func ensurePlatformSupport() error { return ErrUnsupported }

func makeFIFO(string, uint32) error { return ErrUnsupported }

func openFIFO(string) (*os.File, error) { return nil, ErrUnsupported }
