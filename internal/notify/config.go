package notify

// Config controls operator alert and notification behavior.
type Config struct {
	// Enabled is the master switch for notifications and alerts.
	Enabled bool `yaml:"enabled"`
	// Bell enables emitting the ASCII BEL character (\a) to the terminal.
	Bell bool `yaml:"bell"`
	// Desktop enables native operating-system desktop notifications.
	Desktop bool `yaml:"desktop"`
}

// DefaultConfig returns the default notification settings (all enabled).
func DefaultConfig() Config {
	return Config{
		Enabled: true,
		Bell:    true,
		Desktop: true,
	}
}

// DisabledConfig returns settings with all notifications disabled.
func DisabledConfig() Config {
	return Config{
		Enabled: false,
		Bell:    false,
		Desktop: false,
	}
}
