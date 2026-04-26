package model

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// configCacheEntry holds a cached Config along with the file mtime used to detect staleness.
type configCacheEntry struct {
	config Config
	mtime  time.Time
	path   string
}

var (
	configCacheMu sync.Mutex
	configCache   *configCacheEntry
)

// ResetConfigCache clears the cached config, forcing the next LoadConfig call to read from disk.
func ResetConfigCache() {
	configCacheMu.Lock()
	configCache = nil
	configCacheMu.Unlock()
}

// LoadConfig reads, parses, and validates config.yaml from the given .maestro directory.
// Results are cached based on the file's mtime; subsequent calls with an unchanged file
// return the cached Config without re-reading or re-parsing.
func LoadConfig(maestroDir string) (Config, error) {
	cfgPath := filepath.Join(maestroDir, "config.yaml")

	info, err := os.Stat(cfgPath)
	if err != nil {
		return Config{}, fmt.Errorf("stat config.yaml: %w", err)
	}
	mtime := info.ModTime()

	configCacheMu.Lock()
	if configCache != nil && configCache.path == cfgPath && configCache.mtime.Equal(mtime) {
		cached := configCache.config
		configCacheMu.Unlock()
		return cached, nil
	}
	configCacheMu.Unlock()

	data, err := os.ReadFile(cfgPath) //nolint:gosec // maestroDir is the application data directory, not user-controlled input
	if err != nil {
		return Config{}, fmt.Errorf("read config.yaml: %w", err)
	}
	cfg := Config{
		Worktree: WorktreeConfig{
			Enabled:          true,
			CleanupOnSuccess: true,
			CleanupOnFailure: false,
		},
	}
	if err := yamlutil.SafeUnmarshalStrict(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config.yaml: %w", err)
	}
	NormalizeRetryConfig(&cfg)
	NormalizeExperimentalConfig(&cfg)
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("validate config: %w", err)
	}

	configCacheMu.Lock()
	configCache = &configCacheEntry{
		config: cfg,
		mtime:  mtime,
		path:   cfgPath,
	}
	configCacheMu.Unlock()

	return cfg, nil
}
