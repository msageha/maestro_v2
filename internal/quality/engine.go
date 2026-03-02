package quality

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// Engine is the main quality gate evaluation engine
type Engine struct {
	gates          map[GateType][]*CompiledGate
	evaluators     map[ConditionType]RuleEvaluator
	cache          *ResultCache
	singleflight   *singleflight.Group
	mu             sync.RWMutex
	configVersion  string
	configChecksum string
}

// CompiledGate represents a gate with pre-compiled conditions
type CompiledGate struct {
	*GateDefinition
	compiledRules []*CompiledRule
}

// CompiledRule represents a rule with pre-compiled conditions
type CompiledRule struct {
	*RuleDefinition
	compiledCondition *CompiledCondition
}

// CompiledCondition represents a pre-compiled condition
type CompiledCondition struct {
	*RuleCondition
	compiledRegex *regexp.Regexp
	subConditions []*CompiledCondition
}

// NewEngine creates a new quality gate engine
func NewEngine() *Engine {
	engine := &Engine{
		gates:        make(map[GateType][]*CompiledGate),
		evaluators:   make(map[ConditionType]RuleEvaluator),
		cache:        NewResultCache(1000, 30*time.Second), // 1000 items, 30s TTL
		singleflight: &singleflight.Group{},
	}

	// Register built-in evaluators
	engine.RegisterEvaluator(ConditionFieldValidation, &FieldValidationEvaluator{})
	engine.RegisterEvaluator(ConditionAnd, &LogicalAndEvaluator{engine: engine})
	engine.RegisterEvaluator(ConditionOr, &LogicalOrEvaluator{engine: engine})
	engine.RegisterEvaluator(ConditionNot, &LogicalNotEvaluator{engine: engine})
	engine.RegisterEvaluator(ConditionResourceLimit, &ResourceLimitEvaluator{})
	engine.RegisterEvaluator(ConditionDependencyCheck, &DependencyCheckEvaluator{})
	engine.RegisterEvaluator(ConditionScript, &ScriptEvaluator{})

	return engine
}

// RegisterEvaluator registers a rule evaluator for a condition type
func (e *Engine) RegisterEvaluator(condType ConditionType, evaluator RuleEvaluator) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.evaluators[condType] = evaluator
}

// LoadConfiguration loads and compiles gate definitions
func (e *Engine) LoadConfiguration(config *GateConfiguration) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	// Clear existing gates
	e.gates = make(map[GateType][]*CompiledGate)

	// Calculate configuration checksum
	configData, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}
	checksum := sha256.Sum256(configData)
	e.configChecksum = hex.EncodeToString(checksum[:])
	e.configVersion = config.SchemaVersion

	// Compile and index gates by type
	for _, gateDef := range config.Gates {
		if !gateDef.Enabled {
			continue
		}

		compiledGate, err := e.compileGate(&gateDef)
		if err != nil {
			return fmt.Errorf("failed to compile gate %s: %w", gateDef.ID, err)
		}

		gateType := gateDef.Type
		e.gates[gateType] = append(e.gates[gateType], compiledGate)
	}

	// Sort gates by priority (lower number = higher priority)
	for gateType := range e.gates {
		sort.Slice(e.gates[gateType], func(i, j int) bool {
			return e.gates[gateType][i].Priority < e.gates[gateType][j].Priority
		})
	}

	// Clear cache when configuration changes
	e.cache.Clear()

	return nil
}

// compileGate pre-compiles a gate definition for efficient evaluation
func (e *Engine) compileGate(gateDef *GateDefinition) (*CompiledGate, error) {
	compiledGate := &CompiledGate{
		GateDefinition: gateDef,
		compiledRules:  make([]*CompiledRule, 0, len(gateDef.Rules)),
	}

	for _, ruleDef := range gateDef.Rules {
		compiledRule, err := e.compileRule(&ruleDef)
		if err != nil {
			return nil, fmt.Errorf("failed to compile rule %s: %w", ruleDef.ID, err)
		}
		compiledGate.compiledRules = append(compiledGate.compiledRules, compiledRule)
	}

	return compiledGate, nil
}

// compileRule pre-compiles a rule definition
func (e *Engine) compileRule(ruleDef *RuleDefinition) (*CompiledRule, error) {
	compiledCondition, err := e.compileCondition(&ruleDef.Condition)
	if err != nil {
		return nil, err
	}

	return &CompiledRule{
		RuleDefinition:    ruleDef,
		compiledCondition: compiledCondition,
	}, nil
}

// compileCondition pre-compiles a rule condition
func (e *Engine) compileCondition(condition *RuleCondition) (*CompiledCondition, error) {
	compiled := &CompiledCondition{
		RuleCondition: condition,
	}

	// Compile regex patterns
	if condition.Type == ConditionFieldValidation &&
		(condition.Operator == OpMatches || condition.Operator == OpNotMatches) {
		if strValue, ok := condition.Value.(string); ok {
			re, err := regexp.Compile(strValue)
			if err != nil {
				return nil, fmt.Errorf("invalid regex pattern: %w", err)
			}
			compiled.compiledRegex = re
		}
	}

	// Recursively compile sub-conditions for logical operators
	if len(condition.Conditions) > 0 {
		compiled.subConditions = make([]*CompiledCondition, 0, len(condition.Conditions))
		for _, subCond := range condition.Conditions {
			compiledSub, err := e.compileCondition(&subCond)
			if err != nil {
				return nil, err
			}
			compiled.subConditions = append(compiled.subConditions, compiledSub)
		}
	}

	return compiled, nil
}

// Evaluate evaluates all gates of the specified type against the context
func (e *Engine) Evaluate(ctx context.Context, gateType GateType, evalCtx map[string]interface{}) (*EvaluationResult, error) {
	// Create context wrapper
	contextWrapper := &MapEvaluationContext{data: evalCtx}

	// Generate cache key
	cacheKey := e.generateCacheKey(string(gateType), evalCtx)

	// Try to get from cache
	if cached := e.cache.Get(cacheKey); cached != nil {
		cached.CacheHit = true
		return cached, nil
	}

	// Use singleflight to prevent duplicate evaluations
	key := fmt.Sprintf("%s:%s", gateType, cacheKey.ContextFingerprint)
	result, err, _ := e.singleflight.Do(key, func() (interface{}, error) {
		return e.evaluateUncached(ctx, gateType, contextWrapper)
	})

	if err != nil {
		return nil, err
	}

	evalResult := result.(*EvaluationResult)

	// Cache successful evaluations
	if evalResult.Error == nil && !evalResult.TimedOut {
		e.cache.Set(cacheKey, evalResult)
	}

	return evalResult, nil
}

// evaluateUncached performs the actual gate evaluation
func (e *Engine) evaluateUncached(ctx context.Context, gateType GateType, evalCtx EvaluationContext) (*EvaluationResult, error) {
	start := time.Now()

	// Set evaluation timeout (100ms budget)
	timeoutCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()

	e.mu.RLock()
	gates := e.gates[gateType]
	e.mu.RUnlock()

	result := &EvaluationResult{
		GateType:    gateType,
		Passed:      true,
		Action:      ActionAllow,
		RuleResults: make([]RuleResult, 0),
		FailedGates: make([]string, 0),
	}

	// Evaluate gates in priority order
	for _, gate := range gates {
		// Check if we should trigger this gate
		if !e.shouldTriggerGate(gate, evalCtx) {
			continue
		}

		// Check timeout
		select {
		case <-timeoutCtx.Done():
			result.TimedOut = true
			result.Duration = time.Since(start)
			return result, nil
		default:
		}

		gateResult := e.evaluateGate(timeoutCtx, gate, evalCtx)
		result.RuleResults = append(result.RuleResults, gateResult.RuleResults...)

		if !gateResult.Passed {
			result.Passed = false
			result.FailedGates = append(result.FailedGates, gate.ID)

			// Determine action based on severity
			if gateResult.Action == ActionBlock {
				result.Action = ActionBlock
				break // Stop evaluation on block
			} else if gateResult.Action == ActionWarn && result.Action != ActionBlock {
				result.Action = ActionWarn
			}
		}
	}

	result.Duration = time.Since(start)
	return result, nil
}

// evaluateGate evaluates a single gate
func (e *Engine) evaluateGate(ctx context.Context, gate *CompiledGate, evalCtx EvaluationContext) *EvaluationResult {
	result := &EvaluationResult{
		GateID:      gate.ID,
		GateType:    gate.Type,
		Passed:      true,
		Action:      gate.Action.OnPass,
		RuleResults: make([]RuleResult, 0, len(gate.compiledRules)),
	}

	hasError := false
	hasWarning := false

	for _, rule := range gate.compiledRules {
		ruleResult := e.evaluateRule(ctx, rule, evalCtx)
		result.RuleResults = append(result.RuleResults, ruleResult)

		if !ruleResult.Passed {
			result.Passed = false

			switch ruleResult.Severity {
			case SeverityCritical, SeverityError:
				hasError = true
			case SeverityWarning:
				hasWarning = true
			}
		}

		// Check for timeout
		select {
		case <-ctx.Done():
			result.TimedOut = true
			return result
		default:
		}
	}

	// Determine action based on results
	if !result.Passed {
		if hasError {
			result.Action = gate.Action.OnFail
		} else if hasWarning && gate.Action.OnWarn != "" {
			result.Action = gate.Action.OnWarn
		} else {
			result.Action = gate.Action.OnFail
		}
	}

	return result
}

// evaluateRule evaluates a single rule
func (e *Engine) evaluateRule(ctx context.Context, rule *CompiledRule, evalCtx EvaluationContext) RuleResult {
	start := time.Now()

	result := RuleResult{
		RuleID:   rule.ID,
		Severity: rule.Severity,
	}

	// Get the appropriate evaluator
	e.mu.RLock()
	evaluator, exists := e.evaluators[rule.compiledCondition.Type]
	e.mu.RUnlock()

	if !exists {
		result.Error = fmt.Errorf("unknown condition type: %s", rule.compiledCondition.Type)
		result.Passed = false
		result.Message = result.Error.Error()
		result.Duration = time.Since(start)
		return result
	}

	// Evaluate the condition
	passed, err := e.evaluateCondition(ctx, rule.compiledCondition, evalCtx, evaluator)

	result.Passed = passed
	result.Error = err
	result.Duration = time.Since(start)

	if err != nil {
		result.Message = fmt.Sprintf("Rule evaluation error: %v", err)
	} else if !passed {
		result.Message = fmt.Sprintf("Rule %s failed: %s", rule.ID, rule.Description)
	}

	return result
}

// evaluateCondition evaluates a compiled condition recursively
func (e *Engine) evaluateCondition(ctx context.Context, condition *CompiledCondition, evalCtx EvaluationContext, evaluator RuleEvaluator) (bool, error) {
	// For logical operators with sub-conditions, we need special handling
	switch condition.Type {
	case ConditionAnd, ConditionOr, ConditionNot:
		// These are handled by their respective evaluators which will call back into evaluateCondition
		return evaluator.Evaluate(ctx, condition.RuleCondition, evalCtx)
	default:
		// For other types, use the registered evaluator directly
		return evaluator.Evaluate(ctx, condition.RuleCondition, evalCtx)
	}
}

// shouldTriggerGate checks if a gate should be triggered based on context
func (e *Engine) shouldTriggerGate(gate *CompiledGate, evalCtx EvaluationContext) bool {
	trigger := &gate.Trigger

	// Check role filter
	if len(trigger.Roles) > 0 {
		if role, ok := evalCtx.GetField("agent.role"); ok {
			if !contains(trigger.Roles, role.(string)) {
				return false
			}
		}
	}

	// Check bloom level filter
	if len(trigger.BloomLevels) > 0 {
		if bloomLevel, ok := evalCtx.GetField("task.bloom_level"); ok {
			if !containsInt(trigger.BloomLevels, toInt(bloomLevel)) {
				return false
			}
		}
	}

	// Check task type filter
	if len(trigger.TaskTypes) > 0 {
		if taskType, ok := evalCtx.GetField("task.type"); ok {
			if !contains(trigger.TaskTypes, taskType.(string)) {
				return false
			}
		}
	}

	// Check pattern triggers
	for _, pattern := range trigger.Patterns {
		if value, ok := evalCtx.GetField(pattern.Field); ok {
			re, err := regexp.Compile(pattern.Regex)
			if err != nil {
				continue
			}
			matched := re.MatchString(fmt.Sprintf("%v", value))
			if pattern.Negate {
				matched = !matched
			}
			if !matched {
				return false
			}
		}
	}

	return true
}

// generateCacheKey generates a cache key for the evaluation
func (e *Engine) generateCacheKey(gateType string, context map[string]interface{}) *CacheKey {
	// Create a canonical JSON representation
	contextData, _ := json.Marshal(context)
	hash := sha256.Sum256(contextData)

	return &CacheKey{
		GateID:             gateType,
		GateVersionHash:    e.configChecksum,
		ContextFingerprint: hex.EncodeToString(hash[:]),
	}
}

// Helper functions
func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

func containsInt(slice []int, item int) bool {
	for _, i := range slice {
		if i == item {
			return true
		}
	}
	return false
}

func toInt(v interface{}) int {
	switch val := v.(type) {
	case int:
		return val
	case int64:
		return int(val)
	case float64:
		return int(val)
	case string:
		// Try to parse string as int
		var i int
		fmt.Sscanf(val, "%d", &i)
		return i
	default:
		return 0
	}
}

// MapEvaluationContext provides evaluation context from a map
type MapEvaluationContext struct {
	data map[string]interface{}
}

// GetField retrieves a field value by path (e.g., "task.purpose")
func (m *MapEvaluationContext) GetField(path string) (interface{}, bool) {
	parts := strings.Split(path, ".")
	current := m.data

	for i, part := range parts {
		if i == len(parts)-1 {
			// Last part - return the value
			val, ok := current[part]
			return val, ok
		}

		// Navigate deeper
		if next, ok := current[part]; ok {
			if nextMap, ok := next.(map[string]interface{}); ok {
				current = nextMap
			} else {
				return nil, false
			}
		} else {
			return nil, false
		}
	}

	return nil, false
}

// GetResource retrieves resource metrics
func (m *MapEvaluationContext) GetResource(metric string, scope string) (float64, error) {
	// This would be implemented to fetch actual metrics
	// For now, return a placeholder
	return 0, nil
}

// GetDependencies retrieves dependency information
func (m *MapEvaluationContext) GetDependencies(mode string) ([]string, error) {
	// This would be implemented to fetch actual dependencies
	// For now, return empty
	return []string{}, nil
}