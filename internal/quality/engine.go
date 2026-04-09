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
	gates          map[GateType][]*compiledGate
	evaluators     map[ConditionType]RuleEvaluator
	cache          *resultCache
	singleflight   *singleflight.Group
	mu             sync.RWMutex
	configChecksum string
}

// compiledGate represents a gate with pre-compiled conditions
type compiledGate struct {
	*GateDefinition
	compiledRules []*compiledRule
}

// compiledRule represents a rule with pre-compiled conditions
type compiledRule struct {
	*RuleDefinition
	compiledCondition *compiledCondition
}

// compiledCondition represents a pre-compiled condition
type compiledCondition struct {
	*RuleCondition
	compiledRegex *regexp.Regexp
	subConditions []*compiledCondition
}

// NewEngine creates a new quality gate engine
func NewEngine() *Engine {
	engine := &Engine{
		gates:        make(map[GateType][]*compiledGate),
		evaluators:   make(map[ConditionType]RuleEvaluator),
		cache:        newResultCache(1000, 30*time.Second), // 1000 items, 30s TTL
		singleflight: &singleflight.Group{},
	}

	// Register built-in evaluators
	engine.registerEvaluator(ConditionFieldValidation, &fieldValidationEvaluator{})
	engine.registerEvaluator(ConditionAnd, &logicalAndEvaluator{engine: engine})
	engine.registerEvaluator(ConditionOr, &logicalOrEvaluator{engine: engine})
	engine.registerEvaluator(ConditionNot, &logicalNotEvaluator{engine: engine})
	engine.registerEvaluator(ConditionScript, &scriptEvaluator{})

	return engine
}

// registerEvaluator registers a rule evaluator for a condition type
func (e *Engine) registerEvaluator(condType ConditionType, evaluator RuleEvaluator) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.evaluators[condType] = evaluator
}

// LoadConfiguration loads and compiles gate definitions
func (e *Engine) LoadConfiguration(config *GateConfiguration) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	// Clear existing gates
	e.gates = make(map[GateType][]*compiledGate)

	// Calculate configuration checksum
	configData, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}
	checksum := sha256.Sum256(configData)
	e.configChecksum = hex.EncodeToString(checksum[:])
	// Compile and index gates by type
	for _, gateDef := range config.Gates {
		if gateDef.Enabled != nil && !*gateDef.Enabled {
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
func (e *Engine) compileGate(gateDef *GateDefinition) (*compiledGate, error) {
	compiledGate := &compiledGate{
		GateDefinition: gateDef,
		compiledRules:  make([]*compiledRule, 0, len(gateDef.Rules)),
	}

	// Pre-compile trigger pattern regexes to avoid repeated compilation in shouldTriggerGate
	for i := range gateDef.Trigger.Patterns {
		pattern := &gateDef.Trigger.Patterns[i]
		if pattern.Regex != "" && pattern.CompiledRegex == nil {
			re, err := regexp.Compile(pattern.Regex)
			if err != nil {
				return nil, fmt.Errorf("failed to compile trigger pattern regex %q: %w", pattern.Regex, err)
			}
			pattern.CompiledRegex = re
		}
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
func (e *Engine) compileRule(ruleDef *RuleDefinition) (*compiledRule, error) {
	compiledCondition, err := e.compileCondition(&ruleDef.Condition)
	if err != nil {
		return nil, err
	}

	return &compiledRule{
		RuleDefinition:    ruleDef,
		compiledCondition: compiledCondition,
	}, nil
}

// compileCondition pre-compiles a rule condition
func (e *Engine) compileCondition(condition *RuleCondition) (*compiledCondition, error) {
	compiled := &compiledCondition{
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
			// Also set on RuleCondition so evaluators can access it
			condition.CompiledRegex = re
		}
	}

	// Recursively compile sub-conditions for logical operators
	if len(condition.Conditions) > 0 {
		compiled.subConditions = make([]*compiledCondition, 0, len(condition.Conditions))
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
	contextWrapper := &mapEvaluationContext{data: evalCtx}

	// Generate cache key
	cacheKey := e.generatecacheKey(string(gateType), evalCtx)

	// Try to get from cache
	if cached := e.cache.Get(cacheKey); cached != nil {
		cached.CacheHit = true
		return cached, nil
	}

	// Use singleflight to prevent duplicate evaluations
	// Use null byte separator to avoid key collision when gateType contains ":"
	key := string(gateType) + "\x00" + cacheKey.ContextFingerprint
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

		// Check timeout — fail-closed: timeout means not passed
		select {
		case <-timeoutCtx.Done():
			result.TimedOut = true
			result.Passed = false
			result.Action = ActionBlock
			result.Duration = time.Since(start)
			return result, nil
		default:
		}

		gateResult := e.evaluateGate(timeoutCtx, gate, evalCtx)
		result.RuleResults = append(result.RuleResults, gateResult.RuleResults...)

		// Propagate timeout from gate evaluation — fail-closed
		if gateResult.TimedOut {
			result.TimedOut = true
			result.Passed = false
			result.Action = ActionBlock
			result.FailedGates = append(result.FailedGates, gate.ID)
			result.Duration = time.Since(start)
			return result, nil
		}

		if !gateResult.Passed {
			result.Passed = false
			result.FailedGates = append(result.FailedGates, gate.ID)

			// Determine action based on severity
			if gateResult.Action == ActionBlock {
				result.Action = ActionBlock
				break // Stop evaluation on block
			}
			if gateResult.Action == ActionWarn && result.Action != ActionBlock {
				result.Action = ActionWarn
			}
		}
	}

	result.Duration = time.Since(start)
	return result, nil
}

// evaluateGate evaluates a single gate
func (e *Engine) evaluateGate(ctx context.Context, gate *compiledGate, evalCtx EvaluationContext) *EvaluationResult {
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

		// Check for timeout — fail-closed
		select {
		case <-ctx.Done():
			result.TimedOut = true
			result.Passed = false
			result.Action = ActionBlock
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
func (e *Engine) evaluateRule(ctx context.Context, rule *compiledRule, evalCtx EvaluationContext) RuleResult {
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

// evaluateCondition evaluates a compiled condition recursively.
// Logical operators (And/Or/Not) are handled internally by the evaluator
// via recursive calls, so all condition types use the same code path.
func (e *Engine) evaluateCondition(ctx context.Context, condition *compiledCondition, evalCtx EvaluationContext, evaluator RuleEvaluator) (bool, error) {
	return evaluator.Evaluate(ctx, condition.RuleCondition, evalCtx)
}

// shouldTriggerGate checks if a gate should be triggered based on context
func (e *Engine) shouldTriggerGate(gate *compiledGate, evalCtx EvaluationContext) bool {
	trigger := &gate.Trigger

	// Check role filter
	if len(trigger.Roles) > 0 {
		role, ok := evalCtx.GetField("agent.role")
		roleStr, isStr := role.(string)
		if !ok || !isStr || !contains(trigger.Roles, roleStr) {
			return false
		}
	}

	// Check bloom level filter
	if len(trigger.BloomLevels) > 0 {
		bloomLevel, ok := evalCtx.GetField("task.bloom_level")
		if !ok || !containsInt(trigger.BloomLevels, toInt(bloomLevel)) {
			return false
		}
	}

	// Check task type filter
	if len(trigger.TaskTypes) > 0 {
		taskType, ok := evalCtx.GetField("task.type")
		taskTypeStr, isStr := taskType.(string)
		if !ok || !isStr || !contains(trigger.TaskTypes, taskTypeStr) {
			return false
		}
	}

	// Check pattern triggers using pre-compiled regexes
	for _, pattern := range trigger.Patterns {
		if value, ok := evalCtx.GetField(pattern.Field); ok {
			re := pattern.CompiledRegex
			if re == nil {
				// Fallback: should not happen if compileGate ran, but be safe
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

// generatecacheKey generates a cache key for the evaluation
func (e *Engine) generatecacheKey(gateType string, context map[string]interface{}) *cacheKey {
	// Create a canonical JSON representation
	contextData, _ := json.Marshal(context)
	hash := sha256.Sum256(contextData)

	return &cacheKey{
		GateID:             gateType,
		GateVersionHash:    e.configChecksum,
		ContextFingerprint: hex.EncodeToString(hash[:]),
	}
}

// getEvaluator retrieves a registered evaluator with proper read-lock protection.
// This must be used by logical evaluators (AND/OR/NOT) that access the evaluators map
// outside of Engine.evaluateRule, which already holds the lock.
func (e *Engine) getEvaluator(condType ConditionType) (RuleEvaluator, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	eval, exists := e.evaluators[condType]
	return eval, exists
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

func toFloat64(v interface{}) (float64, error) {
	switch val := v.(type) {
	case float64:
		return val, nil
	case float32:
		return float64(val), nil
	case int:
		return float64(val), nil
	case int64:
		return float64(val), nil
	case string:
		var f float64
		_, err := fmt.Sscanf(val, "%f", &f)
		return f, err
	default:
		return 0, fmt.Errorf("cannot convert %T to float64", v)
	}
}

// mapEvaluationContext provides evaluation context from a map
type mapEvaluationContext struct {
	data map[string]interface{}
}

// GetField retrieves a field value by path (e.g., "task.purpose")
func (m *mapEvaluationContext) GetField(path string) (interface{}, bool) {
	parts := strings.Split(path, ".")
	current := m.data

	for i, part := range parts {
		if i == len(parts)-1 {
			// Last part - return the value
			val, ok := current[part]
			return val, ok
		}

		// Navigate deeper
		next, ok := current[part]
		if !ok {
			return nil, false
		}
		nextMap, ok := next.(map[string]interface{})
		if !ok {
			return nil, false
		}
		current = nextMap
	}

	return nil, false
}

