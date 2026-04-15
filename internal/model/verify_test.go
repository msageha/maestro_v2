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
			name:    "empty config is valid",
			config:  VerifyConfig{},
			wantErr: false,
		},
		{
			name:    "semicolon rejected",
			config:  VerifyConfig{Build: []string{"go build; rm -rf /"}},
			wantErr: true,
			errMsg:  "dangerous character \";\"",
		},
		{
			name:    "double ampersand rejected",
			config:  VerifyConfig{Lint: []string{"true && rm -rf /"}},
			wantErr: true,
			errMsg:  "dangerous character \"&&\"",
		},
		{
			name:    "double pipe rejected",
			config:  VerifyConfig{Test: []string{"false || rm -rf /"}},
			wantErr: true,
			errMsg:  "dangerous character \"||\"",
		},
		{
			name:    "backtick rejected",
			config:  VerifyConfig{Build: []string{"echo `whoami`"}},
			wantErr: true,
			errMsg:  "dangerous character \"`\"",
		},
		{
			name:    "dollar paren rejected",
			config:  VerifyConfig{Build: []string{"echo $(whoami)"}},
			wantErr: true,
			errMsg:  "dangerous character \"$(\"",
		},
		{
			name:    "dollar brace rejected",
			config:  VerifyConfig{Build: []string{"echo ${HOME}"}},
			wantErr: true,
			errMsg:  "dangerous character \"${\"",
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
			errMsg:  `dangerous character "|"`,
		},
		{
			name:    "less-than rejected",
			config:  VerifyConfig{Build: []string{"cmd < input.txt"}},
			wantErr: true,
			errMsg:  `dangerous character "<"`,
		},
		{
			name:    "greater-than rejected",
			config:  VerifyConfig{Build: []string{"cmd > output.txt"}},
			wantErr: true,
			errMsg:  `dangerous character ">"`,
		},
		{
			name:    "newline rejected",
			config:  VerifyConfig{Build: []string{"cmd1\ncmd2"}},
			wantErr: true,
			errMsg:  "dangerous character",
		},
		{
			name:    "carriage return rejected",
			config:  VerifyConfig{Build: []string{"cmd1\rcmd2"}},
			wantErr: true,
			errMsg:  "dangerous character",
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

func TestVerifyReport_Fields(t *testing.T) {
	report := VerifyReport{
		TaskID: "task_123",
		Results: []VerifyResult{
			{Category: "build", Command: "go build ./...", Passed: true, ExitCode: 0},
			{Category: "test", Command: "go test ./...", Passed: false, ExitCode: 1, Output: "FAIL"},
		},
		AllPassed: false,
	}

	assert.Equal(t, "task_123", report.TaskID)
	assert.Len(t, report.Results, 2)
	assert.True(t, report.Results[0].Passed)
	assert.False(t, report.Results[1].Passed)
	assert.False(t, report.AllPassed)
}
