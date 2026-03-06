package model

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// LoadConfig reads, parses, and validates config.yaml from the given .maestro directory.
func LoadConfig(maestroDir string) (Config, error) {
	data, err := os.ReadFile(filepath.Join(maestroDir, "config.yaml"))
	if err != nil {
		return Config{}, fmt.Errorf("read config.yaml: %w", err)
	}
	cfg := Config{
		Worktree: WorktreeConfig{
			Enabled: true,
		},
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config.yaml: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("validate config: %w", err)
	}
	return cfg, nil
}
