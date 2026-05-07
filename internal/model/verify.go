package model

import (
	"errors"
	"fmt"
	"io"
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

// unsupportedCommandChars only rejects characters that break the YAML
// document itself or split a single command across lines (newlines /
// carriage returns). Shell metacharacters (`;`, `&&`, `||`, “ ` “, `$(`,
// `${`, `|`, `<`, `>`) used to be rejected as "direct exec" defense-in-
// depth, but verify commands now run under `bash -c` so the LLM-authored
// snapshot can use the natural shell syntax it would otherwise resort to
// (`bash -lc "sleep 35; flutter analyze"`, command pipelines, env
// substitutions, …). Security is the responsibility of the operator's
// `~/.claude` policy hook, which inspects every spawned process —
// duplicating that check here only makes the verify path brittle.
var unsupportedCommandChars = []string{"\n", "\r"}

// VerifyCommand is a parsed verify command. With the 2026-05-06 shell-
// passthrough redesign, Args is always [bash, -c, <cmd>] — Env is no
// longer split out at parse time because `bash -c` understands `KEY=val
// cmd` itself. The struct is preserved for API compatibility with
// existing callers.
type VerifyCommand struct {
	Env  []string
	Args []string
}

// DefaultVerifyConfig returns a minimal verification config with a safe,
// language-agnostic baseline.
//
// The fallback uses `git diff --check`, which works for any git repository
// regardless of language and still satisfies the §5-6 evolution invariant
// ("evolution must not run with zero verification commands") because the
// slice is non-empty. Operators who want richer verification should write
// `.maestro/verify.yaml` — that file is the language-agnostic source of
// truth; the daemon does not guess what verify means for the project.
func DefaultVerifyConfig() *VerifyConfig {
	return &VerifyConfig{
		Build: []string{"git diff --check"},
	}
}

// DefaultVerifyConfigForProject returns the language-agnostic baseline.
// projectRoot is accepted for backward compatibility with call sites that
// thread the project path through, but is no longer consulted: language
// detection has been removed (see DefaultVerifyConfig for the rationale).
func DefaultVerifyConfigForProject(projectRoot string) *VerifyConfig {
	_ = projectRoot
	return DefaultVerifyConfig()
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

// Validate checks that no command contains a YAML-document-breaking
// character (newline / carriage return). Shell metacharacters are
// allowed — the verify runner executes commands under `bash -c`.
func (v *VerifyConfig) Validate() error {
	for _, cmd := range v.AllCommands() {
		if strings.TrimSpace(cmd) == "" {
			return fmt.Errorf("verify config: empty command")
		}
		for _, ch := range unsupportedCommandChars {
			if strings.Contains(cmd, ch) {
				return fmt.Errorf("verify config: unsupported character %q in command %q (commands must be a single line)", ch, cmd)
			}
		}
	}
	return nil
}

// ParseVerifyCommand wraps a verify command for `bash -c` execution.
// The command is passed verbatim to bash so the LLM-authored snapshot
// can use shell metacharacters (`;`, `&&`, pipelines, env substitution,
// …) without having to learn a separate direct-exec grammar. Returns
// the canonical Args = [bash, -c, <cmd>] used by execVerifyCommand.
func ParseVerifyCommand(command string) (VerifyCommand, error) {
	trimmed := strings.TrimSpace(command)
	if trimmed == "" {
		return VerifyCommand{}, fmt.Errorf("empty command")
	}
	return VerifyCommand{
		Args: []string{"bash", "-c", trimmed},
	}, nil
}

// LoadOrDefaultVerifyConfig reads and parses a verify.yaml file.
// If the file does not exist, it returns DefaultVerifyConfig() as a fallback.
// If the file exists but cannot be parsed, it returns an error.
//
// New production code should prefer LoadOrDefaultVerifyConfigForProject so the
// fallback is project-aware (see DefaultVerifyConfigForProject) rather than
// hard-coded to Go's `go vet ./...`.
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

// LoadOrDefaultVerifyConfigForProject reads and parses verify.yaml at path.
// If the file does not exist it returns the project-appropriate default from
// DefaultVerifyConfigForProject(projectRoot). Non-Go repositories receive a
// repository-generic fallback (`git diff --check`) rather than a guaranteed-
// failing Go command or an empty command set.
func LoadOrDefaultVerifyConfigForProject(projectRoot, path string) (*VerifyConfig, error) {
	cfg, err := LoadVerifyConfig(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return DefaultVerifyConfigForProject(projectRoot), nil
		}
		return nil, err
	}
	return cfg, nil
}

// ParseVerifyConfigYAML decodes a verify.yaml document body with the
// same strict-decode + Validate pipeline that LoadVerifyConfig uses.
// CLI and UDS write paths share this helper so that an unknown
// category (e.g. `slow_lint:`) is rejected with the same helpful
// error in every code path — previously the CLI / UDS paths used a
// non-strict yamlv3.Unmarshal and the unknown entry was silently
// dropped, surfacing only as "verify config must contain at least
// one command" when the surviving config happened to be empty
// (Report 2026-05-06 P0-1 / P1-1).
func ParseVerifyConfigYAML(data []byte) (*VerifyConfig, error) {
	var f verifyFile
	dec := yamlv3.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w (allowed categories: build, lint, test, typecheck, security, performance)", err)
	}
	if err := f.Verify.Validate(); err != nil {
		return nil, err
	}
	return &f.Verify, nil
}

// LoadVerifyConfig reads and parses a verify.yaml file.
// The parsed config is validated via Validate() so that unsupported
// commands are rejected at load time. Strict YAML decoding (KnownFields
// = true) rejects unknown verify categories with a helpful error rather
// than silently dropping them — a Planner that wrote `verify: { slow_lint:
// [...] }` used to see "verify config must contain at least one command"
// because every entry under an unknown key was discarded; now the error
// names the offending field so the caller can correct the snapshot.
// Callers that rely on a Fallback (DefaultVerifyConfig) should use
// LoadOrDefaultVerifyConfig.
func LoadVerifyConfig(path string) (*VerifyConfig, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is a config file path from validated inputs
	if err != nil {
		return nil, fmt.Errorf("load verify config: %w", err)
	}
	cfg, err := ParseVerifyConfigYAML(data)
	if err != nil {
		return nil, fmt.Errorf("load verify config: %w", err)
	}
	return cfg, nil
}

// SaveVerifyConfig writes a VerifyConfig to the given path atomically.
func SaveVerifyConfig(path string, config *VerifyConfig) error {
	f := verifyFile{Verify: *config}
	content, err := yamlv3.Marshal(&f)
	if err != nil {
		return fmt.Errorf("save verify config: yaml marshal: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil { //nolint:gosec // config directories are user-readable application state
		return fmt.Errorf("save verify config: create dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".verify-tmp-*.yaml")
	if err != nil {
		return fmt.Errorf("save verify config: create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName) //nolint:errcheck,gosec // best-effort cleanup
		}
	}()

	if _, err := tmp.Write(content); err != nil {
		writeErr := fmt.Errorf("save verify config: write temp: %w", err)
		if closeErr := tmp.Close(); closeErr != nil {
			return errors.Join(writeErr, fmt.Errorf("close temp: %w", closeErr))
		}
		return writeErr
	}
	if err := tmp.Sync(); err != nil {
		syncErr := fmt.Errorf("save verify config: sync temp: %w", err)
		if closeErr := tmp.Close(); closeErr != nil {
			return errors.Join(syncErr, fmt.Errorf("close temp: %w", closeErr))
		}
		return syncErr
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("save verify config: close temp: %w", err)
	}
	// Both tmpName and path live under the controlled maestroDir layout
	// (tmp is created via os.CreateTemp in Dir(path)); nothing on either
	// path is user-controlled, so gosec G703 is a false positive here.
	if err := os.Rename(tmpName, path); err != nil { //nolint:gosec // controlled maestroDir paths
		return fmt.Errorf("save verify config: rename: %w", err)
	}
	tmpName = "" // Rename succeeded; prevent deferred cleanup from removing the target.
	return nil
}
