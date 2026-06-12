package quality

import (
	"context"
	"regexp"
	"time"
)

// GateType represents the type of quality gate
type GateType string

const (
	// GateTypePreTask is evaluated before a task begins execution.
	GateTypePreTask GateType = "pre_task"
	// GateTypePostTask is evaluated after a task completes execution.
	GateTypePostTask GateType = "post_task"
	// GateTypePhaseTransition is evaluated when transitioning between plan phases.
	GateTypePhaseTransition GateType = "phase_transition"
	// GateTypeCommandValidation is evaluated when validating an incoming command.
	GateTypeCommandValidation GateType = "command_validation"
)

// Severity represents the severity level of a rule failure
type Severity string

const (
	// SeverityInfo is the lowest quality gate severity level.
	SeverityInfo Severity = "info"
	// SeverityWarning indicates a non-blocking concern that should be reviewed.
	SeverityWarning Severity = "warning"
	// SeverityError indicates a blocking failure that prevents the operation.
	SeverityError Severity = "error"
	// SeverityCritical indicates a critical failure requiring immediate attention.
	SeverityCritical Severity = "critical"
)

// ActionType represents the action to take based on gate evaluation
type ActionType string

const (
	// ActionAllow permits the operation to proceed without interruption.
	ActionAllow ActionType = "allow"
	// ActionLog records the gate result without blocking the operation.
	ActionLog ActionType = "log"
	// ActionBlock prevents the operation from proceeding.
	ActionBlock ActionType = "block"
	// ActionWarn records a warning without blocking the operation.
	ActionWarn ActionType = "warn"
	// ActionContinue allows the operation to continue despite a gate failure.
	ActionContinue ActionType = "continue"
)

// ConditionType represents the type of rule condition
type ConditionType string

const (
	// ConditionFieldValidation checks a specific field value against an operator and value.
	ConditionFieldValidation ConditionType = "field_validation"
	// ConditionAnd requires all sub-conditions to be true.
	ConditionAnd ConditionType = "and"
	// ConditionOr requires at least one sub-condition to be true.
	ConditionOr ConditionType = "or"
	// ConditionNot inverts the result of its sub-condition.
	ConditionNot ConditionType = "not"
	// ConditionScript evaluates an embedded script expression.
	ConditionScript ConditionType = "script"
)

// FieldOperator represents comparison operators for field validation
type FieldOperator string

const (
	// OpExists checks that a field is present in the evaluation context.
	OpExists FieldOperator = "exists"
	// OpNotExists checks that a field is absent from the evaluation context.
	OpNotExists FieldOperator = "not_exists"
	// OpEquals checks that a field value equals the specified value.
	OpEquals FieldOperator = "equals"
	// OpNotEquals checks that a field value does not equal the specified value.
	OpNotEquals FieldOperator = "not_equals"
	// OpContains checks that a field value contains the specified substring.
	OpContains FieldOperator = "contains"
	// OpNotContains checks that a field value does not contain the specified substring.
	OpNotContains FieldOperator = "not_contains"
	// OpMatches checks that a field value matches the specified regular expression.
	OpMatches FieldOperator = "matches"
	// OpNotMatches checks that a field value does not match the specified regular expression.
	OpNotMatches FieldOperator = "not_matches"
	// OpGT checks that a field value is greater than the specified value.
	OpGT FieldOperator = "gt"
	// OpGTE checks that a field value is greater than or equal to the specified value.
	OpGTE FieldOperator = "gte"
	// OpLT checks that a field value is less than the specified value.
	OpLT FieldOperator = "lt"
	// OpLTE checks that a field value is less than or equal to the specified value.
	OpLTE FieldOperator = "lte"
	// OpIn checks that a field value is one of the specified values.
	OpIn FieldOperator = "in"
	// OpNotIn checks that a field value is not one of the specified values.
	OpNotIn FieldOperator = "not_in"
)

// GateDefinition represents a single quality gate
type GateDefinition struct {
	ID          string            `yaml:"id"`
	Name        string            `yaml:"name"`
	Description string            `yaml:"description"`
	Enabled     *bool             `yaml:"enabled"`
	Type        GateType          `yaml:"type"`
	Priority    int               `yaml:"priority"`
	Trigger     TriggerDefinition `yaml:"trigger"`
	Rules       []RuleDefinition  `yaml:"rules"`
	Action      ActionDefinition  `yaml:"action"`
}

// TriggerDefinition defines when a gate should be triggered
type TriggerDefinition struct {
	Roles       []string         `yaml:"roles"`
	TaskTypes   []string         `yaml:"task_types"`
	Phases      []string         `yaml:"phases"`
	BloomLevels []int            `yaml:"bloom_levels"`
	Patterns    []PatternTrigger `yaml:"patterns"`
}

// PatternTrigger defines pattern matching for triggers
type PatternTrigger struct {
	Field         string         `yaml:"field"`
	Regex         string         `yaml:"regex"`
	Negate        bool           `yaml:"negate"`
	CompiledRegex *regexp.Regexp `yaml:"-"`
}

// RuleDefinition represents a single validation rule
type RuleDefinition struct {
	ID          string        `yaml:"id"`
	Description string        `yaml:"description"`
	Condition   RuleCondition `yaml:"condition"`
	Severity    Severity      `yaml:"severity"`
}

// RuleCondition represents the condition to evaluate
type RuleCondition struct {
	Type           ConditionType   `yaml:"type"`
	Field          string          `yaml:"field"`
	Operator       FieldOperator   `yaml:"operator"`
	Value          interface{}     `yaml:"value"`
	CaseSensitive  *bool           `yaml:"case_sensitive"`
	Conditions     []RuleCondition `yaml:"conditions"`
	Language       string          `yaml:"language"`
	Script         string          `yaml:"script"`
	TimeoutSeconds int             `yaml:"timeout_seconds"`

	// Compiled fields (not in YAML)
	CompiledRegex  *regexp.Regexp `yaml:"-"`
	CompiledScript interface{}    `yaml:"-"` // Compiled script function
	SourceFile     string         `yaml:"-"` // Source config file path for permission re-verification
}

// IsCaseSensitive returns the effective case sensitivity for field
// validation: true unless the gate author explicitly wrote
// `case_sensitive: false`. The field is a *bool because a plain bool
// cannot distinguish "absent" from "false" — the old defaulting forced
// every gate to case-sensitive matching and made `case_sensitive: false`
// impossible to configure.
func (c *RuleCondition) IsCaseSensitive() bool {
	return c.CaseSensitive == nil || *c.CaseSensitive
}

// ActionDefinition defines actions based on rule evaluation
type ActionDefinition struct {
	OnPass ActionType `yaml:"on_pass"`
	OnFail ActionType `yaml:"on_fail"`
	OnWarn ActionType `yaml:"on_warn"`
}

// GateConfiguration represents the full gate configuration file
type GateConfiguration struct {
	SchemaVersion string           `yaml:"schema_version"`
	Metadata      *GateMetadata    `yaml:"metadata"`
	Gates         []GateDefinition `yaml:"gates"`
}

// GateMetadata represents metadata about the gate configuration
type GateMetadata struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// EvaluationResult represents the result of gate evaluation
type EvaluationResult struct {
	GateID      string
	GateType    GateType
	Passed      bool
	Action      ActionType
	Duration    time.Duration
	CacheHit    bool
	TimedOut    bool
	FailedGates []string
	RuleResults []RuleResult
	Error       error
}

// RuleResult represents the result of a single rule evaluation
type RuleResult struct {
	RuleID   string
	Passed   bool
	Severity Severity
	Message  string
	Duration time.Duration
	Error    error
}

// EvaluationContext provides context data for evaluation
type EvaluationContext interface {
	GetField(path string) (interface{}, bool)
}

// RuleEvaluator evaluates a specific type of rule condition
type RuleEvaluator interface {
	Evaluate(ctx context.Context, condition *RuleCondition, evalCtx EvaluationContext) (bool, error)
}

// cacheKey represents a key for caching evaluation results
type cacheKey struct {
	GateID             string
	GateVersionHash    string
	ContextFingerprint string
}
