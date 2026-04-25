package model

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	yamlv3 "gopkg.in/yaml.v3"
)

// VerifyConfig holds the verification commands defined in verify.yaml.
type VerifyConfig struct {
	Build       []string `yaml:"build"`
	Lint        []string `yaml:"lint"`
	Test        []string `yaml:"test"`
	Typecheck   []string `yaml:"typecheck"`
	Security    []string `yaml:"security,omitempty"`    // C-3: セキュリティ検証コマンド
	Performance []string `yaml:"performance,omitempty"` // C-3: パフォーマンスベンチコマンド
}

// VerifyResult holds the result of executing a single verification command.
type VerifyResult struct {
	Category string        `yaml:"category"`
	Command  string        `yaml:"command"`
	Passed   bool          `yaml:"passed"`
	Output   string        `yaml:"output"`
	Duration time.Duration `yaml:"duration"`
	ExitCode int           `yaml:"exit_code"`
}

// verifyFile is the on-disk YAML wrapper for verify.yaml.
type verifyFile struct {
	Verify VerifyConfig `yaml:"verify"`
}

// dangerousChars are shell meta-characters that are rejected by Validate.
var dangerousChars = []string{";", "&&", "||", "`", "$(", "${", "|", "<", ">", "\n", "\r"}

// DefaultVerifyConfig returns a minimal verification config with safe defaults.
func DefaultVerifyConfig() *VerifyConfig {
	return &VerifyConfig{
		Build: []string{"go vet ./..."},
	}
}

// IsEmpty reports whether the config has no commands in any category.
func (v *VerifyConfig) IsEmpty() bool {
	return len(v.Build) == 0 && len(v.Lint) == 0 && len(v.Test) == 0 && len(v.Typecheck) == 0 &&
		len(v.Security) == 0 && len(v.Performance) == 0
}

// AllCommands returns all commands in category order: build, lint, test, typecheck, security, performance.
func (v *VerifyConfig) AllCommands() []string {
	cmds := make([]string, 0, len(v.Build)+len(v.Lint)+len(v.Test)+len(v.Typecheck)+len(v.Security)+len(v.Performance))
	cmds = append(cmds, v.Build...)
	cmds = append(cmds, v.Lint...)
	cmds = append(cmds, v.Test...)
	cmds = append(cmds, v.Typecheck...)
	cmds = append(cmds, v.Security...)
	cmds = append(cmds, v.Performance...)
	return cmds
}

// Validate checks that all commands are safe (no shell meta-characters).
func (v *VerifyConfig) Validate() error {
	for _, cmd := range v.AllCommands() {
		if strings.TrimSpace(cmd) == "" {
			return fmt.Errorf("verify config: empty command")
		}
		for _, ch := range dangerousChars {
			if strings.Contains(cmd, ch) {
				return fmt.Errorf("verify config: dangerous character %q in command %q", ch, cmd)
			}
		}
	}
	return nil
}

// LoadOrDefaultVerifyConfig reads and parses a verify.yaml file.
// If the file does not exist, it returns DefaultVerifyConfig() as a fallback.
// If the file exists but cannot be parsed, it returns an error.
func LoadOrDefaultVerifyConfig(path string) (*VerifyConfig, error) {
	cfg, err := LoadVerifyConfig(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return DefaultVerifyConfig(), nil
		}
		return nil, err
	}
	return cfg, nil
}

// LoadVerifyConfig reads and parses a verify.yaml file.
// The parsed config is validated via Validate() so that dangerous shell
// meta-characters in command strings are rejected at load time. Callers that
// rely on a Fallback (DefaultVerifyConfig) should use LoadOrDefaultVerifyConfig.
func LoadVerifyConfig(path string) (*VerifyConfig, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is a config file path from validated inputs
	if err != nil {
		return nil, fmt.Errorf("load verify config: %w", err)
	}
	var f verifyFile
	if err := yamlv3.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("load verify config: %w", err)
	}
	if err := f.Verify.Validate(); err != nil {
		return nil, fmt.Errorf("load verify config: %w", err)
	}
	return &f.Verify, nil
}

// SaveVerifyConfig writes a VerifyConfig to the given path atomically.
func SaveVerifyConfig(path string, config *VerifyConfig) error {
	f := verifyFile{Verify: *config}
	content, err := yamlv3.Marshal(&f)
	if err != nil {
		return fmt.Errorf("save verify config: yaml marshal: %w", err)
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".verify-tmp-*.yaml")
	if err != nil {
		return fmt.Errorf("save verify config: create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			os.Remove(tmpName) //nolint:errcheck // best-effort cleanup
		}
	}()

	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return fmt.Errorf("save verify config: write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("save verify config: sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("save verify config: close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("save verify config: rename: %w", err)
	}
	tmpName = "" // Rename succeeded; prevent deferred cleanup from removing the target.
	return nil
}
