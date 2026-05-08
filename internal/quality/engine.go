package quality

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
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

// LoadConfiguration loads and compiles gate definitions.
// Heavy work (JSON marshal, SHA-256 hash, regex compilation, sorting) is
// performed outside the lock; only the final swap is done under mu.Lock.
func (e *Engine) LoadConfiguration(config *GateConfiguration) error {
	// --- prepare phase (no lock) ---

	// Calculate configuration checksum
	configData, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}
	checksum := sha256.Sum256(configData)
	newChecksum := hex.EncodeToString(checksum[:])

	// Compile and index gates by type
	newGates := make(map[GateType][]*compiledGate)
	for _, gateDef := range config.Gates {
		if gateDef.Enabled != nil && !*gateDef.Enabled {
			continue
		}

		compiledGate, err := e.compileGate(&gateDef)
		if err != nil {
			return fmt.Errorf("failed to compile gate %s: %w", gateDef.ID, err)
		}

		gateType := gateDef.Type
		newGates[gateType] = append(newGates[gateType], compiledGate)
	}

	// Sort gates by priority (lower number = higher priority)
	for gateType := range newGates {
		sort.Slice(newGates[gateType], func(i, j int) bool {
			return newGates[gateType][i].Priority < newGates[gateType][j].Priority
		})
	}

	// --- swap phase (under lock) ---
	e.mu.Lock()
	e.gates = newGates
	e.configChecksum = newChecksum
	e.mu.Unlock()

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
	cacheKey, keyErr := e.generateCacheKey(string(gateType), evalCtx)
	if keyErr != nil {
		// Skip caching on marshal failure to avoid collisions
		return e.evaluateUncached(ctx, gateType, contextWrapper)
	}

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

	evalResult, ok := result.(*EvaluationResult)
	if !ok {
		return nil, fmt.Errorf("unexpected singleflight result type: %T", result)
	}

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
		if !ok || !isStr || !slices.Contains(trigger.Roles, roleStr) {
			return false
		}
	}

	// Check bloom level filter
	if len(trigger.BloomLevels) > 0 {
		bloomLevel, ok := evalCtx.GetField("task.bloom_level")
		bloomInt, bloomErr := toInt(bloomLevel)
		if !ok || bloomErr != nil || !slices.Contains(trigger.BloomLevels, bloomInt) {
			return false
		}
	}

	// Check task type filter
	if len(trigger.TaskTypes) > 0 {
		taskType, ok := evalCtx.GetField("task.type")
		taskTypeStr, isStr := taskType.(string)
		if !ok || !isStr || !slices.Contains(trigger.TaskTypes, taskTypeStr) {
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

// generateCacheKey generates a cache key for the evaluation.
// Returns an error if the context cannot be deterministically serialized,
// in which case the caller should skip caching to avoid collisions.
func (e *Engine) generateCacheKey(gateType string, context map[string]interface{}) (*cacheKey, error) {
	// Create a canonical JSON representation
	contextData, err := json.Marshal(context)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal context for cache key: %w", err)
	}
	hash := sha256.Sum256(contextData)

	return &cacheKey{
		GateID:             gateType,
		GateVersionHash:    e.configChecksum,
		ContextFingerprint: hex.EncodeToString(hash[:]),
	}, nil
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

func toInt(v interface{}) (int, error) {
	switch val := v.(type) {
	case int:
		return val, nil
	case int64:
		return int(val), nil
	case float64:
		return int(val), nil
	case string:
		i, err := strconv.Atoi(val)
		if err != nil {
			return 0, fmt.Errorf("cannot convert string %q to int: %w", val, err)
		}
		return i, nil
	default:
		return 0, fmt.Errorf("cannot convert %T to int", v)
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

// ConditionFeatureGate is the condition type for feature gate evaluation.
const ConditionFeatureGate ConditionType = "feature_gate"

// FeatureGateRule evaluates feature gates based on task complexity.
//
// This rule is INTENTIONALLY non-blocking. It is a feature-profile selector,
// not a quality gate: it computes the complexity level and resolves the
// corresponding FeatureProfile, but its job ends there. The profile result is
// not propagated through *RuleResult (which only carries pass/severity/
// message), so a "fail" return would only stop the gate evaluation pipeline
// without delivering useful information to the caller.
//
// Note: this evaluator is registered explicitly via RegisterFeatureGateRule;
// the daemon does not register it today. Real gating in production is done by
// the field-validation / script evaluators registered in NewEngine, combined
// with quality_gates.enforcement.failure_action="block" in config.yaml.
type FeatureGateRule struct {
	evaluator FeatureGateEvaluator
	scorer    ComplexityAnalyzer
}

// Evaluate is intentionally non-blocking and always returns (true, nil); see
// the FeatureGateRule doc comment for why this rule cannot fail a task (the
// resolved profile is not propagated through *RuleResult).
//
// The function still walks the resolution pipeline so that any side effects
// in the configured FeatureGateEvaluator / scorer continue to fire:
//
//   - If task.complexity_level is set in the evaluation context, it is used
//     directly; otherwise the level is computed from scorer.Estimate.
//   - The evaluator is queried for the profile at that level. If the
//     returned profile has no enabled features, the evaluator is queried a
//     second time with ProfileLevelSimple. Both lookup results are
//     discarded — the function unconditionally returns (true, nil).
func (r *FeatureGateRule) Evaluate(_ context.Context, _ *RuleCondition, evalCtx EvaluationContext) (bool, error) {
	var level FeatureProfileLevel

	// Explicit complexity level override.
	if raw, ok := evalCtx.GetField("task.complexity_level"); ok {
		if s, isStr := raw.(string); isStr && s != "" {
			level = FeatureProfileLevel(s)
		}
	}

	// Compute complexity when no explicit level is set.
	if level == "" {
		input := r.buildComplexityInput(evalCtx)
		result := r.scorer.Estimate(input)
		level = complexityToProfileLevel(result.Level)
	}

	// Resolve the profile. If the configured FeatureGateEvaluator returns an
	// empty profile for the chosen level (mis-configured tier), retry the
	// lookup with ProfileLevelSimple. Both results are discarded — the gate
	// is non-blocking and the resolved profile is not propagated through
	// *RuleResult; see the FeatureGateRule doc comment.
	if profile := r.evaluator.Evaluate(level); len(profile.EnabledFeatures) == 0 {
		r.evaluator.Evaluate(ProfileLevelSimple)
	}
	return true, nil
}

// buildComplexityInput extracts ComplexityInput fields from the evaluation context.
func (r *FeatureGateRule) buildComplexityInput(evalCtx EvaluationContext) ComplexityInput {
	var input ComplexityInput
	if v, ok := evalCtx.GetField("task.file_count"); ok {
		if i, err := toInt(v); err == nil {
			input.FileCount = i
		}
	}
	if v, ok := evalCtx.GetField("task.dependency_depth"); ok {
		if i, err := toInt(v); err == nil {
			input.DependencyDepth = i
		}
	}
	if v, ok := evalCtx.GetField("task.bloom_level"); ok {
		if i, err := toInt(v); err == nil {
			input.BloomLevel = i
		}
	}
	if v, ok := evalCtx.GetField("task.past_repair_rate"); ok {
		if f, err := toFloat64(v); err == nil {
			input.PastRepairRate = f
		}
	}
	if v, ok := evalCtx.GetField("task.expected_path_count"); ok {
		if i, err := toInt(v); err == nil {
			input.ExpectedPathCount = i
		}
	}
	return input
}

// complexityToProfileLevel maps a ComplexityLevel to a FeatureProfileLevel.
func complexityToProfileLevel(level ComplexityLevel) FeatureProfileLevel {
	switch level {
	case ComplexityLevelSimple:
		return ProfileLevelSimple
	case ComplexityLevelStandard:
		return ProfileLevelStandard
	case ComplexityLevelComplex:
		return ProfileLevelComplex
	case ComplexityLevelCritical:
		return ProfileLevelCritical
	default:
		return ProfileLevelSimple
	}
}

// RegisterFeatureGateRule registers a FeatureGateRule as a pre_task evaluator on the engine.
func RegisterFeatureGateRule(engine *Engine, evaluator FeatureGateEvaluator, scorer ComplexityAnalyzer) {
	rule := &FeatureGateRule{
		evaluator: evaluator,
		scorer:    scorer,
	}
	engine.registerEvaluator(ConditionFeatureGate, rule)
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
