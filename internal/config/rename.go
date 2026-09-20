package config

import (
	"os"
	"time"
)

// configurationPublishTimeout bounds how long publishing a configuration waits
// for a rename that another process is transiently refusing.
//
// It matches the lock timeout deliberately: both describe the same thing, how
// long a cooperating caller is willing to wait for a file somebody else is
// touching. A var rather than a const so tests can shorten it.
var configurationPublishTimeout = 2 * time.Second

// renameBackoffStart and renameBackoffMax shape the wait between attempts. The
// first retry is quick because most interference clears in a few milliseconds;
// the cap keeps a long wait from turning into a busy loop.
const (
	renameBackoffStart = 5 * time.Millisecond
	renameBackoffMax   = 100 * time.Millisecond
)

// publishByRename moves a fully written temporary file over the configuration
// it replaces, retrying while the platform reports interference it expects to
// pass.
//
// On Windows a rename fails outright when any other handle is open on either
// file. That is not only antivirus: Go does not pass FILE_SHARE_DELETE when it
// opens a file, so an ordinary concurrent reader of the configuration is enough
// to refuse the publish. The previous budget here was five attempts ten
// milliseconds apart, which is shorter than a scanner takes to let go, and the
// caller saw "could not atomically publish configuration" for something that
// would have succeeded a moment later.
//
// Retrying is confined to those transient refusals. A rename that fails because
// the source vanished or the target directory is gone is returned immediately:
// waiting on it only delays a report the caller needs now.
func publishByRename(source, target string) error {
	return retryRename(
		func() error { return os.Rename(source, target) },
		retryableRenameError,
		configurationPublishTimeout,
	)
}

// retryRename is publishByRename's loop, separated from the platform so the
// timing behaviour can be tested on every platform rather than only the one
// whose filesystem misbehaves.
func retryRename(rename func() error, retryable func(error) bool, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	delay := renameBackoffStart

	for {
		err := rename()
		if err == nil {
			return nil
		}
		if !retryable(err) || !time.Now().Before(deadline) {
			return err
		}

		time.Sleep(delay)
		if delay < renameBackoffMax {
			delay *= 2
			if delay > renameBackoffMax {
				delay = renameBackoffMax
			}
		}
	}
}
