//go:build windows

package config

import (
	"errors"

	"golang.org/x/sys/windows"
)

// retryableRenameError reports a rename Windows refused because something else
// currently holds one of the two files, which is a condition that passes.
//
// The three codes were confirmed on this platform rather than assumed:
//
//	an exclusive handle on the destination  -> ERROR_ACCESS_DENIED (5)
//	an exclusive handle on the source       -> ERROR_SHARING_VIOLATION (32)
//
// ERROR_ACCESS_DENIED also covers an ordinary reader: Go opens a file without
// FILE_SHARE_DELETE, so any concurrent read of the configuration refuses the
// publish. ERROR_LOCK_VIOLATION is included for the byte-range case, which the
// configuration lock uses on its own file.
//
// Everything else is permanent and returned to the caller unchanged. A missing
// source or directory does not become available by waiting.
func retryableRenameError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}
