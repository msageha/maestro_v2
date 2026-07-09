package quality

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/msageha/maestro_v2/internal/ptr"
)

func TestEngine_LoadConfiguration(t *testing.T) {
	engine := NewEngine()

	config := &GateConfiguration{
		SchemaVersion: "1.0.0",
		Gates: []GateDefinition{
			{
				ID:       "test_gate",
				Name:     "Test Gate",
				Enabled:  ptr.Bool(true),
				Type:     GateTypePreTask,
				Priority: 10,
				Rules: []RuleDefinition{
					{
						ID: "test_rule",
						Condition: RuleCondition{
							Type:     ConditionFieldValidation,
							Field:    "task.purpose",
							Operator: OpExists,
						},
						Severity: SeverityError,
					},
				},
				Action: ActionDefinition{
					OnPass: ActionAllow,
					OnFail: ActionBlock,
				},
			},
		},
	}

	err := engine.LoadConfiguration(config)
	assert.NoError(t, err)
	assert.Len(t, engine.gates[GateTypePreTask], 1)
}

func TestEngine_Evaluate_SimpleFieldValidation(t *testing.T) {
	engine := NewEngine()

	// Load test configuration
	config := &GateConfiguration{
		SchemaVersion: "1.0.0",
		Gates: []GateDefinition{
			{
				ID:       "field_test",
				Name:     "Field Test",
				Enabled:  ptr.Bool(true),
				Type:     GateTypePreTask,
				Priority: 10,
				Rules: []RuleDefinition{
					{
						ID: "check_purpose",
						Condition: RuleCondition{
							Type:     ConditionFieldValidation,
							Field:    "task.purpose",
							Operator: OpExists,
						},
						Severity: SeverityError,
					},
				},
				Action: ActionDefinition{
					OnPass: ActionAllow,
					OnFail: ActionBlock,
				},
			},
		},
	}

	require.NoError(t, engine.LoadConfiguration(config))

	testCases := []struct {
		name       string
		context    map[string]interface{}
		expectPass bool
	}{
		{
			name: "with purpose - should pass",
			context: map[string]interface{}{
				"task": map[string]interface{}{
					"purpose": "test",
				},
			},
			expectPass: true,
		},
		{
			name: "without purpose - should fail",
			context: map[string]interface{}{
				"task": map[string]interface{}{},
			},
			expectPass: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			result, err := engine.Evaluate(ctx, GateTypePreTask, tc.context)
			require.NoError(t, err)
			assert.Equal(t, tc.expectPass, result.Passed)
		})
	}
}

func TestEngine_Evaluate_LogicalOperators(t *testing.T) {
	engine := NewEngine()

	config := &GateConfiguration{
		SchemaVersion: "1.0.0",
		Gates: []GateDefinition{
			{
				ID:       "logical_test",
				Name:     "Logical Test",
				Enabled:  ptr.Bool(true),
				Type:     GateTypePreTask,
				Priority: 10,
				Rules: []RuleDefinition{
					{
						ID: "and_condition",
						Condition: RuleCondition{
							Type: ConditionAnd,
							Conditions: []RuleCondition{
								{
									Type:     ConditionFieldValidation,
									Field:    "task.bloom_level",
									Operator: OpGTE,
									Value:    1,
								},
								{
									Type:     ConditionFieldValidation,
									Field:    "task.bloom_level",
									Operator: OpLTE,
									Value:    6,
								},
							},
						},
						Severity: SeverityError,
					},
				},
				Action: ActionDefinition{
					OnPass: ActionAllow,
					OnFail: ActionBlock,
				},
			},
		},
	}

	require.NoError(t, engine.LoadConfiguration(config))

	testCases := []struct {
		name       string
		bloomLevel int
		expectPass bool
	}{
		{"valid bloom level 3", 3, true},
		{"too low bloom level 0", 0, false},
		{"too high bloom level 7", 7, false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			evalCtx := map[string]interface{}{
				"task": map[string]interface{}{
					"bloom_level": tc.bloomLevel,
				},
			}
			result, err := engine.Evaluate(ctx, GateTypePreTask, evalCtx)
			require.NoError(t, err)
			assert.Equal(t, tc.expectPass, result.Passed)
		})
	}
}

func TestEngine_Evaluate_Caching(t *testing.T) {
	engine := NewEngine()

	config := &GateConfiguration{
		SchemaVersion: "1.0.0",
		Gates: []GateDefinition{
			{
				ID:       "cache_test",
				Name:     "Cache Test",
				Enabled:  ptr.Bool(true),
				Type:     GateTypePreTask,
				Priority: 10,
				Rules: []RuleDefinition{
					{
						ID: "simple_check",
						Condition: RuleCondition{
							Type:     ConditionFieldValidation,
							Field:    "task.id",
							Operator: OpExists,
						},
						Severity: SeverityInfo,
					},
				},
				Action: ActionDefinition{
					OnPass: ActionAllow,
					OnFail: ActionWarn,
				},
			},
		},
	}

	require.NoError(t, engine.LoadConfiguration(config))

	ctx := context.Background()
	evalCtx := map[string]interface{}{
		"task": map[string]interface{}{
			"id": "test_123",
		},
	}

	// First evaluation - should not be cached
	result1, err := engine.Evaluate(ctx, GateTypePreTask, evalCtx)
	require.NoError(t, err)
	assert.False(t, result1.CacheHit)

	// Second evaluation - should be cached
	result2, err := engine.Evaluate(ctx, GateTypePreTask, evalCtx)
	require.NoError(t, err)
	assert.True(t, result2.CacheHit)
	assert.Equal(t, result1.Passed, result2.Passed)
}

func TestEngine_Evaluate_Performance(t *testing.T) {
	engine := NewEngine()

	// Load a complex configuration with multiple gates
	config := &GateConfiguration{
		SchemaVersion: "1.0.0",
		Gates: []GateDefinition{
			{
				ID:       "perf_gate_1",
				Name:     "Performance Gate 1",
				Enabled:  ptr.Bool(true),
				Type:     GateTypePreTask,
				Priority: 10,
				Rules: []RuleDefinition{
					{
						ID: "rule1",
						Condition: RuleCondition{
							Type:     ConditionFieldValidation,
							Field:    "task.purpose",
							Operator: OpExists,
						},
						Severity: SeverityError,
					},
					{
						ID: "rule2",
						Condition: RuleCondition{
							Type:     ConditionFieldValidation,
							Field:    "task.content",
							Operator: OpNotContains,
							Value:    "dangerous",
						},
						Severity: SeverityWarning,
					},
				},
				Action: ActionDefinition{
					OnPass: ActionAllow,
					OnFail: ActionBlock,
				},
			},
			{
				ID:       "perf_gate_2",
				Name:     "Performance Gate 2",
				Enabled:  ptr.Bool(true),
				Type:     GateTypePreTask,
				Priority: 20,
				Rules: []RuleDefinition{
					{
						ID: "complex_and",
						Condition: RuleCondition{
							Type: ConditionAnd,
							Conditions: []RuleCondition{
								{
									Type:     ConditionFieldValidation,
									Field:    "task.bloom_level",
									Operator: OpGTE,
									Value:    1,
								},
								{
									Type:     ConditionFieldValidation,
									Field:    "task.bloom_level",
									Operator: OpLTE,
									Value:    6,
								},
							},
						},
						Severity: SeverityError,
					},
				},
				Action: ActionDefinition{
					OnPass: ActionAllow,
					OnFail: ActionWarn,
				},
			},
		},
	}

	require.NoError(t, engine.LoadConfiguration(config))

	ctx := context.Background()
	evalCtx := map[string]interface{}{
		"task": map[string]interface{}{
			"id":          "perf_test",
			"purpose":     "Performance testing",
			"content":     "Complex content with many lines for testing performance",
			"bloom_level": 3,
		},
	}

	// Run multiple evaluations and check timing
	iterations := 100
	var totalDuration time.Duration

	for i := 0; i < iterations; i++ {
		start := time.Now()
		result, err := engine.Evaluate(ctx, GateTypePreTask, evalCtx)
		duration := time.Since(start)

		require.NoError(t, err)
		assert.True(t, result.Passed)
		assert.Less(t, duration, 100*time.Millisecond, "Single evaluation should be under 100ms")

		totalDuration += duration
	}

	avgDuration := totalDuration / time.Duration(iterations)
	t.Logf("Average evaluation time: %v", avgDuration)
	assert.Less(t, avgDuration, 50*time.Millisecond, "Average should be well under 100ms")
}

func TestEngine_Evaluate_Timeout(t *testing.T) {
	engine := NewEngine()

	// Create a gate with a slow script
	config := &GateConfiguration{
		SchemaVersion: "1.0.0",
		Gates: []GateDefinition{
			{
				ID:       "timeout_test",
				Name:     "Timeout Test",
				Enabled:  ptr.Bool(true),
				Type:     GateTypePreTask,
				Priority: 1,
				Rules: []RuleDefinition{
					{
						ID: "slow_script",
						Condition: RuleCondition{
							Type:           ConditionScript,
							Language:       "bash",
							Script:         "sleep 5",
							TimeoutSeconds: 10,
						},
						Severity: SeverityError,
					},
				},
				Action: ActionDefinition{
					OnPass: ActionAllow,
					OnFail: ActionBlock,
				},
			},
		},
	}

	require.NoError(t, engine.LoadConfiguration(config))

	// Create a context with a short timeout
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	evalCtx := map[string]interface{}{
		"task": map[string]interface{}{
			"id": "timeout_test",
		},
	}

	start := time.Now()
	result, err := engine.Evaluate(ctx, GateTypePreTask, evalCtx)
	duration := time.Since(start)

	require.NoError(t, err)
	assert.True(t, result.TimedOut)
	assert.False(t, result.Passed, "timeout should fail-closed (passed=false)")
	assert.Equal(t, ActionBlock, result.Action, "timeout should block")
	assert.Less(t, duration, 150*time.Millisecond, "Should timeout quickly")
}

func TestEngine_CompileRegex(t *testing.T) {
	engine := NewEngine()

	config := &GateConfiguration{
		SchemaVersion: "1.0.0",
		Gates: []GateDefinition{
			{
				ID:       "regex_test",
				Name:     "Regex Test",
				Enabled:  ptr.Bool(true),
				Type:     GateTypePreTask,
				Priority: 10,
				Rules: []RuleDefinition{
					{
						ID: "match_pattern",
						Condition: RuleCondition{
							Type:     ConditionFieldValidation,
							Field:    "task.content",
							Operator: OpNotMatches,
							Value:    "rm\\s+-rf\\s+/",
						},
						Severity: SeverityCritical,
					},
				},
				Action: ActionDefinition{
					OnPass: ActionAllow,
					OnFail: ActionBlock,
				},
			},
		},
	}

	require.NoError(t, engine.LoadConfiguration(config))

	testCases := []struct {
		name       string
		content    string
		expectPass bool
	}{
		{
			name:       "dangerous command",
			content:    "rm -rf /",
			expectPass: false,
		},
		{
			name:       "safe command",
			content:    "echo hello",
			expectPass: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			evalCtx := map[string]interface{}{
				"task": map[string]interface{}{
					"content": tc.content,
				},
			}
			result, err := engine.Evaluate(ctx, GateTypePreTask, evalCtx)
			require.NoError(t, err)
			assert.Equal(t, tc.expectPass, result.Passed)
		})
	}
}

func TestEngine_TriggerFilters(t *testing.T) {
	engine := NewEngine()

	config := &GateConfiguration{
		SchemaVersion: "1.0.0",
		Gates: []GateDefinition{
			{
				ID:       "role_filtered",
				Name:     "Role Filtered Gate",
				Enabled:  ptr.Bool(true),
				Type:     GateTypePreTask,
				Priority: 10,
				Trigger: TriggerDefinition{
					Roles: []string{"worker"},
				},
				Rules: []RuleDefinition{
					{
						ID: "worker_only",
						Condition: RuleCondition{
							Type:     ConditionFieldValidation,
							Field:    "task.id",
							Operator: OpExists,
						},
						Severity: SeverityInfo,
					},
				},
				Action: ActionDefinition{
					OnPass: ActionAllow,
					OnFail: ActionWarn,
				},
			},
			{
				ID:       "bloom_filtered",
				Name:     "Bloom Level Filtered Gate",
				Enabled:  ptr.Bool(true),
				Type:     GateTypePreTask,
				Priority: 20,
				Trigger: TriggerDefinition{
					BloomLevels: []int{4, 5, 6},
				},
				Rules: []RuleDefinition{
					{
						ID: "complex_only",
						Condition: RuleCondition{
							Type:     ConditionFieldValidation,
							Field:    "task.complexity",
							Operator: OpExists,
						},
						Severity: SeverityWarning,
					},
				},
				Action: ActionDefinition{
					OnPass: ActionAllow,
					OnFail: ActionWarn,
				},
			},
		},
	}

	require.NoError(t, engine.LoadConfiguration(config))

	// Test role filtering
	t.Run("role filtering", func(t *testing.T) {
		ctx := context.Background()

		// Should trigger for worker role
		workerCtx := map[string]interface{}{
			"agent": map[string]interface{}{
				"role": "worker",
			},
			"task": map[string]interface{}{
				"id": "test",
			},
		}
		result, err := engine.Evaluate(ctx, GateTypePreTask, workerCtx)
		require.NoError(t, err)
		assert.True(t, result.Passed)

		// Should not trigger for planner role
		plannerCtx := map[string]interface{}{
			"agent": map[string]interface{}{
				"role": "planner",
			},
			"task": map[string]interface{}{},
		}
		result, err = engine.Evaluate(ctx, GateTypePreTask, plannerCtx)
		require.NoError(t, err)
		assert.True(t, result.Passed) // Passes because gate doesn't trigger
	})

	// Test bloom level filtering
	t.Run("bloom level filtering", func(t *testing.T) {
		ctx := context.Background()

		// Should trigger for bloom level 5
		highBloomCtx := map[string]interface{}{
			"task": map[string]interface{}{
				"bloom_level": 5,
				// No complexity field - should fail
			},
		}
		result, err := engine.Evaluate(ctx, GateTypePreTask, highBloomCtx)
		require.NoError(t, err)
		assert.False(t, result.Passed)

		// Should not trigger for bloom level 2
		lowBloomCtx := map[string]interface{}{
			"task": map[string]interface{}{
				"bloom_level": 2,
				// No complexity field - but gate doesn't trigger
			},
		}
		result, err = engine.Evaluate(ctx, GateTypePreTask, lowBloomCtx)
		require.NoError(t, err)
		assert.True(t, result.Passed) // Passes because gate doesn't trigger
	})
}

// TestEngine_Evaluate_LogicalOperatorsWithRegex is a regression test for
// sub-condition compilation: compileCondition used to compile loop-variable
// copies, so matches/not_matches under and/or/not always failed with
// "regex not pre-compiled".
func TestEngine_Evaluate_LogicalOperatorsWithRegex(t *testing.T) {
	engine := NewEngine()

	config := &GateConfiguration{
		SchemaVersion: "1.0.0",
		Gates: []GateDefinition{
			{
				ID:       "logical_regex",
				Name:     "Logical Regex",
				Enabled:  ptr.Bool(true),
				Type:     GateTypePreTask,
				Priority: 10,
				Rules: []RuleDefinition{
					{
						ID: "and_matches",
						Condition: RuleCondition{
							Type: ConditionAnd,
							Conditions: []RuleCondition{
								{
									Type:     ConditionFieldValidation,
									Field:    "task.id",
									Operator: OpMatches,
									Value:    `^task-[0-9]+$`,
								},
								{
									Type: ConditionOr,
									Conditions: []RuleCondition{
										{
											Type:     ConditionFieldValidation,
											Field:    "task.content",
											Operator: OpNotMatches,
											Value:    `rm\s+-rf`,
										},
										{
											Type:     ConditionFieldValidation,
											Field:    "task.approved",
											Operator: OpEquals,
											Value:    true,
										},
									},
								},
								{
									Type: ConditionNot,
									Conditions: []RuleCondition{
										{
											Type:     ConditionFieldValidation,
											Field:    "task.content",
											Operator: OpMatches,
											Value:    `forbidden`,
										},
									},
								},
							},
						},
						Severity: SeverityError,
					},
				},
				Action: ActionDefinition{
					OnPass: ActionAllow,
					OnFail: ActionBlock,
				},
			},
		},
	}

	require.NoError(t, engine.LoadConfiguration(config))

	testCases := []struct {
		name       string
		context    map[string]interface{}
		expectPass bool
	}{
		{
			name: "all sub-conditions satisfied",
			context: map[string]interface{}{
				"task": map[string]interface{}{
					"id":      "task-42",
					"content": "echo hello",
				},
			},
			expectPass: true,
		},
		{
			name: "and fails on id regex mismatch",
			context: map[string]interface{}{
				"task": map[string]interface{}{
					"id":      "not-a-task",
					"content": "echo hello",
				},
			},
			expectPass: false,
		},
		{
			name: "or passes via approved despite dangerous content",
			context: map[string]interface{}{
				"task": map[string]interface{}{
					"id":       "task-7",
					"content":  "rm -rf /tmp/scratch",
					"approved": true,
				},
			},
			expectPass: true,
		},
		{
			name: "not fails on forbidden content",
			context: map[string]interface{}{
				"task": map[string]interface{}{
					"id":      "task-9",
					"content": "run forbidden step",
				},
			},
			expectPass: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := engine.Evaluate(context.Background(), GateTypePreTask, tc.context)
			require.NoError(t, err)
			for _, rr := range result.RuleResults {
				require.NoError(t, rr.Error, "rule %s must evaluate without error", rr.RuleID)
			}
			assert.Equal(t, tc.expectPass, result.Passed)
		})
	}
}

func TestEngine_TriggerFilters_Phases(t *testing.T) {
	engine := NewEngine()

	config := &GateConfiguration{
		SchemaVersion: "1.0.0",
		Gates: []GateDefinition{
			{
				ID:       "phase_filtered",
				Name:     "Phase Filtered Gate",
				Enabled:  ptr.Bool(true),
				Type:     GateTypePreTask,
				Priority: 10,
				Trigger: TriggerDefinition{
					Phases: []string{"implementation", "verification"},
				},
				Rules: []RuleDefinition{
					{
						ID: "phase_scoped_rule",
						Condition: RuleCondition{
							Type:     ConditionFieldValidation,
							Field:    "task.checklist",
							Operator: OpExists,
						},
						Severity: SeverityError,
					},
				},
				Action: ActionDefinition{
					OnPass: ActionAllow,
					OnFail: ActionBlock,
				},
			},
		},
	}

	require.NoError(t, engine.LoadConfiguration(config))
	ctx := context.Background()

	// Matching phase without checklist: gate triggers and fails.
	matchingCtx := map[string]interface{}{
		"task": map[string]interface{}{
			"phase": "implementation",
		},
	}
	result, err := engine.Evaluate(ctx, GateTypePreTask, matchingCtx)
	require.NoError(t, err)
	assert.False(t, result.Passed, "gate must trigger for a listed phase")

	// Non-matching phase: gate must not trigger.
	otherPhaseCtx := map[string]interface{}{
		"task": map[string]interface{}{
			"phase": "research",
		},
	}
	result, err = engine.Evaluate(ctx, GateTypePreTask, otherPhaseCtx)
	require.NoError(t, err)
	assert.True(t, result.Passed, "gate must not trigger for an unlisted phase")

	// Missing phase field: gate must not trigger.
	noPhaseCtx := map[string]interface{}{
		"task": map[string]interface{}{},
	}
	result, err = engine.Evaluate(ctx, GateTypePreTask, noPhaseCtx)
	require.NoError(t, err)
	assert.True(t, result.Passed, "gate must not trigger without a phase field")
}

// TestEngine_ConcurrentReloadAndEvaluate exercises configuration reloads
// racing with evaluations. Run with -race: generateCacheKey used to read
// configChecksum without holding e.mu.
func TestEngine_ConcurrentReloadAndEvaluate(t *testing.T) {
	engine := NewEngine()

	makeConfig := func(pattern string) *GateConfiguration {
		return &GateConfiguration{
			SchemaVersion: "1.0.0",
			Gates: []GateDefinition{
				{
					ID:       "reload_race",
					Name:     "Reload Race Gate",
					Enabled:  ptr.Bool(true),
					Type:     GateTypePreTask,
					Priority: 10,
					Rules: []RuleDefinition{
						{
							ID: "match_rule",
							Condition: RuleCondition{
								Type:     ConditionFieldValidation,
								Field:    "task.id",
								Operator: OpMatches,
								Value:    pattern,
							},
							Severity: SeverityError,
						},
					},
					Action: ActionDefinition{
						OnPass: ActionAllow,
						OnFail: ActionBlock,
					},
				},
			},
		}
	}

	require.NoError(t, engine.LoadConfiguration(makeConfig(`^task-a$`)))

	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			pattern := `^task-a$`
			if i%2 == 1 {
				pattern = `^task-b$`
			}
			for j := 0; j < 50; j++ {
				if err := engine.LoadConfiguration(makeConfig(pattern)); err != nil {
					t.Errorf("LoadConfiguration: %v", err)
					return
				}
			}
		}(i)
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			evalCtx := map[string]interface{}{
				"task": map[string]interface{}{
					"id": "task-a",
				},
			}
			for j := 0; j < 50; j++ {
				if _, err := engine.Evaluate(ctx, GateTypePreTask, evalCtx); err != nil {
					t.Errorf("Evaluate: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}
