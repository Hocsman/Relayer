//go:build !darwin && !linux

package config

// The in-process per-path lock in ReplaceAgents remains active on platforms
// without Unix flock. The desktop runtime does not launch agents there yet.
func lockConfigurationFile(string) (func(), error) {
	return func() {}, nil
}
