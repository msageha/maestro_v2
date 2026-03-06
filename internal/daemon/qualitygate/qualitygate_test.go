package qualitygate

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
    trigger:
      bloom_levels: [0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10]
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
          operator: not_contains
          value: "rm -rf /"
        severity: critical
    action:
      on_pass: allow
      on_fail: block
`

func setupTestQualityGate(t *testing.T) (*Daemon, string) {
	tmpDir := t.TempDir()
	maestroDir := filepath.Join(tmpDir, ".maestro")
	require.NoError(t, os.MkdirAll(maestroDir, 0755))

	gatesDir := filepath.Join(maestroDir, "quality_gates")
	require.NoError(t, os.MkdirAll(gatesDir, 0755))

	gateFile := filepath.Join(gatesDir, "test_gates.yaml")
	require.NoError(t, os.WriteFile(gateFile, []byte(testGateDefinitionYAML), 0644))

	cfg := model.Config{}
	lockMap := lock.NewMutexMap()
	logger := log.New(os.Stdout, "[TEST] ", log.LstdFlags)

	qg := NewDaemon(maestroDir, cfg, lockMap, logger, core.LogLevelDebug, context.Background())

	err := qg.loadGateDefinitions()
	require.NoError(t, err)

	return qg, maestroDir
}

func TestDaemon_Start_Stop(t *testing.T) {
	qg, _ := setupTestQualityGate(t)

	err := qg.Start()
	assert.NoError(t, err)

	time.Sleep(10 * time.Millisecond)

	err = qg.Stop()
	assert.NoError(t, err)
}

func TestDaemon_EmitEvent(t *testing.T) {
	qg, _ := setupTestQualityGate(t)

	err := qg.Start()
	require.NoError(t, err)
	defer qg.Stop()

	event := TaskStartEvent{
		TaskID:    "task_123",
		CommandID: "cmd_456",
		AgentID:   "worker1",
		StartedAt: time.Now(),
	}

	qg.EmitEvent(event)

	time.Sleep(50 * time.Millisecond)

	evalCount, successCount, failureCount, avgTimeMs := qg.GetMetrics().GetStats()
	assert.Greater(t, evalCount, int64(0))
	assert.LessOrEqual(t, avgTimeMs, int64(100))

	t.Logf("Metrics: evals=%d success=%d failure=%d avg_ms=%d",
		evalCount, successCount, failureCount, avgTimeMs)
}

func TestEvaluateGateWithResult_FieldValidation(t *testing.T) {
	qg, _ := setupTestQualityGate(t)

	testCases := []struct {
		name       string
		gateType   string
		context    map[string]interface{}
		expectPass bool
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
			expectPass: true,
		},
		{
			name:     "task without purpose - should fail",
			gateType: "pre_task",
			context: map[string]interface{}{
				"task": map[string]interface{}{
					"content": "echo hello",
				},
			},
			expectPass: false,
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
			expectPass: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := qg.EvaluateGateWithResult(tc.gateType, tc.context)
			assert.NoError(t, err)
			assert.Equal(t, tc.expectPass, result.Passed)
		})
	}
}

func TestEvaluateGateWithResult_LogicalOperators(t *testing.T) {
	qg, _ := setupTestQualityGate(t)

	testCases := []struct {
		name       string
		context    map[string]interface{}
		expectPass bool
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
			result, err := qg.EvaluateGateWithResult("pre_task", tc.context)
			require.NoError(t, err)
			assert.Equal(t, tc.expectPass, result.Passed, "Result: %+v", result)
		})
	}
}

func TestMetrics(t *testing.T) {
	metrics := &Metrics{}

	metrics.RecordEvaluation(true, 50)
	metrics.RecordEvaluation(false, 75)
	metrics.RecordEvaluation(true, 25)

	evalCount, successCount, failureCount, avgTimeMs := metrics.GetStats()

	assert.Equal(t, int64(3), evalCount)
	assert.Equal(t, int64(2), successCount)
	assert.Equal(t, int64(1), failureCount)
	assert.Equal(t, int64(50), avgTimeMs)
}

func TestDaemon_ConcurrentStopEmit(t *testing.T) {
	qg, _ := setupTestQualityGate(t)

	err := qg.Start()
	require.NoError(t, err)

	const numEmitters = 10
	const eventsPerEmitter = 50

	var wg sync.WaitGroup

	start := make(chan struct{})
	for i := 0; i < numEmitters; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			<-start
			for j := 0; j < eventsPerEmitter; j++ {
				qg.EmitEvent(TaskStartEvent{
					TaskID:    fmt.Sprintf("task_%d_%d", id, j),
					CommandID: "cmd_race_test",
					AgentID:   fmt.Sprintf("worker%d", id),
					StartedAt: time.Now(),
				})
			}
		}(i)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		time.Sleep(time.Millisecond)
		_ = qg.Stop()
	}()

	close(start)

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for concurrent Stop+EmitEvent to complete")
	}
}

func TestDaemon_StopIdempotent(t *testing.T) {
	qg, _ := setupTestQualityGate(t)

	err := qg.Start()
	require.NoError(t, err)

	err = qg.Stop()
	assert.NoError(t, err)

	err = qg.Stop()
	assert.NoError(t, err)

	qg.EmitEvent(TaskStartEvent{
		TaskID:    "task_after_stop",
		CommandID: "cmd_after_stop",
		AgentID:   "worker1",
		StartedAt: time.Now(),
	})
}

func TestDaemon_EmitAfterStop(t *testing.T) {
	qg, _ := setupTestQualityGate(t)

	err := qg.Start()
	require.NoError(t, err)

	err = qg.Stop()
	require.NoError(t, err)

	for i := 0; i < 200; i++ {
		qg.EmitEvent(TaskCompleteEvent{
			TaskID:      fmt.Sprintf("task_%d", i),
			CommandID:   "cmd_post_stop",
			AgentID:     "worker1",
			Status:      "completed",
			CompletedAt: time.Now(),
		})
	}
}

func generateLongContent(lines int) string {
	content := ""
	for i := 0; i < lines; i++ {
		content += fmt.Sprintf("Line %d: Some content for testing performance\n", i)
	}
	return content
}
