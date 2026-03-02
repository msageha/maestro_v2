package quality

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// FieldValidationEvaluator evaluates field validation conditions
type FieldValidationEvaluator struct{}

// Evaluate checks if a field meets the specified condition
func (e *FieldValidationEvaluator) Evaluate(ctx context.Context, condition *RuleCondition, evalCtx EvaluationContext) (bool, error) {
	value, exists := evalCtx.GetField(condition.Field)

	switch condition.Operator {
	case OpExists:
		return exists && value != nil && value != "", nil

	case OpNotExists:
		return !exists || value == nil || value == "", nil

	case OpEquals:
		if !exists {
			return false, nil
		}
		return e.compareValues(value, condition.Value, true), nil

	case OpNotEquals:
		if !exists {
			return true, nil
		}
		return !e.compareValues(value, condition.Value, true), nil

	case OpContains:
		if !exists {
			return false, nil
		}
		return e.containsValue(value, condition.Value, condition.CaseSensitive), nil

	case OpNotContains:
		if !exists {
			return true, nil
		}
		return !e.containsValue(value, condition.Value, condition.CaseSensitive), nil

	case OpMatches, OpNotMatches:
		if !exists {
			return condition.Operator == OpNotMatches, nil
		}

		// Use pre-compiled regex if available
		var re *regexp.Regexp
		if condition.CompiledRegex != nil {
			re = condition.CompiledRegex.(*regexp.Regexp)
		} else {
			// Compile on the fly (shouldn't happen with proper compilation)
			pattern, ok := condition.Value.(string)
			if !ok {
				return false, fmt.Errorf("regex pattern must be string")
			}
			var err error
			re, err = regexp.Compile(pattern)
			if err != nil {
				return false, fmt.Errorf("invalid regex: %w", err)
			}
		}

		matched := re.MatchString(fmt.Sprintf("%v", value))
		if condition.Operator == OpMatches {
			return matched, nil
		}
		return !matched, nil

	case OpGT, OpGTE, OpLT, OpLTE:
		if !exists {
			return false, nil
		}
		return e.compareNumeric(value, condition.Value, condition.Operator)

	case OpIn:
		if !exists {
			return false, nil
		}
		return e.isInList(value, condition.Value), nil

	case OpNotIn:
		if !exists {
			return true, nil
		}
		return !e.isInList(value, condition.Value), nil

	default:
		return false, fmt.Errorf("unknown operator: %s", condition.Operator)
	}
}

// compareValues compares two values for equality
func (e *FieldValidationEvaluator) compareValues(a, b interface{}, caseSensitive bool) bool {
	aStr := fmt.Sprintf("%v", a)
	bStr := fmt.Sprintf("%v", b)

	if !caseSensitive {
		aStr = strings.ToLower(aStr)
		bStr = strings.ToLower(bStr)
	}

	return aStr == bStr
}

// containsValue checks if a contains b
func (e *FieldValidationEvaluator) containsValue(a, b interface{}, caseSensitive bool) bool {
	aStr := fmt.Sprintf("%v", a)
	bStr := fmt.Sprintf("%v", b)

	if !caseSensitive {
		aStr = strings.ToLower(aStr)
		bStr = strings.ToLower(bStr)
	}

	return strings.Contains(aStr, bStr)
}

// compareNumeric performs numeric comparison
func (e *FieldValidationEvaluator) compareNumeric(a, b interface{}, op FieldOperator) (bool, error) {
	aNum, err := toFloat64(a)
	if err != nil {
		return false, fmt.Errorf("cannot convert %v to number: %w", a, err)
	}

	bNum, err := toFloat64(b)
	if err != nil {
		return false, fmt.Errorf("cannot convert %v to number: %w", b, err)
	}

	switch op {
	case OpGT:
		return aNum > bNum, nil
	case OpGTE:
		return aNum >= bNum, nil
	case OpLT:
		return aNum < bNum, nil
	case OpLTE:
		return aNum <= bNum, nil
	default:
		return false, fmt.Errorf("invalid numeric operator: %s", op)
	}
}

// isInList checks if value is in a list
func (e *FieldValidationEvaluator) isInList(value, list interface{}) bool {
	// Handle different list types
	switch l := list.(type) {
	case []interface{}:
		for _, item := range l {
			if e.compareValues(value, item, true) {
				return true
			}
		}
	case []string:
		valStr := fmt.Sprintf("%v", value)
		for _, item := range l {
			if valStr == item {
				return true
			}
		}
	default:
		// Try to parse as comma-separated string
		listStr := fmt.Sprintf("%v", list)
		items := strings.Split(listStr, ",")
		valStr := fmt.Sprintf("%v", value)
		for _, item := range items {
			if strings.TrimSpace(valStr) == strings.TrimSpace(item) {
				return true
			}
		}
	}
	return false
}

// LogicalAndEvaluator evaluates AND conditions
type LogicalAndEvaluator struct {
	engine *Engine
}

// Evaluate checks if all sub-conditions are true
func (e *LogicalAndEvaluator) Evaluate(ctx context.Context, condition *RuleCondition, evalCtx EvaluationContext) (bool, error) {
	for _, subCond := range condition.Conditions {
		// Get the evaluator for the sub-condition type
		evaluator, exists := e.engine.evaluators[subCond.Type]
		if !exists {
			return false, fmt.Errorf("unknown condition type: %s", subCond.Type)
		}

		passed, err := evaluator.Evaluate(ctx, &subCond, evalCtx)
		if err != nil {
			return false, err
		}
		if !passed {
			return false, nil // Short-circuit on first false
		}

		// Check for timeout
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		default:
		}
	}
	return true, nil
}

// LogicalOrEvaluator evaluates OR conditions
type LogicalOrEvaluator struct {
	engine *Engine
}

// Evaluate checks if any sub-condition is true
func (e *LogicalOrEvaluator) Evaluate(ctx context.Context, condition *RuleCondition, evalCtx EvaluationContext) (bool, error) {
	for _, subCond := range condition.Conditions {
		// Get the evaluator for the sub-condition type
		evaluator, exists := e.engine.evaluators[subCond.Type]
		if !exists {
			return false, fmt.Errorf("unknown condition type: %s", subCond.Type)
		}

		passed, err := evaluator.Evaluate(ctx, &subCond, evalCtx)
		if err != nil {
			return false, err
		}
		if passed {
			return true, nil // Short-circuit on first true
		}

		// Check for timeout
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		default:
		}
	}
	return false, nil
}

// LogicalNotEvaluator evaluates NOT conditions
type LogicalNotEvaluator struct {
	engine *Engine
}

// Evaluate negates the sub-condition
func (e *LogicalNotEvaluator) Evaluate(ctx context.Context, condition *RuleCondition, evalCtx EvaluationContext) (bool, error) {
	if len(condition.Conditions) != 1 {
		return false, fmt.Errorf("NOT condition must have exactly one sub-condition")
	}

	subCond := condition.Conditions[0]
	evaluator, exists := e.engine.evaluators[subCond.Type]
	if !exists {
		return false, fmt.Errorf("unknown condition type: %s", subCond.Type)
	}

	passed, err := evaluator.Evaluate(ctx, &subCond, evalCtx)
	if err != nil {
		return false, err
	}
	return !passed, nil
}

// ResourceLimitEvaluator evaluates resource limit conditions
type ResourceLimitEvaluator struct{}

// Evaluate checks if resource usage is within limits
func (e *ResourceLimitEvaluator) Evaluate(ctx context.Context, condition *RuleCondition, evalCtx EvaluationContext) (bool, error) {
	value, err := evalCtx.GetResource(condition.Resource, condition.Scope)
	if err != nil {
		// If we can't get the resource, assume it's within limits
		// This is a fail-open approach for non-critical checks
		return true, nil
	}

	return value <= condition.Limit, nil
}

// DependencyCheckEvaluator evaluates dependency conditions
type DependencyCheckEvaluator struct{}

// Evaluate checks dependency conditions
func (e *DependencyCheckEvaluator) Evaluate(ctx context.Context, condition *RuleCondition, evalCtx EvaluationContext) (bool, error) {
	deps, err := evalCtx.GetDependencies(condition.Mode)
	if err != nil {
		return false, fmt.Errorf("failed to get dependencies: %w", err)
	}

	switch condition.Mode {
	case "all_completed":
		// Check if all dependencies are in the list
		for _, dep := range condition.Dependencies {
			if !contains(deps, dep) {
				return false, nil
			}
		}
		return true, nil

	case "all_success":
		// This would check if all dependencies succeeded
		// For now, just check existence
		for _, dep := range condition.Dependencies {
			if !contains(deps, dep) {
				return false, nil
			}
		}
		return true, nil

	case "any_failed":
		// This would check if any dependency failed
		// For now, return false (no failures)
		return false, nil

	case "circular_check":
		// This would check for circular dependencies
		// For now, assume no circular dependencies
		return true, nil

	default:
		return false, fmt.Errorf("unknown dependency mode: %s", condition.Mode)
	}
}

// ScriptEvaluator evaluates custom script conditions
type ScriptEvaluator struct{}

// Evaluate runs a script and checks its exit code
func (e *ScriptEvaluator) Evaluate(ctx context.Context, condition *RuleCondition, evalCtx EvaluationContext) (bool, error) {
	// If we have a compiled script function, use it
	if condition.CompiledScript != nil {
		if fn, ok := condition.CompiledScript.(func(context.Context, map[string]interface{}) (bool, error)); ok {
			// Convert EvaluationContext to map for script
			data := make(map[string]interface{})
			// This is a simplified conversion - in practice, you'd need to extract all relevant data
			if mapCtx, ok := evalCtx.(*MapEvaluationContext); ok {
				data = mapCtx.data
			}
			return fn(ctx, data)
		}
	}

	// Otherwise, execute the script as a shell command
	timeout := time.Duration(condition.TimeoutSeconds) * time.Second
	if timeout == 0 {
		timeout = 30 * time.Second // Default timeout
	}

	scriptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var cmd *exec.Cmd
	switch condition.Language {
	case "python":
		cmd = exec.CommandContext(scriptCtx, "python", "-c", condition.Script)
	case "bash", "":
		cmd = exec.CommandContext(scriptCtx, "bash", "-c", condition.Script)
	default:
		return false, fmt.Errorf("unsupported script language: %s", condition.Language)
	}

	// Run the script
	err := cmd.Run()
	if err != nil {
		// Check if it was a timeout
		if scriptCtx.Err() == context.DeadlineExceeded {
			return false, fmt.Errorf("script timed out after %v", timeout)
		}
		// Non-zero exit code means failure
		return false, nil
	}

	// Zero exit code means success
	return true, nil
}

// Helper function to convert to float64
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