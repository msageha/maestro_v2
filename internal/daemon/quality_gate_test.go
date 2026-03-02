package daemon

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test fixtures for quality gate definitions
const testGateDefinitionYAML = `
schema_version: "1.0.0"
gates:
  - id: test_required_fields
    name: "Test Required Fields"
    type: pre_task
    enabled: true
    priority: 10
    rules:
      - id: check_purpose
        condition:
          type: field_validation
          field: task.purpose
          operator: exists
        severity: error
    action:
      on_pass: allow
      on_fail: block

  - id: test_bloom_level
    name: "Test Bloom Level"
    type: pre_task
    enabled: true
    priority: 15
    rules:
      - id: bloom_range
        condition:
          type: and
          conditions:
            - type: field_validation
              field: task.bloom_level
              operator: gte
              value: 1
            - type: field_validation
              field: task.bloom_level
              operator: lte
              value: 6
        severity: error
    action:
      on_pass: allow
      on_fail: block

  - id: test_dangerous_commands
    name: "Test Dangerous Commands"
    type: pre_task
    enabled: true
    priority: 5
    rules:
      - id: detect_rm_rf
        condition:
          type: field_validation
          field: task.content
          operator: contains
          value: "rm -rf /"
        severity: critical
    action:
      on_pass: allow
      on_fail: block
`

func setupTestQualityGate(t *testing.T) (*QualityGateDaemon, string) {
	tmpDir := t.TempDir()
	maestroDir := filepath.Join(tmpDir, ".maestro")
	require.NoError(t, os.MkdirAll(maestroDir, 0755))

	// Create quality_gates directory and write test definition
	gatesDir := filepath.Join(maestroDir, "quality_gates")
	require.NoError(t, os.MkdirAll(gatesDir, 0755))

	gateFile := filepath.Join(gatesDir, "test_gates.yaml")
	require.NoError(t, os.WriteFile(gateFile, []byte(testGateDefinitionYAML), 0644))

	cfg := model.Config{}
	lockMap := lock.NewMutexMap()
	logger := log.New(os.Stdout, "[TEST] ", log.LstdFlags)

	qg := NewQualityGateDaemon(maestroDir, cfg, lockMap, logger, LogLevelDebug)

	// Initialize the gate engine
	err := qg.loadGateDefinitions()
	require.NoError(t, err)

	return qg, maestroDir
}

func TestQualityGateDaemon_Start_Stop(t *testing.T) {
	qg, _ := setupTestQualityGate(t)

	// Start the daemon
	err := qg.Start()
	assert.NoError(t, err)

	// Verify it's running
	time.Sleep(10 * time.Millisecond)

	// Stop the daemon
	err = qg.Stop()
	assert.NoError(t, err)
}

func TestQualityGateDaemon_EmitEvent(t *testing.T) {
	qg, _ := setupTestQualityGate(t)

	err := qg.Start()
	require.NoError(t, err)
	defer qg.Stop()

	// Emit a task start event
	event := TaskStartEvent{
		TaskID:    "task_123",
		CommandID: "cmd_456",
		AgentID:   "worker1",
		StartedAt: time.Now(),
	}

	qg.EmitEvent(event)

	// Give it time to process
	time.Sleep(50 * time.Millisecond)

	// Check metrics
	evalCount, successCount, failureCount, avgTimeMs := qg.GetMetrics().GetStats()
	assert.Greater(t, evalCount, int64(0), "Should have at least one evaluation")
	assert.LessOrEqual(t, avgTimeMs, int64(100), "Average time should be under 100ms")

	t.Logf("Metrics: evals=%d success=%d failure=%d avg_ms=%d",
		evalCount, successCount, failureCount, avgTimeMs)
}

func TestEvaluateGate_FieldValidation(t *testing.T) {
	qg, _ := setupTestQualityGate(t)

	testCases := []struct {
		name        string
		gateType    string
		context     map[string]interface{}
		expectError bool
		expectPass  bool
	}{
		{
			name:     "task with purpose - should pass",
			gateType: "pre_task",
			context: map[string]interface{}{
				"task": map[string]interface{}{
					"purpose": "Test task",
					"content": "echo hello",
				},
			},
			expectError: false,
			expectPass:  true,
		},
		{
			name:     "task without purpose - should fail",
			gateType: "pre_task",
			context: map[string]interface{}{
				"task": map[string]interface{}{
					"content": "echo hello",
				},
			},
			expectError: false,
			expectPass:  false,
		},
		{
			name:     "dangerous command - should fail",
			gateType: "pre_task",
			context: map[string]interface{}{
				"task": map[string]interface{}{
					"purpose": "Dangerous task",
					"content": "rm -rf /",
				},
			},
			expectError: false,
			expectPass:  false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := qg.evaluateGateWithResult(tc.gateType, tc.context)

			if tc.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tc.expectPass, result.Passed)
			}
		})
	}
}

func TestEvaluateGate_LogicalOperators(t *testing.T) {
	qg, _ := setupTestQualityGate(t)

	testCases := []struct {
		name        string
		context     map[string]interface{}
		expectPass  bool
	}{
		{
			name: "bloom level in range - should pass",
			context: map[string]interface{}{
				"task": map[string]interface{}{
					"purpose":     "Test",
					"bloom_level": 3,
				},
			},
			expectPass: true,
		},
		{
			name: "bloom level too low - should fail",
			context: map[string]interface{}{
				"task": map[string]interface{}{
					"purpose":     "Test",
					"bloom_level": 0,
				},
			},
			expectPass: false,
		},
		{
			name: "bloom level too high - should fail",
			context: map[string]interface{}{
				"task": map[string]interface{}{
					"purpose":     "Test",
					"bloom_level": 7,
				},
			},
			expectPass: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := qg.evaluateGateWithResult("pre_task", tc.context)
			require.NoError(t, err)
			assert.Equal(t, tc.expectPass, result.Passed, "Result: %+v", result)
		})
	}
}

func TestEvaluateGate_Performance(t *testing.T) {
	qg, _ := setupTestQualityGate(t)

	// Create a context with many fields for a realistic test
	context := map[string]interface{}{
		"task": map[string]interface{}{
			"id":                 "task_perf_test",
			"purpose":            "Performance test task",
			"content":            "Complex task content with many lines\n" + generateLongContent(100),
			"bloom_level":        4,
			"acceptance_criteria": "Must complete quickly",
			"constraints":        []string{"time_limit", "memory_limit"},
		},
		"command": map[string]interface{}{
			"id":      "cmd_perf_test",
			"purpose": "Test command",
		},
	}

	// Run multiple evaluations and measure time
	iterations := 100
	var totalDuration time.Duration

	for i := 0; i < iterations; i++ {
		start := time.Now()
		_, err := qg.evaluateGateWithResult("pre_task", context)
		duration := time.Since(start)

		require.NoError(t, err)
		assert.Less(t, duration, 100*time.Millisecond,
			"Evaluation %d took %v, exceeds 100ms limit", i, duration)

		totalDuration += duration
	}

	avgDuration := totalDuration / time.Duration(iterations)
	t.Logf("Average evaluation time over %d iterations: %v", iterations, avgDuration)
	assert.Less(t, avgDuration, 50*time.Millisecond,
		"Average evaluation time should be well under 100ms")
}

func TestEvaluateGate_Caching(t *testing.T) {
	qg, _ := setupTestQualityGate(t)

	context := map[string]interface{}{
		"task": map[string]interface{}{
			"purpose": "Cache test",
			"content": "echo test",
		},
	}

	// First evaluation - should not be cached
	result1, err := qg.evaluateGateWithResult("pre_task", context)
	require.NoError(t, err)
	assert.False(t, result1.CacheHit)

	// Second evaluation with same context - should be cached
	result2, err := qg.evaluateGateWithResult("pre_task", context)
	require.NoError(t, err)
	assert.True(t, result2.CacheHit)
	assert.Equal(t, result1.Passed, result2.Passed)

	// Evaluation with different context - should not be cached
	context["task"].(map[string]interface{})["content"] = "echo different"
	result3, err := qg.evaluateGateWithResult("pre_task", context)
	require.NoError(t, err)
	assert.False(t, result3.CacheHit)
}

func TestEvaluateGate_Priority(t *testing.T) {
	qg, _ := setupTestQualityGate(t)

	// Context that would fail the dangerous command check (priority 5)
	// but would pass the required fields check (priority 10)
	context := map[string]interface{}{
		"task": map[string]interface{}{
			"purpose": "Dangerous task",
			"content": "rm -rf /",
		},
	}

	result, err := qg.evaluateGateWithResult("pre_task", context)
	require.NoError(t, err)
	assert.False(t, result.Passed, "Should fail due to dangerous command")

	// Verify that the dangerous command gate ran first (lower priority number)
	assert.Contains(t, result.FailedGates, "test_dangerous_commands")
}

func TestEvaluateGate_Timeout(t *testing.T) {
	qg, _ := setupTestQualityGate(t)

	// Override the evaluation timeout for this test
	origTimeout := qg.evaluationTimeout
	qg.evaluationTimeout = 10 * time.Millisecond
	defer func() { qg.evaluationTimeout = origTimeout }()

	// Add a slow script gate definition directly to the YAML file
	gatesDir := filepath.Join(filepath.Dir(qg.maestroDir), ".maestro", "quality_gates")
	slowGateYAML := `
schema_version: "1.0.0"
gates:
  - id: slow_gate
    name: "Slow Gate"
    type: pre_task
    enabled: true
    priority: 1
    rules:
      - id: slow_rule
        condition:
          type: script
          language: bash
          script: "sleep 1"
          timeout_seconds: 5
        severity: error
    action:
      on_pass: allow
      on_fail: block
`
	slowGateFile := filepath.Join(gatesDir, "slow_gate.yaml")
	require.NoError(t, os.WriteFile(slowGateFile, []byte(slowGateYAML), 0644))

	// Reload gate definitions
	require.NoError(t, qg.loadGateDefinitions())

	context := map[string]interface{}{
		"task": map[string]interface{}{
			"purpose": "Timeout test",
		},
	}

	start := time.Now()
	result, err := qg.evaluateGateWithResult("pre_task", context)
	duration := time.Since(start)

	require.NoError(t, err)
	assert.True(t, result.TimedOut)
	assert.Less(t, duration, 50*time.Millisecond, "Should timeout quickly")
}

func TestQualityGateMetrics(t *testing.T) {
	metrics := &QualityGateMetrics{}

	// Record some evaluations
	metrics.RecordEvaluation(true, 50)
	metrics.RecordEvaluation(false, 75)
	metrics.RecordEvaluation(true, 25)

	evalCount, successCount, failureCount, avgTimeMs := metrics.GetStats()

	assert.Equal(t, int64(3), evalCount)
	assert.Equal(t, int64(2), successCount)
	assert.Equal(t, int64(1), failureCount)
	assert.Equal(t, int64(50), avgTimeMs) // (50+75+25)/3 = 50
}

// Helper function to generate long content for performance tests
func generateLongContent(lines int) string {
	content := ""
	for i := 0; i < lines; i++ {
		content += fmt.Sprintf("Line %d: Some content for testing performance\n", i)
	}
	return content
}

// Benchmark tests
func BenchmarkEvaluateGate_Simple(b *testing.B) {
	tmpDir := b.TempDir()
	maestroDir := filepath.Join(tmpDir, ".maestro")
	require.NoError(b, os.MkdirAll(maestroDir, 0755))

	// Create quality_gates directory and write test definition
	gatesDir := filepath.Join(maestroDir, "quality_gates")
	require.NoError(b, os.MkdirAll(gatesDir, 0755))

	gateFile := filepath.Join(gatesDir, "test_gates.yaml")
	require.NoError(b, os.WriteFile(gateFile, []byte(testGateDefinitionYAML), 0644))

	cfg := model.Config{}
	lockMap := lock.NewMutexMap()
	logger := log.New(os.Stdout, "[TEST] ", log.LstdFlags)

	qg := NewQualityGateDaemon(maestroDir, cfg, lockMap, logger, LogLevelError)

	// Initialize the gate engine
	err := qg.loadGateDefinitions()
	require.NoError(b, err)

	context := map[string]interface{}{
		"task": map[string]interface{}{
			"purpose": "Benchmark task",
			"content": "echo benchmark",
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = qg.evaluateGateWithResult("pre_task", context)
	}
}

func BenchmarkEvaluateGate_Complex(b *testing.B) {
	tmpDir := b.TempDir()
	maestroDir := filepath.Join(tmpDir, ".maestro")
	require.NoError(b, os.MkdirAll(maestroDir, 0755))

	// Create quality_gates directory and write test definition
	gatesDir := filepath.Join(maestroDir, "quality_gates")
	require.NoError(b, os.MkdirAll(gatesDir, 0755))

	gateFile := filepath.Join(gatesDir, "test_gates.yaml")
	require.NoError(b, os.WriteFile(gateFile, []byte(testGateDefinitionYAML), 0644))

	cfg := model.Config{}
	lockMap := lock.NewMutexMap()
	logger := log.New(os.Stdout, "[TEST] ", log.LstdFlags)

	qg := NewQualityGateDaemon(maestroDir, cfg, lockMap, logger, LogLevelError)

	// Initialize the gate engine
	err := qg.loadGateDefinitions()
	require.NoError(b, err)

	context := map[string]interface{}{
		"task": map[string]interface{}{
			"purpose":     "Complex benchmark task",
			"content":     generateLongContent(50),
			"bloom_level": 4,
			"constraints": []string{"time", "memory", "cpu"},
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = qg.evaluateGateWithResult("pre_task", context)
	}
}