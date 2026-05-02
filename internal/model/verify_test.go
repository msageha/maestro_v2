package model

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	yamlv3 "gopkg.in/yaml.v3"
)

func TestVerifyConfig_YAMLRoundTrip(t *testing.T) {
	original := VerifyConfig{
		Build:     []string{"go build ./..."},
		Lint:      []string{"golangci-lint run"},
		Test:      []string{"go test ./..."},
		Typecheck: []string{"go vet ./..."},
	}

	data, err := yamlv3.Marshal(&original)
	require.NoError(t, err)

	var decoded VerifyConfig
	require.NoError(t, yamlv3.Unmarshal(data, &decoded))

	assert.Equal(t, original, decoded)
}

// TestDefaultVerifyConfig pins the language-agnostic baseline. The default
// is `git diff --check`, which works for any git repository regardless of
// language and keeps the §5-6 evolution invariant satisfied (non-empty
// command list).
func TestDefaultVerifyConfig(t *testing.T) {
	cfg := DefaultVerifyConfig()
	require.NotNil(t, cfg)
	assert.Equal(t, []string{"git diff --check"}, cfg.Build)
	assert.Empty(t, cfg.Lint)
	assert.Empty(t, cfg.Test)
	assert.Empty(t, cfg.Typecheck)
	assert.Empty(t, cfg.Security)
	assert.Empty(t, cfg.Performance)
	assert.False(t, cfg.IsEmpty())
	assert.NoError(t, cfg.Validate())
}

// TestDefaultVerifyConfigForProject_AlwaysReturnsBaseline verifies that
// language detection has been removed: the result no longer depends on
// projectRoot contents. Operators tailor verification per project via
// .maestro/verify.yaml — the daemon does not auto-inject npm audit /
// pip-audit / cargo audit / gosec etc. anymore.
func TestDefaultVerifyConfigForProject_AlwaysReturnsBaseline(t *testing.T) {
	cases := []string{
		"",          // empty root
		t.TempDir(), // empty dir, no markers
		filepath.Join(t.TempDir(), "missing"),
	}
	for _, dir := range cases {
		t.Run(dir, func(t *testing.T) {
			cfg := DefaultVerifyConfigForProject(dir)
			require.NotNil(t, cfg)
			assert.Equal(t, []string{"git diff --check"}, cfg.Build)
			assert.Empty(t, cfg.Security)
			assert.Empty(t, cfg.Performance)
		})
	}
}

// Polyglot fixture: even when go.mod and package.json sit side by side
// in the same directory, the result is the same language-agnostic
// baseline. This pins the fact that DetectProjectLanguage no longer
// exists.
func TestDefaultVerifyConfigForProject_PolyglotIgnoresMarkers(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module test\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0o644))

	cfg := DefaultVerifyConfigForProject(dir)
	require.NotNil(t, cfg)
	assert.Equal(t, []string{"git diff --check"}, cfg.Build)
}

func TestVerifyConfig_IsEmpty(t *testing.T) {
	tests := []struct {
		name   string
		config VerifyConfig
		want   bool
	}{
		{
			name:   "all empty",
			config: VerifyConfig{},
			want:   true,
		},
		{
			name:   "build only",
			config: VerifyConfig{Build: []string{"go build ./..."}},
			want:   false,
		},
		{
			name:   "lint only",
			config: VerifyConfig{Lint: []string{"golangci-lint run"}},
			want:   false,
		},
		{
			name:   "test only",
			config: VerifyConfig{Test: []string{"go test ./..."}},
			want:   false,
		},
		{
			name:   "typecheck only",
			config: VerifyConfig{Typecheck: []string{"go vet ./..."}},
			want:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.config.IsEmpty())
		})
	}
}

func TestVerifyConfig_AllCommands_CategoryOrder(t *testing.T) {
	cfg := VerifyConfig{
		Build:     []string{"b1", "b2"},
		Lint:      []string{"l1"},
		Test:      []string{"t1", "t2"},
		Typecheck: []string{"tc1"},
	}

	got := cfg.AllCommands()
	want := []string{"b1", "b2", "l1", "t1", "t2", "tc1"}
	assert.Equal(t, want, got)
}

func TestVerifyConfig_AllCommands_Empty(t *testing.T) {
	cfg := VerifyConfig{}
	assert.Empty(t, cfg.AllCommands())
}

func TestVerifyConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		config  VerifyConfig
		wantErr bool
		errMsg  string
	}{
		{
			name:    "valid commands",
			config:  VerifyConfig{Build: []string{"go build ./..."}, Test: []string{"go test -v ./..."}},
			wantErr: false,
		},
		{
			name:    "valid quoted argument",
			config:  VerifyConfig{Build: []string{`printf "hello world"`}},
			wantErr: false,
		},
		{
			name:    "valid env assignment",
			config:  VerifyConfig{Build: []string{`CGO_ENABLED=0 go test ./...`}},
			wantErr: false,
		},
		{
			name:    "empty config is valid",
			config:  VerifyConfig{},
			wantErr: false,
		},
		{
			name:    "semicolon rejected",
			config:  VerifyConfig{Build: []string{"go build; rm -rf /"}},
			wantErr: true,
			errMsg:  "unsupported character \";\"",
		},
		{
			name:    "double ampersand rejected",
			config:  VerifyConfig{Lint: []string{"true && rm -rf /"}},
			wantErr: true,
			errMsg:  "unsupported character \"&&\"",
		},
		{
			name:    "double pipe rejected",
			config:  VerifyConfig{Test: []string{"false || rm -rf /"}},
			wantErr: true,
			errMsg:  "unsupported character \"||\"",
		},
		{
			name:    "backtick rejected",
			config:  VerifyConfig{Build: []string{"echo `whoami`"}},
			wantErr: true,
			errMsg:  "unsupported character \"`\"",
		},
		{
			name:    "dollar paren rejected",
			config:  VerifyConfig{Build: []string{"echo $(whoami)"}},
			wantErr: true,
			errMsg:  "unsupported character \"$(\"",
		},
		{
			name:    "dollar brace rejected",
			config:  VerifyConfig{Build: []string{"echo ${HOME}"}},
			wantErr: true,
			errMsg:  "unsupported character \"${\"",
		},
		{
			name:    "empty command rejected",
			config:  VerifyConfig{Build: []string{""}},
			wantErr: true,
			errMsg:  "empty command",
		},
		{
			name:    "whitespace-only command rejected",
			config:  VerifyConfig{Build: []string{"   "}},
			wantErr: true,
			errMsg:  "empty command",
		},
		{
			name:    "pipe rejected",
			config:  VerifyConfig{Build: []string{"cmd1 | cmd2"}},
			wantErr: true,
			errMsg:  `unsupported character "|"`,
		},
		{
			name:    "less-than rejected",
			config:  VerifyConfig{Build: []string{"cmd < input.txt"}},
			wantErr: true,
			errMsg:  `unsupported character "<"`,
		},
		{
			name:    "greater-than rejected",
			config:  VerifyConfig{Build: []string{"cmd > output.txt"}},
			wantErr: true,
			errMsg:  `unsupported character ">"`,
		},
		{
			name:    "newline rejected",
			config:  VerifyConfig{Build: []string{"cmd1\ncmd2"}},
			wantErr: true,
			errMsg:  "unsupported character",
		},
		{
			name:    "carriage return rejected",
			config:  VerifyConfig{Build: []string{"cmd1\rcmd2"}},
			wantErr: true,
			errMsg:  "unsupported character",
		},
		{
			name:    "unterminated quote rejected",
			config:  VerifyConfig{Build: []string{`printf "unterminated`}},
			wantErr: true,
			errMsg:  "unterminated quote",
		},
		{
			name:    "shell c rejected",
			config:  VerifyConfig{Build: []string{`sh -c "go test ./..."`}},
			wantErr: true,
			errMsg:  "shell -c is not supported",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errMsg)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestParseVerifyCommand(t *testing.T) {
	t.Parallel()
	got, err := ParseVerifyCommand(`CGO_ENABLED=0 go test -run "Test With Space" ./...`)
	require.NoError(t, err)
	assert.Equal(t, []string{"CGO_ENABLED=0"}, got.Env)
	assert.Equal(t, []string{"go", "test", "-run", "Test With Space", "./..."}, got.Args)
}

func TestLoadSaveVerifyConfig_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "verify.yaml")

	original := &VerifyConfig{
		Build:     []string{"go build ./..."},
		Lint:      []string{"golangci-lint run", "staticcheck ./..."},
		Test:      []string{"go test -race ./..."},
		Typecheck: []string{"go vet ./..."},
	}

	require.NoError(t, SaveVerifyConfig(path, original))

	loaded, err := LoadVerifyConfig(path)
	require.NoError(t, err)
	assert.Equal(t, original, loaded)
}

func TestLoadVerifyConfig_FileNotFound(t *testing.T) {
	_, err := LoadVerifyConfig("/nonexistent/verify.yaml")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "load verify config")
}

func TestLoadVerifyConfig_RejectsUnsupportedChars(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "verify.yaml")

	// verify.yaml whose build command contains a shell syntax character (&&)
	// must be rejected at load time. Otherwise, downstream consumers might
	// pass the string to a shell and execute the second arm.
	content := `verify:
  build:
    - go build ./... && rm -rf /
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))

	_, err := LoadVerifyConfig(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported character")
}

func TestLoadOrDefaultVerifyConfig_RejectsUnsupportedChars(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "verify.yaml")

	content := `verify:
  test:
    - go test ./... | tee /tmp/out
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))

	_, err := LoadOrDefaultVerifyConfig(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported character")
}

func TestLoadVerifyConfig_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "verify.yaml")

	require.NoError(t, os.WriteFile(path, []byte("{{invalid yaml"), 0644))

	_, err := LoadVerifyConfig(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "load verify config")
}

func TestLoadVerifyConfig_SchemaFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "verify.yaml")

	content := `verify:
  build:
    - go build ./...
  lint:
    - golangci-lint run
  test:
    - go test ./...
  typecheck:
    - go vet ./...
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))

	cfg, err := LoadVerifyConfig(path)
	require.NoError(t, err)
	assert.Equal(t, []string{"go build ./..."}, cfg.Build)
	assert.Equal(t, []string{"golangci-lint run"}, cfg.Lint)
	assert.Equal(t, []string{"go test ./..."}, cfg.Test)
	assert.Equal(t, []string{"go vet ./..."}, cfg.Typecheck)
}

func TestSaveVerifyConfig_WritesVerifyWrapper(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "verify.yaml")

	cfg := &VerifyConfig{Build: []string{"go build ./..."}}
	require.NoError(t, SaveVerifyConfig(path, cfg))

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	var f verifyFile
	require.NoError(t, yamlv3.Unmarshal(data, &f))
	assert.Equal(t, []string{"go build ./..."}, f.Verify.Build)
}

func TestLoadOrDefaultVerifyConfig_FileExists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "verify.yaml")

	content := `verify:
  build:
    - go build ./...
  lint:
    - golangci-lint run
  test:
    - go test ./...
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))

	cfg, err := LoadOrDefaultVerifyConfig(path)
	require.NoError(t, err)
	assert.Equal(t, []string{"go build ./..."}, cfg.Build)
	assert.Equal(t, []string{"golangci-lint run"}, cfg.Lint)
	assert.Equal(t, []string{"go test ./..."}, cfg.Test)
}

func TestLoadOrDefaultVerifyConfig_FileNotFound(t *testing.T) {
	cfg, err := LoadOrDefaultVerifyConfig("/nonexistent/verify.yaml")
	require.NoError(t, err)
	assert.Equal(t, DefaultVerifyConfig(), cfg)
	assert.Equal(t, []string{"git diff --check"}, cfg.Build)
	assert.Empty(t, cfg.Lint)
}

func TestLoadOrDefaultVerifyConfig_ParseError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "verify.yaml")

	require.NoError(t, os.WriteFile(path, []byte("{{invalid yaml"), 0644))

	_, err := LoadOrDefaultVerifyConfig(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "load verify config")
}
