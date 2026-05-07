package model

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

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
	// Non-strict on purpose. config.yaml is operator-edited and survives
	// across Maestro versions: when a field is removed (e.g. degraded-mode
	// fallback), an existing YAML still carrying the legacy key must keep
	// loading rather than wedging the daemon on `unknown field`. Spelling
	// mistakes on truly novel fields are surfaced by `cfg.Validate()` and
	// by the missing-default behaviour itself, so strict-decode value-add
	// is small relative to the upgrade-time breakage cost.
	if err := yamlutil.SafeUnmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config.yaml: %w", err)
	}
	// Best-effort top-level typo detection. We do not fail load on
	// unknown keys (silent failure is preferable to a wedged daemon, see
	// the strict-decode comment above), but we DO want operators to
	// notice when they typo a top-level field — `wortree:` instead of
	// `worktree:` looks fine to YAML but silently disables the override.
	// This only inspects the document's top-level mapping; nested typos
	// are out of scope because they would require a full schema walk
	// and risk false positives on free-form maps (feature_profiles, etc.).
	logUnknownConfigTopLevelKeys(data)
	// Surface explicitly-retired nested keys (e.g. worktree.commit_policy).
	// Top-level unknown-key detection misses these because the parent
	// section IS known. Operators have hit this loop on legacy YAMLs
	// (Report 2026-05-05): "I left commit_policy in, why isn't it
	// working?" — the daemon ignored it silently.
	logRetiredConfigKeys(data)
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

// configTopLevelKnownKeys returns the YAML tag names used by the top-level
// fields of Config. Computed once at startup; cheap reflection.
var configTopLevelKnownKeys = func() map[string]struct{} {
	known := make(map[string]struct{})
	t := reflect.TypeOf(Config{})
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("yaml")
		if tag == "" || tag == "-" {
			continue
		}
		// "name,omitempty" → "name"
		name := tag
		if comma := indexComma(name); comma >= 0 {
			name = name[:comma]
		}
		if name == "" {
			continue
		}
		known[name] = struct{}{}
	}
	return known
}()

// indexComma returns the index of the first comma in s, or -1.
func indexComma(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			return i
		}
	}
	return -1
}

// retiredConfigPaths lists explicit nested keys whose runtime support
// was removed but whose YAML may still appear in legacy operator
// configs. Each entry is a dotted path from the document root. The
// schema decoder ignores these silently (yaml.v3 default behaviour),
// so without this surface a stale `worktree.commit_policy:` block
// would appear "active" to the operator while having no effect
// (Report 2026-05-05). Per-key reasons help the operator understand
// why removal is safe.
//
// Keep this list short and explicit. Recording every renamed field
// across the project's history would create maintenance churn; only
// list keys whose presence in an existing operator config is
// likely (i.e. shipped in a previous templates/config.yaml).
var retiredConfigPaths = map[string]string{
	"worktree.commit_policy":              "commit_policy gates (max_files / require_gitignore / message_pattern) were retired with the orchestrator-owns-commit redesign; the daemon commits Worker output verbatim. Drop the worktree.commit_policy block from your config.yaml.",
	"fallback":                            "degraded-mode worker blacklisting (fallback.enabled / consecutive_failure_threshold / recovery_check_interval_sec / min_healthy_duration_sec) was retired; per-task retry + circuit_breaker.progress_timeout cover the recovery path. Drop the fallback block.",
	"watcher.clear_second_enter_delay_ms": "Claude Code 2.x runs /clear on the first Enter; the second-Enter delay is unused. Drop watcher.clear_second_enter_delay_ms from your config.yaml.",
}

// logRetiredConfigKeys emits a WARN for each retired nested key found
// in the YAML document. Best-effort tree walk; parse failures are
// silently swallowed because the main decoder already succeeded.
func logRetiredConfigKeys(data []byte) {
	var doc yamlv3.Node
	if err := yamlv3.Unmarshal(data, &doc); err != nil {
		return
	}
	root := &doc
	if root.Kind == yamlv3.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}
	if root.Kind != yamlv3.MappingNode {
		return
	}
	for path, reason := range retiredConfigPaths {
		if nodeHasPath(root, splitDotPath(path)) {
			slog.Warn("config_retired_key_present",
				"path", path,
				"hint", reason)
		}
	}
}

// splitDotPath splits "a.b.c" into ["a","b","c"]. Empty input returns nil.
func splitDotPath(s string) []string {
	if s == "" {
		return nil
	}
	out := make([]string, 0, 4)
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// nodeHasPath walks a YAML mapping tree looking for the dotted-key
// sequence. Returns true iff every segment resolves to a child node.
// Non-mapping intermediate nodes (sequence, scalar) abort the walk.
func nodeHasPath(node *yamlv3.Node, segments []string) bool {
	if len(segments) == 0 || node == nil || node.Kind != yamlv3.MappingNode {
		return false
	}
	head, tail := segments[0], segments[1:]
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i]
		if key.Kind != yamlv3.ScalarNode || key.Value != head {
			continue
		}
		if len(tail) == 0 {
			return true
		}
		return nodeHasPath(node.Content[i+1], tail)
	}
	return false
}

// logUnknownConfigTopLevelKeys emits a single WARN listing top-level keys in
// config.yaml that are not part of Config's known schema. Best-effort: any
// parse failure here is silently swallowed because the main decoder above
// already succeeded, so we know the YAML is structurally valid.
func logUnknownConfigTopLevelKeys(data []byte) {
	var doc yamlv3.Node
	if err := yamlv3.Unmarshal(data, &doc); err != nil {
		return
	}
	root := &doc
	if root.Kind == yamlv3.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}
	if root.Kind != yamlv3.MappingNode {
		return
	}
	var unknown []string
	// MappingNode.Content alternates [key, value, key, value, ...].
	for i := 0; i+1 < len(root.Content); i += 2 {
		key := root.Content[i]
		if key.Kind != yamlv3.ScalarNode {
			continue
		}
		if _, ok := configTopLevelKnownKeys[key.Value]; ok {
			continue
		}
		unknown = append(unknown, key.Value)
	}
	if len(unknown) == 0 {
		return
	}
	sort.Strings(unknown)
	slog.Warn("config_unknown_top_level_keys",
		"keys", unknown,
		"hint", "these top-level fields are not recognised by Maestro and will be silently ignored — verify they are not typos (e.g. wortree -> worktree) or legacy fields that can be removed")
}
