//go:build !windows

package config

// retryableRenameError reports nothing as retryable away from Windows.
//
// rename(2) is atomic and does not fail because another process happens to hold
// the file open, so every error it returns is a real one: a missing source, a
// cross-device move, a permission problem. Retrying would delay the report
// without changing it.
func retryableRenameError(error) bool { return false }
