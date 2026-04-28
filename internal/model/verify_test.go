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

func TestDefaultVerifyConfig(t *testing.T) {
	cfg := DefaultVerifyConfig()
	require.NotNil(t, cfg)
	assert.Equal(t, []string{"go vet ./..."}, cfg.Build)
	assert.Empty(t, cfg.Lint)
	assert.Empty(t, cfg.Test)
	assert.Empty(t, cfg.Typecheck)
	assert.False(t, cfg.IsEmpty())
	assert.NoError(t, cfg.Validate())
}

func TestDefaultVerifyConfigForProject_NonGoUsesGitDiffCheck(t *testing.T) {
	dir := t.TempDir()

	cfg := DefaultVerifyConfigForProject(dir)
	require.NotNil(t, cfg)
	assert.Equal(t, []string{"git diff --check"}, cfg.Build)
	assert.False(t, cfg.IsEmpty())
	assert.NoError(t, cfg.Validate())
}

func TestDefaultVerifyConfigForProject_GoUsesGoVet(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module test\n"), 0o644))

	cfg := DefaultVerifyConfigForProject(dir)
	require.NotNil(t, cfg)
	assert.Equal(t, []string{"go vet ./..."}, cfg.Build)
	// Go projects automatically get gosec for Security and go-bench for Performance
	// so extended_verification.security_check / performance_bench have something
	// to run without relying on the operator hand-writing verify.yaml.
	assert.Equal(t, []string{"gosec ./..."}, cfg.Security)
	assert.Equal(t, []string{"go test -bench=. ./..."}, cfg.Performance)
	assert.False(t, cfg.IsEmpty())
	assert.NoError(t, cfg.Validate())
}

func TestDetectProjectLanguage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		marker string // file to create at projectRoot
		want   string
	}{
		{"empty dir", "", ""},
		{"go.mod", "go.mod", "go"},
		{"package.json", "package.json", "node"},
		{"pyproject.toml", "pyproject.toml", "python"},
		{"requirements.txt", "requirements.txt", "python"},
		{"setup.py", "setup.py", "python"},
		{"Cargo.toml", "Cargo.toml", "rust"},
		{"Gemfile", "Gemfile", "ruby"},
		{"pom.xml", "pom.xml", "java"},
		{"build.gradle", "build.gradle", "java"},
		{"build.gradle.kts", "build.gradle.kts", "java"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if tt.marker != "" {
				require.NoError(t, os.WriteFile(filepath.Join(dir, tt.marker), []byte(""), 0o644))
			}
			assert.Equal(t, tt.want, DetectProjectLanguage(dir))
		})
	}
}

func TestDetectProjectLanguage_EmptyRoot(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "", DetectProjectLanguage(""))
}

func TestDetectProjectLanguage_GoTakesPrecedenceOverNode(t *testing.T) {
	// Polyglot repo (Go + Node) classifies as Go because go.mod is probed first.
	// Operators with this layout should write an explicit verify.yaml.
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte(""), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0o644))
	assert.Equal(t, "go", DetectProjectLanguage(dir))
}

func TestDefaultSecurityCommandsForLanguage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		language string
		want     []string
	}{
		{"go", []string{"gosec ./..."}},
		{"node", []string{"npm audit --audit-level=high"}},
		{"python", []string{"uvx pip-audit"}},
		{"rust", []string{"cargo audit"}},
		{"ruby", []string{"bundle audit check"}},
		{"java", nil},
		{"", nil},
		{"unknown", nil},
	}
	for _, tt := range tests {
		t.Run(tt.language, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, DefaultSecurityCommandsForLanguage(tt.language))
		})
	}
}

func TestDefaultPerformanceCommandsForLanguage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		language string
		want     []string
	}{
		{"go", []string{"go test -bench=. ./..."}},
		{"rust", []string{"cargo bench"}},
		{"node", nil},
		{"python", nil},
		{"ruby", nil},
		{"", nil},
	}
	for _, tt := range tests {
		t.Run(tt.language, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, DefaultPerformanceCommandsForLanguage(tt.language))
		})
	}
}

func TestDefaultVerifyConfigForProject_NodePopulatesSecurityNotPerformance(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0o644))

	cfg := DefaultVerifyConfigForProject(dir)
	require.NotNil(t, cfg)
	assert.Equal(t, []string{"git diff --check"}, cfg.Build)
	assert.Equal(t, []string{"npm audit --audit-level=high"}, cfg.Security)
	assert.Empty(t, cfg.Performance, "node projects have no canonical bench runner")
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
	assert.Equal(t, []string{"go vet ./..."}, cfg.Build)
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
