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
	GateTypePostTask          GateType = "post_task"
	GateTypePhaseTransition   GateType = "phase_transition"
	GateTypeCommandValidation GateType = "command_validation"
)

// Severity represents the severity level of a rule failure
type Severity string

const (
	// SeverityInfo is the lowest quality gate severity level.
	SeverityInfo Severity = "info"
	// SeverityWarning indicates a non-blocking concern that should be reviewed.
	SeverityWarning  Severity = "warning"
	SeverityError    Severity = "error"
	SeverityCritical Severity = "critical"
)

// ActionType represents the action to take based on gate evaluation
type ActionType string

const (
	// ActionAllow permits the operation to proceed without interruption.
	ActionAllow ActionType = "allow"
	// ActionLog records the gate result without blocking the operation.
	ActionLog      ActionType = "log"
	ActionBlock    ActionType = "block"
	ActionWarn     ActionType = "warn"
	ActionContinue ActionType = "continue"
)

// ConditionType represents the type of rule condition
type ConditionType string

const (
	// ConditionFieldValidation checks a specific field value against an operator and value.
	ConditionFieldValidation ConditionType = "field_validation"
	// ConditionAnd requires all sub-conditions to be true.
	ConditionAnd             ConditionType = "and"
	ConditionOr              ConditionType = "or"
	ConditionNot             ConditionType = "not"
	ConditionScript          ConditionType = "script"
)

// FieldOperator represents comparison operators for field validation
type FieldOperator string

const (
	// OpExists checks that a field is present in the evaluation context.
	OpExists FieldOperator = "exists"
	// OpNotExists checks that a field is absent from the evaluation context.
	OpNotExists   FieldOperator = "not_exists"
	OpEquals      FieldOperator = "equals"
	OpNotEquals   FieldOperator = "not_equals"
	OpContains    FieldOperator = "contains"
	OpNotContains FieldOperator = "not_contains"
	OpMatches     FieldOperator = "matches"
	OpNotMatches  FieldOperator = "not_matches"
	OpGT          FieldOperator = "gt"
	OpGTE         FieldOperator = "gte"
	OpLT          FieldOperator = "lt"
	OpLTE         FieldOperator = "lte"
	OpIn          FieldOperator = "in"
	OpNotIn       FieldOperator = "not_in"
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
	Roles       []string          `yaml:"roles"`
	TaskTypes   []string          `yaml:"task_types"`
	Phases      []string          `yaml:"phases"`
	BloomLevels []int             `yaml:"bloom_levels"`
	Patterns    []PatternTrigger  `yaml:"patterns"`
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
	Type           ConditionType    `yaml:"type"`
	Field          string           `yaml:"field"`
	Operator       FieldOperator    `yaml:"operator"`
	Value          interface{}      `yaml:"value"`
	CaseSensitive  bool             `yaml:"case_sensitive"`
	Conditions     []RuleCondition  `yaml:"conditions"`
	Language       string           `yaml:"language"`
	Script         string           `yaml:"script"`
	TimeoutSeconds int              `yaml:"timeout_seconds"`

	// Compiled fields (not in YAML)
	CompiledRegex  interface{} `yaml:"-"` // *regexp.Regexp after compilation
	CompiledScript interface{} `yaml:"-"` // Compiled script function
	SourceFile     string      `yaml:"-"` // Source config file path for permission re-verification
}

// ActionDefinition defines actions based on rule evaluation
type ActionDefinition struct {
	OnPass           ActionType       `yaml:"on_pass"`
	OnFail           ActionType       `yaml:"on_fail"`
	OnWarn           ActionType       `yaml:"on_warn"`
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
	GateID           string
	GateVersionHash  string
	ContextFingerprint string
}