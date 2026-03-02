package quality

import (
	"context"
	"time"
)

// GateType represents the type of quality gate
type GateType string

const (
	GateTypePreTask           GateType = "pre_task"
	GateTypePostTask          GateType = "post_task"
	GateTypePhaseTransition   GateType = "phase_transition"
	GateTypeCommandValidation GateType = "command_validation"
)

// Severity represents the severity level of a rule failure
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityError    Severity = "error"
	SeverityCritical Severity = "critical"
)

// ActionType represents the action to take based on gate evaluation
type ActionType string

const (
	ActionAllow    ActionType = "allow"
	ActionLog      ActionType = "log"
	ActionBlock    ActionType = "block"
	ActionWarn     ActionType = "warn"
	ActionRetry    ActionType = "retry"
	ActionRollback ActionType = "rollback"
	ActionEscalate ActionType = "escalate"
	ActionContinue ActionType = "continue"
	ActionPrompt   ActionType = "prompt"
)

// ConditionType represents the type of rule condition
type ConditionType string

const (
	ConditionFieldValidation ConditionType = "field_validation"
	ConditionAnd             ConditionType = "and"
	ConditionOr              ConditionType = "or"
	ConditionNot             ConditionType = "not"
	ConditionResourceLimit   ConditionType = "resource_limit"
	ConditionDependencyCheck ConditionType = "dependency_check"
	ConditionScript          ConditionType = "script"
)

// FieldOperator represents comparison operators for field validation
type FieldOperator string

const (
	OpExists      FieldOperator = "exists"
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
	Enabled     bool              `yaml:"enabled"`
	Type        GateType          `yaml:"type"`
	Priority    int               `yaml:"priority"`
	Trigger     TriggerDefinition `yaml:"trigger"`
	Rules       []RuleDefinition  `yaml:"rules"`
	Action      ActionDefinition  `yaml:"action"`
	Metrics     MetricsDefinition `yaml:"metrics"`
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
	Field  string `yaml:"field"`
	Regex  string `yaml:"regex"`
	Negate bool   `yaml:"negate"`
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
	Resource       string           `yaml:"resource"`
	Limit          float64          `yaml:"limit"`
	Scope          string           `yaml:"scope"`
	Mode           string           `yaml:"mode"`
	Dependencies   []string         `yaml:"dependencies"`
	Language       string           `yaml:"language"`
	Script         string           `yaml:"script"`
	TimeoutSeconds int              `yaml:"timeout_seconds"`

	// Compiled fields (not in YAML)
	CompiledRegex  interface{} `yaml:"-"` // *regexp.Regexp after compilation
	CompiledScript interface{} `yaml:"-"` // Compiled script function
}

// ActionDefinition defines actions based on rule evaluation
type ActionDefinition struct {
	OnPass           ActionType       `yaml:"on_pass"`
	OnFail           ActionType       `yaml:"on_fail"`
	OnWarn           ActionType       `yaml:"on_warn"`
	RetryConfig      *RetryConfig     `yaml:"retry_config"`
	RollbackConfig   *RollbackConfig  `yaml:"rollback_config"`
	NotificationConfig *NotificationConfig `yaml:"notification"`
}

// RetryConfig defines retry behavior
type RetryConfig struct {
	MaxAttempts        int  `yaml:"max_attempts"`
	DelaySeconds       int  `yaml:"delay_seconds"`
	ExponentialBackoff bool `yaml:"exponential_backoff"`
}

// RollbackConfig defines rollback behavior
type RollbackConfig struct {
	Target       string `yaml:"target"`
	PreserveLogs bool   `yaml:"preserve_logs"`
}

// NotificationConfig defines notification settings
type NotificationConfig struct {
	Channels       []string          `yaml:"channels"`
	WebhookURL     string            `yaml:"webhook_url"`
	IncludeContext bool              `yaml:"include_context"`
}

// MetricsDefinition defines metrics collection settings
type MetricsDefinition struct {
	Enabled       bool              `yaml:"enabled"`
	Tags          map[string]string `yaml:"tags"`
	TrackDuration bool              `yaml:"track_duration"`
	TrackFailures bool              `yaml:"track_failures"`
}

// GateConfiguration represents the full gate configuration file
type GateConfiguration struct {
	SchemaVersion string           `yaml:"schema_version"`
	Metadata      *GateMetadata    `yaml:"metadata"`
	Gates         []GateDefinition `yaml:"gates"`
}

// GateMetadata represents metadata about the gate configuration
type GateMetadata struct {
	Name        string    `yaml:"name"`
	Description string    `yaml:"description"`
	Author      string    `yaml:"author"`
	CreatedAt   time.Time `yaml:"created_at"`
	UpdatedAt   time.Time `yaml:"updated_at"`
	Tags        []string  `yaml:"tags"`
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
	GetResource(metric string, scope string) (float64, error)
	GetDependencies(mode string) ([]string, error)
}

// RuleEvaluator evaluates a specific type of rule condition
type RuleEvaluator interface {
	Evaluate(ctx context.Context, condition *RuleCondition, evalCtx EvaluationContext) (bool, error)
}

// CacheKey represents a key for caching evaluation results
type CacheKey struct {
	GateID           string
	GateVersionHash  string
	ContextFingerprint string
}

// CacheEntry represents a cached evaluation result
type CacheEntry struct {
	Result    *EvaluationResult
	ExpiresAt time.Time
}