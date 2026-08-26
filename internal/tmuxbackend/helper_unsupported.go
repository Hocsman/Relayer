//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package tmuxbackend

func executeLaunchSpec(string, string) error { return ErrUnsupported }
