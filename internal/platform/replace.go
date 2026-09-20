package platform

import (
	"os"
	"time"
)

// Replacing a file in place is not uniformly available. On Unix rename(2) is
// atomic and never fails because somebody else holds the file; on Windows both
// rename and remove are refused while any other handle is open on either path,
// and Go does not pass FILE_SHARE_DELETE when it opens a file, so an ordinary
// concurrent reader is enough. That refusal clears on its own, which makes it
// worth waiting out rather than reporting.

// ReplaceTimeout bounds how long a replacement waits for a refusal to clear. It
// is a var so a test can shorten it; nothing else should write it.
var ReplaceTimeout = 2 * time.Second

// replaceBackoffStart and replaceBackoffMax shape the wait between attempts.
// The first retry is quick because most interference clears in a few
// milliseconds; the cap keeps a long wait from becoming a busy loop.
const (
	replaceBackoffStart = 5 * time.Millisecond
	replaceBackoffMax   = 100 * time.Millisecond
)

// PublishByRename moves a fully written temporary file over the file it
// replaces, waiting out a transient platform refusal.
//
// A failure that will not clear -- a missing source, a missing directory -- is
// returned at once: waiting does not create a file, and the caller needs the
// report now.
func PublishByRename(source, target string) error {
	return RetryWhileTransient(func() error { return os.Rename(source, target) })
}

// RemoveFile deletes a file, waiting out a transient platform refusal.
//
// A file that is already absent is not an error: callers use this to clear a
// path before writing it, and a path that is already clear is the state they
// wanted.
func RemoveFile(path string) error {
	err := RetryWhileTransient(func() error { return os.Remove(path) })
	if err != nil && os.IsNotExist(err) {
		return nil
	}
	return err
}

// RetryWhileTransient runs an operation until it succeeds, fails permanently,
// or the budget is spent. The last error is returned unwrapped so a caller can
// still inspect it.
func RetryWhileTransient(operation func() error) error {
	return retryWhileTransient(operation, TransientFileError, ReplaceTimeout)
}

// retryWhileTransient is the loop with its platform dependencies passed in, so
// the timing behaviour can be tested on every platform rather than only the one
// whose filesystem refuses the operation.
func retryWhileTransient(
	operation func() error,
	transient func(error) bool,
	timeout time.Duration,
) error {
	deadline := time.Now().Add(timeout)
	delay := replaceBackoffStart

	for {
		err := operation()
		if err == nil {
			return nil
		}
		if !transient(err) || !time.Now().Before(deadline) {
			return err
		}

		time.Sleep(delay)
		if delay < replaceBackoffMax {
			delay *= 2
			if delay > replaceBackoffMax {
				delay = replaceBackoffMax
			}
		}
	}
}
