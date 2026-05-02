package model

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
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

var (
	unsupportedCommandChars = []string{";", "&&", "||", "`", "$(", "${", "|", "<", ">", "\n", "\r"}
	envAssignmentName       = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// VerifyCommand is a parsed direct-exec verify command.
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

// Validate checks that all commands are simple direct-exec invocations.
func (v *VerifyConfig) Validate() error {
	for _, cmd := range v.AllCommands() {
		if strings.TrimSpace(cmd) == "" {
			return fmt.Errorf("verify config: empty command")
		}
		for _, ch := range unsupportedCommandChars {
			if strings.Contains(cmd, ch) {
				return fmt.Errorf("verify config: unsupported character %q in command %q", ch, cmd)
			}
		}
		parsed, err := ParseVerifyCommand(cmd)
		if err != nil {
			return fmt.Errorf("verify config: invalid command %q: %w", cmd, err)
		}
		if isShellCInvocation(parsed.Args) {
			return fmt.Errorf("verify config: shell -c is not supported in command %q", cmd)
		}
	}
	return nil
}

func isShellCInvocation(args []string) bool {
	if len(args) < 2 {
		return false
	}
	exe := filepath.Base(args[0])
	if exe != "sh" && exe != "bash" {
		return false
	}
	return strings.HasPrefix(args[1], "-") && strings.Contains(args[1], "c")
}

// ParseVerifyCommand parses the limited verify command grammar used for direct
// exec: optional leading KEY=VALUE environment assignments followed by argv.
func ParseVerifyCommand(command string) (VerifyCommand, error) {
	fields, err := splitVerifyFields(command)
	if err != nil {
		return VerifyCommand{}, err
	}
	var parsed VerifyCommand
	i := 0
	for ; i < len(fields); i++ {
		name, ok := envAssignmentNameOf(fields[i])
		if !ok {
			break
		}
		if !envAssignmentName.MatchString(name) {
			return VerifyCommand{}, fmt.Errorf("invalid env assignment name %q", name)
		}
		parsed.Env = append(parsed.Env, fields[i])
	}
	if i >= len(fields) {
		return VerifyCommand{}, fmt.Errorf("missing executable")
	}
	parsed.Args = fields[i:]
	return parsed, nil
}

func envAssignmentNameOf(field string) (string, bool) {
	idx := strings.IndexByte(field, '=')
	if idx <= 0 {
		return "", false
	}
	return field[:idx], true
}

func splitVerifyFields(command string) ([]string, error) {
	var fields []string
	var b strings.Builder
	inSingle := false
	inDouble := false
	haveToken := false

	flush := func() {
		if haveToken {
			fields = append(fields, b.String())
			b.Reset()
			haveToken = false
		}
	}

	for i := 0; i < len(command); i++ {
		ch := command[i]
		switch {
		case inSingle:
			if ch == '\'' {
				inSingle = false
			} else {
				b.WriteByte(ch)
				haveToken = true
			}
		case inDouble:
			switch ch {
			case '"':
				inDouble = false
			case '\\':
				i++
				if i >= len(command) {
					return nil, fmt.Errorf("trailing escape")
				}
				b.WriteByte(command[i])
				haveToken = true
			default:
				b.WriteByte(ch)
				haveToken = true
			}
		default:
			switch ch {
			case '\'':
				inSingle = true
				haveToken = true
			case '"':
				inDouble = true
				haveToken = true
			case '\\':
				i++
				if i >= len(command) {
					return nil, fmt.Errorf("trailing escape")
				}
				b.WriteByte(command[i])
				haveToken = true
			case ' ', '\t':
				flush()
			default:
				b.WriteByte(ch)
				haveToken = true
			}
		}
	}
	if inSingle || inDouble {
		return nil, fmt.Errorf("unterminated quote")
	}
	flush()
	return fields, nil
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

// LoadVerifyConfig reads and parses a verify.yaml file.
// The parsed config is validated via Validate() so that unsupported shell
// syntax in command strings is rejected at load time. Callers that
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
