package quality

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoader_LoadFromBytes(t *testing.T) {
	loader := NewLoader(".")

	tests := []struct {
		name    string
		yaml    string
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid configuration",
			yaml: `
schema_version: "1.0.0"
gates:
  - id: test_gate
    name: "Test Gate"
    type: pre_task
    rules:
      - id: rule1
        condition:
          type: field_validation
          field: task.id
          operator: exists
    action:
      on_pass: allow
      on_fail: block
`,
			wantErr: false,
		},
		{
			name: "missing schema version",
			yaml: `
gates:
  - id: test_gate
    name: "Test Gate"
    type: pre_task
    rules:
      - id: rule1
        condition:
          type: field_validation
          field: task.id
          operator: exists
    action:
      on_pass: allow
      on_fail: block
`,
			wantErr: true,
			errMsg:  "schema_version is required",
		},
		{
			name: "invalid gate type",
			yaml: `
schema_version: "1.0.0"
gates:
  - id: test_gate
    name: "Test Gate"
    type: invalid_type
    rules:
      - id: rule1
        condition:
          type: field_validation
          field: task.id
          operator: exists
    action:
      on_pass: allow
      on_fail: block
`,
			wantErr: true,
			errMsg:  "invalid type",
		},
		{
			name: "missing rules",
			yaml: `
schema_version: "1.0.0"
gates:
  - id: test_gate
    name: "Test Gate"
    type: pre_task
    rules: []
    action:
      on_pass: allow
      on_fail: block
`,
			wantErr: true,
			errMsg:  "must have at least one rule",
		},
		{
			name: "invalid regex pattern",
			yaml: `
schema_version: "1.0.0"
gates:
  - id: test_gate
    name: "Test Gate"
    type: pre_task
    rules:
      - id: rule1
        condition:
          type: field_validation
          field: task.content
          operator: matches
          value: "[invalid(regex"
    action:
      on_pass: allow
      on_fail: block
`,
			wantErr: true,
			errMsg:  "invalid regex pattern",
		},
		{
			name: "complex logical conditions",
			yaml: `
schema_version: "1.0.0"
gates:
  - id: complex_gate
    name: "Complex Gate"
    type: pre_task
    rules:
      - id: complex_rule
        condition:
          type: and
          conditions:
            - type: field_validation
              field: task.id
              operator: exists
            - type: or
              conditions:
                - type: field_validation
                  field: task.bloom_level
                  operator: gte
                  value: 3
                - type: field_validation
                  field: task.priority
                  operator: equals
                  value: "high"
    action:
      on_pass: allow
      on_fail: block
`,
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config, err := loader.LoadFromBytes([]byte(tt.yaml))

			if tt.wantErr {
				assert.Error(t, err)
				if tt.errMsg != "" {
					assert.Contains(t, err.Error(), tt.errMsg)
				}
			} else {
				require.NoError(t, err)
				assert.NotNil(t, config)
				assert.Equal(t, "1.0.0", config.SchemaVersion)
				assert.NotEmpty(t, config.Gates)
			}
		})
	}
}

func TestLoader_LoadFromFile(t *testing.T) {
	// Create a temporary directory for test files
	tmpDir, err := os.MkdirTemp("", "loader_test")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	loader := NewLoader(tmpDir)

	// Create a test YAML file
	testFile := filepath.Join(tmpDir, "test_gates.yaml")
	testContent := `
schema_version: "1.0.0"
metadata:
  name: "Test Gates"
  description: "Test gate configuration"
gates:
  - id: file_test_gate
    name: "File Test Gate"
    type: post_task
    priority: 25
    rules:
      - id: rule1
        condition:
          type: field_validation
          field: result.status
          operator: in
          value: [completed, failed]
        severity: error
    action:
      on_pass: allow
      on_fail: block
`
	err = os.WriteFile(testFile, []byte(testContent), 0644)
	require.NoError(t, err)

	// Test loading the file
	config, err := loader.LoadFromFile(testFile)
	require.NoError(t, err)
	assert.NotNil(t, config)
	assert.Equal(t, "1.0.0", config.SchemaVersion)
	assert.Len(t, config.Gates, 1)
	assert.Equal(t, "file_test_gate", config.Gates[0].ID)
	assert.Equal(t, 25, config.Gates[0].Priority)

	// Test caching - second load should use cache
	config2, err := loader.LoadFromFile(testFile)
	require.NoError(t, err)
	assert.Equal(t, config, config2)

	// Verify the file is cached
	assert.Contains(t, loader.loadedFiles, testFile)
}

func TestLoader_ReloadFile(t *testing.T) {
	// Create a temporary directory for test files
	tmpDir, err := os.MkdirTemp("", "reload_test")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	loader := NewLoader(tmpDir)

	// Create initial file
	testFile := filepath.Join(tmpDir, "reload_test.yaml")
	initialContent := `
schema_version: "1.0.0"
gates:
  - id: initial_gate
    name: "Initial Gate"
    type: pre_task
    rules:
      - id: rule1
        condition:
          type: field_validation
          field: task.id
          operator: exists
    action:
      on_pass: allow
      on_fail: block
`
	err = os.WriteFile(testFile, []byte(initialContent), 0644)
	require.NoError(t, err)

	// Load the file
	config1, err := loader.LoadFromFile(testFile)
	require.NoError(t, err)
	assert.Len(t, config1.Gates, 1)
	assert.Equal(t, "initial_gate", config1.Gates[0].ID)

	// Sleep to ensure file modification time changes
	time.Sleep(10 * time.Millisecond)

	// Modify the file
	updatedContent := `
schema_version: "1.0.0"
gates:
  - id: updated_gate
    name: "Updated Gate"
    type: post_task
    rules:
      - id: rule1
        condition:
          type: field_validation
          field: result.status
          operator: exists
    action:
      on_pass: allow
      on_fail: warn
`
	err = os.WriteFile(testFile, []byte(updatedContent), 0644)
	require.NoError(t, err)

	// Reload the file
	config2, reloaded, err := loader.ReloadFile(testFile)
	require.NoError(t, err)
	assert.True(t, reloaded)
	assert.Len(t, config2.Gates, 1)
	assert.Equal(t, "updated_gate", config2.Gates[0].ID)

	// Reload again without changes - should not reload
	config3, reloaded, err := loader.ReloadFile(testFile)
	require.NoError(t, err)
	assert.False(t, reloaded)
	assert.Equal(t, config2, config3)
}

func TestLoader_LoadDirectory(t *testing.T) {
	// Create a temporary directory with multiple YAML files
	tmpDir, err := os.MkdirTemp("", "dir_test")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	loader := NewLoader(tmpDir)

	// Create multiple gate files
	gates1 := `
schema_version: "1.0.0"
gates:
  - id: gate1
    name: "Gate 1"
    type: pre_task
    rules:
      - id: rule1
        condition:
          type: field_validation
          field: task.id
          operator: exists
    action:
      on_pass: allow
      on_fail: block
`
	err = os.WriteFile(filepath.Join(tmpDir, "gates1.yaml"), []byte(gates1), 0644)
	require.NoError(t, err)

	gates2 := `
schema_version: "1.0.0"
gates:
  - id: gate2
    name: "Gate 2"
    type: post_task
    rules:
      - id: rule1
        condition:
          type: field_validation
          field: result.status
          operator: exists
    action:
      on_pass: allow
      on_fail: warn
`
	err = os.WriteFile(filepath.Join(tmpDir, "gates2.yml"), []byte(gates2), 0644)
	require.NoError(t, err)

	// Create an invalid file (should be skipped)
	invalidGate := `
invalid yaml content {{
`
	err = os.WriteFile(filepath.Join(tmpDir, "invalid.yaml"), []byte(invalidGate), 0644)
	require.NoError(t, err)

	// Load all files from directory
	configs, err := loader.LoadDirectory(tmpDir)
	require.NoError(t, err)
	assert.Len(t, configs, 2) // Should load 2 valid files

	// Check that both valid gates were loaded
	var foundGate1, foundGate2 bool
	for _, config := range configs {
		for _, gate := range config.Gates {
			if gate.ID == "gate1" {
				foundGate1 = true
			}
			if gate.ID == "gate2" {
				foundGate2 = true
			}
		}
	}
	assert.True(t, foundGate1)
	assert.True(t, foundGate2)
}

func TestLoader_GetDefaultGates(t *testing.T) {
	loader := NewLoader(".")

	// Get default gates
	config := loader.GetDefaultGates()
	assert.NotNil(t, config)
	assert.Equal(t, "1.0.0", config.SchemaVersion)
	assert.NotEmpty(t, config.Gates)

	// Check metadata
	assert.NotNil(t, config.Metadata)
	assert.Equal(t, "Default Gates", config.Metadata.Name)

	// Check default gate
	found := false
	for _, gate := range config.Gates {
		if gate.ID == "default_required_fields" {
			found = true
			assert.Equal(t, GateTypePreTask, gate.Type)
			assert.True(t, gate.Enabled)
			assert.Equal(t, 10, gate.Priority)
			break
		}
	}
	assert.True(t, found)
}

func TestLoader_ApplyDefaults(t *testing.T) {
	loader := NewLoader(".")

	yaml := `
schema_version: "1.0.0"
gates:
  - id: test_gate
    name: "Test Gate"
    type: pre_task
    rules:
      - id: rule1
        condition:
          type: field_validation
          field: task.id
          operator: exists
    action:
      on_fail: retry
`

	config, err := loader.LoadFromBytes([]byte(yaml))
	require.NoError(t, err)

	gate := config.Gates[0]

	// Check defaults were applied
	assert.True(t, gate.Enabled)                    // Default to enabled
	assert.Equal(t, 50, gate.Priority)              // Default priority
	assert.Equal(t, ActionAllow, gate.Action.OnPass) // Default on_pass
	assert.Equal(t, ActionContinue, gate.Action.OnWarn) // Default on_warn

	// Check retry config defaults
	assert.NotNil(t, gate.Action.RetryConfig)
	assert.Equal(t, 3, gate.Action.RetryConfig.MaxAttempts)
	assert.Equal(t, 5, gate.Action.RetryConfig.DelaySeconds)
	assert.False(t, gate.Action.RetryConfig.ExponentialBackoff)

	// Check rule defaults
	assert.Equal(t, SeverityError, gate.Rules[0].Severity)
	assert.True(t, gate.Rules[0].Condition.CaseSensitive)
}

func TestLoader_ValidationEdgeCases(t *testing.T) {
	loader := NewLoader(".")

	tests := []struct {
		name    string
		yaml    string
		wantErr bool
		errMsg  string
	}{
		{
			name: "duplicate gate IDs",
			yaml: `
schema_version: "1.0.0"
gates:
  - id: duplicate_id
    name: "Gate 1"
    type: pre_task
    rules:
      - id: rule1
        condition:
          type: field_validation
          field: task.id
          operator: exists
    action:
      on_pass: allow
      on_fail: block
  - id: duplicate_id
    name: "Gate 2"
    type: post_task
    rules:
      - id: rule1
        condition:
          type: field_validation
          field: result.status
          operator: exists
    action:
      on_pass: allow
      on_fail: block
`,
			wantErr: true,
			errMsg:  "duplicate gate ID",
		},
		{
			name: "invalid priority range",
			yaml: `
schema_version: "1.0.0"
gates:
  - id: test_gate
    name: "Test Gate"
    type: pre_task
    priority: 150
    rules:
      - id: rule1
        condition:
          type: field_validation
          field: task.id
          operator: exists
    action:
      on_pass: allow
      on_fail: block
`,
			wantErr: true,
			errMsg:  "priority must be between",
		},
		{
			name: "script without timeout",
			yaml: `
schema_version: "1.0.0"
gates:
  - id: script_gate
    name: "Script Gate"
    type: pre_task
    rules:
      - id: script_rule
        condition:
          type: script
          script: "exit 0"
          language: bash
    action:
      on_pass: allow
      on_fail: block
`,
			wantErr: false, // Should apply default timeout
		},
		{
			name: "resource limit without limit value",
			yaml: `
schema_version: "1.0.0"
gates:
  - id: resource_gate
    name: "Resource Gate"
    type: pre_task
    rules:
      - id: resource_rule
        condition:
          type: resource_limit
          resource: file_count
    action:
      on_pass: allow
      on_fail: block
`,
			wantErr: true,
			errMsg:  "positive limit",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loader.LoadFromBytes([]byte(tt.yaml))

			if tt.wantErr {
				assert.Error(t, err)
				if tt.errMsg != "" {
					assert.Contains(t, err.Error(), tt.errMsg)
				}
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestLoader_CompilePatterns(t *testing.T) {
	loader := NewLoader(".")

	yaml := `
schema_version: "1.0.0"
gates:
  - id: pattern_gate
    name: "Pattern Gate"
    type: pre_task
    trigger:
      patterns:
        - field: task.content
          regex: "^[a-z]+$"
    rules:
      - id: regex_rule
        condition:
          type: field_validation
          field: task.content
          operator: matches
          value: "\\d{3}-\\d{2}-\\d{4}"
    action:
      on_pass: allow
      on_fail: block
`

	config, err := loader.LoadFromBytes([]byte(yaml))
	require.NoError(t, err)

	// Check that regex was compiled
	condition := config.Gates[0].Rules[0].Condition
	assert.NotNil(t, condition.CompiledRegex)
}

func TestLoader_ComplexConditions(t *testing.T) {
	loader := NewLoader(".")

	yaml := `
schema_version: "1.0.0"
gates:
  - id: complex_gate
    name: "Complex Conditions Gate"
    type: phase_transition
    trigger:
      roles: [orchestrator, planner]
      bloom_levels: [4, 5, 6]
      task_types: [implementation, refactoring]
    rules:
      - id: nested_logic
        condition:
          type: or
          conditions:
            - type: and
              conditions:
                - type: field_validation
                  field: phase.status
                  operator: equals
                  value: completed
                - type: dependency_check
                  mode: all_completed
            - type: not
              conditions:
                - type: field_validation
                  field: phase.blocked
                  operator: equals
                  value: true
    action:
      on_pass: allow
      on_fail: rollback
      rollback_config:
        target: phase
        preserve_logs: true
    metrics:
      enabled: true
      track_duration: true
      track_failures: true
      tags:
        environment: test
        version: "1.0"
`

	config, err := loader.LoadFromBytes([]byte(yaml))
	require.NoError(t, err)

	gate := config.Gates[0]

	// Check trigger configuration
	assert.Equal(t, []string{"orchestrator", "planner"}, gate.Trigger.Roles)
	assert.Equal(t, []int{4, 5, 6}, gate.Trigger.BloomLevels)
	assert.Equal(t, []string{"implementation", "refactoring"}, gate.Trigger.TaskTypes)

	// Check action configuration
	assert.Equal(t, ActionRollback, gate.Action.OnFail)
	assert.NotNil(t, gate.Action.RollbackConfig)
	assert.Equal(t, "phase", gate.Action.RollbackConfig.Target)
	assert.True(t, gate.Action.RollbackConfig.PreserveLogs)

	// Check metrics configuration
	assert.True(t, gate.Metrics.Enabled)
	assert.True(t, gate.Metrics.TrackDuration)
	assert.True(t, gate.Metrics.TrackFailures)
	assert.Equal(t, "test", gate.Metrics.Tags["environment"])
	assert.Equal(t, "1.0", gate.Metrics.Tags["version"])
}