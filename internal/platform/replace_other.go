//go:build !windows

package platform

// TransientFileError reports nothing as transient away from Windows.
//
// rename(2) and unlink(2) do not fail because another process holds the file
// open, so every error they return is a real one: a missing path, a
// cross-device move, a permission problem. Retrying would delay the report
// without changing it.
func TransientFileError(error) bool { return false }
