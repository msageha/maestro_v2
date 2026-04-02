package quality

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeGateFile creates a gate YAML file under configDir/quality_gates/.
func writeGateFile(t *testing.T, configDir, name, content string) {
	t.Helper()
	gatesDir := filepath.Join(configDir, "quality_gates")
	require.NoError(t, os.MkdirAll(gatesDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(gatesDir, name), []byte(content), 0644))
}

func TestLoader_ApplyDefaults(t *testing.T) {
	tmpDir := t.TempDir()
	loader := NewLoader(tmpDir)

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
      on_fail: block
`
	writeGateFile(t, tmpDir, "test.yaml", yaml)

	config, err := loader.LoadConfiguration()
	require.NoError(t, err)
	require.Len(t, config.Gates, 1)

	gate := config.Gates[0]

	// Check defaults were applied
	assert.NotNil(t, gate.Enabled)                   // Default to enabled
	assert.True(t, *gate.Enabled)                    // Default to enabled
	assert.Equal(t, 50, gate.Priority)              // Default priority
	assert.Equal(t, ActionAllow, gate.Action.OnPass) // Default on_pass
	assert.Equal(t, ActionContinue, gate.Action.OnWarn) // Default on_warn

	// Check rule defaults
	assert.Equal(t, SeverityError, gate.Rules[0].Severity)
	assert.True(t, gate.Rules[0].Condition.CaseSensitive)
}

func TestLoader_ValidationEdgeCases(t *testing.T) {
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
			name: "resource limit is unknown condition type",
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
          field: file_count
    action:
      on_pass: allow
      on_fail: block
`,
			wantErr: true,
			errMsg:  "unknown condition type",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			loader := NewLoader(tmpDir)
			writeGateFile(t, tmpDir, "test.yaml", tt.yaml)

			_, err := loader.LoadConfiguration()

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

func TestLoader_ComplexConditions(t *testing.T) {
	tmpDir := t.TempDir()
	loader := NewLoader(tmpDir)

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
                - type: field_validation
                  field: task.dependencies_met
                  operator: equals
                  value: true
            - type: not
              conditions:
                - type: field_validation
                  field: phase.blocked
                  operator: equals
                  value: true
    action:
      on_pass: allow
      on_fail: block
`
	writeGateFile(t, tmpDir, "test.yaml", yaml)

	config, err := loader.LoadConfiguration()
	require.NoError(t, err)
	require.Len(t, config.Gates, 1)

	gate := config.Gates[0]

	// Check trigger configuration
	assert.Equal(t, []string{"orchestrator", "planner"}, gate.Trigger.Roles)
	assert.Equal(t, []int{4, 5, 6}, gate.Trigger.BloomLevels)
	assert.Equal(t, []string{"implementation", "refactoring"}, gate.Trigger.TaskTypes)

	// Check action configuration
	assert.Equal(t, ActionBlock, gate.Action.OnFail)
}

func TestLoader_EnabledFalsePreserved(t *testing.T) {
	tmpDir := t.TempDir()
	loader := NewLoader(tmpDir)

	yaml := `
schema_version: "1.0.0"
gates:
  - id: disabled_gate
    name: "Disabled Gate"
    enabled: false
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
  - id: enabled_gate
    name: "Enabled Gate"
    enabled: true
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
  - id: unset_gate
    name: "Unset Gate"
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
	writeGateFile(t, tmpDir, "test.yaml", yaml)

	config, err := loader.LoadConfiguration()
	require.NoError(t, err)
	require.Len(t, config.Gates, 3)

	// enabled: false should be preserved as false
	disabledGate := config.Gates[0]
	assert.Equal(t, "disabled_gate", disabledGate.ID)
	require.NotNil(t, disabledGate.Enabled)
	assert.False(t, *disabledGate.Enabled, "enabled: false must be preserved, not overwritten to true")

	// enabled: true should remain true
	enabledGate := config.Gates[1]
	assert.Equal(t, "enabled_gate", enabledGate.ID)
	require.NotNil(t, enabledGate.Enabled)
	assert.True(t, *enabledGate.Enabled)

	// unset enabled should default to true
	unsetGate := config.Gates[2]
	assert.Equal(t, "unset_gate", unsetGate.ID)
	require.NotNil(t, unsetGate.Enabled)
	assert.True(t, *unsetGate.Enabled, "unset enabled should default to true")
}

func TestValidateFilePermissions(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "perms_test")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	t.Run("safe permissions", func(t *testing.T) {
		safeFile := filepath.Join(tmpDir, "safe.yaml")
		err := os.WriteFile(safeFile, []byte("test"), 0600)
		require.NoError(t, err)

		err = validateFilePermissions(safeFile)
		assert.NoError(t, err)
	})

	t.Run("owner read-write only", func(t *testing.T) {
		file := filepath.Join(tmpDir, "owner_rw.yaml")
		err := os.WriteFile(file, []byte("test"), 0644)
		require.NoError(t, err)

		err = validateFilePermissions(file)
		assert.NoError(t, err, "0644 should be safe (no group/other write)")
	})

	t.Run("group writable rejected", func(t *testing.T) {
		file := filepath.Join(tmpDir, "group_w.yaml")
		err := os.WriteFile(file, []byte("test"), 0600)
		require.NoError(t, err)
		// Explicitly set permissions to bypass umask
		require.NoError(t, os.Chmod(file, 0664))

		err = validateFilePermissions(file)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "must not be writable by group or others")
	})

	t.Run("world writable rejected", func(t *testing.T) {
		file := filepath.Join(tmpDir, "world_w.yaml")
		err := os.WriteFile(file, []byte("test"), 0600)
		require.NoError(t, err)
		// Explicitly set permissions to bypass umask
		require.NoError(t, os.Chmod(file, 0666))

		err = validateFilePermissions(file)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "must not be writable by group or others")
	})

	t.Run("nonexistent file", func(t *testing.T) {
		err := validateFilePermissions(filepath.Join(tmpDir, "nonexistent.yaml"))
		assert.Error(t, err)
	})
}
