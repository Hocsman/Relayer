//go:build windows

package platform

import (
	"errors"

	"golang.org/x/sys/windows"
)

// TransientFileError reports a rename or remove Windows refused because
// something else currently holds one of the paths, which is a condition that
// passes on its own.
//
// The codes were confirmed on Windows rather than assumed:
//
//	an exclusive handle on the destination -> ERROR_ACCESS_DENIED (5)
//	an exclusive handle on the source      -> ERROR_SHARING_VIOLATION (32)
//
// ERROR_ACCESS_DENIED also covers an ordinary reader, because Go opens a file
// without FILE_SHARE_DELETE: any concurrent read of the target is enough for
// the operation to be refused. ERROR_LOCK_VIOLATION covers the byte-range case.
//
// Everything else is permanent and returned to the caller unchanged. A missing
// file or directory does not appear by waiting. A genuine permission problem
// reports ERROR_ACCESS_DENIED too, so it costs the budget before being
// reported -- the price of not failing a save that would have worked.
func TransientFileError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}
