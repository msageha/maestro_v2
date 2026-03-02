package quality

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// Loader loads and validates gate configurations
type Loader struct {
	configDir string
	mu              sync.RWMutex
	configurations  map[string]*GateConfiguration
	loadedFiles     map[string]time.Time
	defaultGates    *GateConfiguration
	validationCache map[string]*regexp.Regexp
}

// NewLoader creates a new configuration loader
func NewLoader(configDir string) *Loader {
	return &Loader{
		configDir:       configDir,
		configurations:  make(map[string]*GateConfiguration),
		loadedFiles:     make(map[string]time.Time),
		validationCache: make(map[string]*regexp.Regexp),
	}
}

// LoadConfiguration loads gate configurations from the specified directory
func (l *Loader) LoadConfiguration() (*GateConfiguration, error) {
	config := &GateConfiguration{
		SchemaVersion: "1.0.0",
		Gates:         []GateDefinition{},
	}

	// Load all YAML files from the gates directory
	gatesDir := filepath.Join(l.configDir, "quality_gates")
	if _, err := os.Stat(gatesDir); os.IsNotExist(err) {
		// Directory doesn't exist, return empty configuration
		return config, nil
	}

	// Walk through the directory and load all YAML files
	err := filepath.Walk(gatesDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Skip directories and non-YAML files
		if info.IsDir() || (!hasExtension(path, ".yaml") && !hasExtension(path, ".yml")) {
			return nil
		}

		// Load the file
		fileConfig, err := l.loadFile(path)
		if err != nil {
			return fmt.Errorf("failed to load %s: %w", path, err)
		}

		// Merge gates
		config.Gates = append(config.Gates, fileConfig.Gates...)

		// Use the first file's metadata if not set
		if config.Metadata == nil && fileConfig.Metadata != nil {
			config.Metadata = fileConfig.Metadata
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	// Validate the configuration
	if err := l.validateConfiguration(config); err != nil {
		return nil, fmt.Errorf("configuration validation failed: %w", err)
	}

	// Apply defaults
	l.applyDefaults(config)

	return config, nil
}

// loadFile loads a single configuration file
func (l *Loader) loadFile(path string) (*GateConfiguration, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var config GateConfiguration
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse YAML: %w", err)
	}

	return &config, nil
}

// validateConfiguration validates the gate configuration
func (l *Loader) validateConfiguration(config *GateConfiguration) error {
	// Check schema version
	if config.SchemaVersion == "" {
		return fmt.Errorf("schema_version is required")
	}
	if config.SchemaVersion != "1.0.0" {
		return fmt.Errorf("unsupported schema version: %s", config.SchemaVersion)
	}

	// Validate each gate
	gateIDs := make(map[string]bool)
	for i, gate := range config.Gates {
		// Check for duplicate IDs
		if gateIDs[gate.ID] {
			return fmt.Errorf("duplicate gate ID: %s", gate.ID)
		}
		gateIDs[gate.ID] = true

		// Validate required fields
		if gate.ID == "" {
			return fmt.Errorf("gate %d: missing ID", i)
		}
		if gate.Name == "" {
			return fmt.Errorf("gate %s: missing name", gate.ID)
		}
		if gate.Type == "" {
			return fmt.Errorf("gate %s: missing type", gate.ID)
		}

		// Validate gate type
		switch gate.Type {
		case GateTypePreTask, GateTypePostTask, GateTypePhaseTransition, GateTypeCommandValidation:
			// Valid
		default:
			return fmt.Errorf("gate %s: invalid type: %s", gate.ID, gate.Type)
		}

		// Validate priority
		if gate.Priority != 0 && (gate.Priority < 1 || gate.Priority > 100) {
			return fmt.Errorf("gate %s: priority must be between 1 and 100", gate.ID)
		}

		// Validate rules
		if len(gate.Rules) == 0 {
			return fmt.Errorf("gate %s: must have at least one rule", gate.ID)
		}

		for j, rule := range gate.Rules {
			if rule.ID == "" {
				return fmt.Errorf("gate %s, rule %d: missing ID", gate.ID, j)
			}

			// Validate condition
			if err := l.validateCondition(&rule.Condition); err != nil {
				return fmt.Errorf("gate %s, rule %s: %w", gate.ID, rule.ID, err)
			}

			// Validate severity
			switch rule.Severity {
			case SeverityInfo, SeverityWarning, SeverityError, SeverityCritical, "":
				// Valid or will be defaulted
			default:
				return fmt.Errorf("gate %s, rule %s: invalid severity: %s", gate.ID, rule.ID, rule.Severity)
			}
		}

		// Validate actions
		if err := l.validateAction(&gate.Action); err != nil {
			return fmt.Errorf("gate %s: %w", gate.ID, err)
		}
	}

	return nil
}

// validateCondition validates a rule condition
func (l *Loader) validateCondition(condition *RuleCondition) error {
	// Validate condition type
	switch condition.Type {
	case ConditionFieldValidation:
		if condition.Field == "" {
			return fmt.Errorf("field validation condition requires field")
		}
		// Validate operator
		switch condition.Operator {
		case OpExists, OpNotExists, OpEquals, OpNotEquals, OpContains, OpNotContains,
			OpMatches, OpNotMatches, OpGT, OpGTE, OpLT, OpLTE, OpIn, OpNotIn, "":
			// Valid or will be defaulted
		default:
			return fmt.Errorf("invalid operator: %s", condition.Operator)
		}

	case ConditionAnd, ConditionOr:
		if len(condition.Conditions) == 0 {
			return fmt.Errorf("%s condition requires sub-conditions", condition.Type)
		}
		// Recursively validate sub-conditions
		for _, subCond := range condition.Conditions {
			if err := l.validateCondition(&subCond); err != nil {
				return err
			}
		}

	case ConditionNot:
		if len(condition.Conditions) != 1 {
			return fmt.Errorf("NOT condition must have exactly one sub-condition")
		}
		if err := l.validateCondition(&condition.Conditions[0]); err != nil {
			return err
		}

	case ConditionResourceLimit:
		if condition.Resource == "" {
			return fmt.Errorf("resource limit condition requires resource")
		}
		if condition.Limit <= 0 {
			return fmt.Errorf("resource limit condition requires positive limit value")
		}

	case ConditionDependencyCheck:
		if condition.Mode == "" {
			return fmt.Errorf("dependency check condition requires mode")
		}

	case ConditionScript:
		if condition.Script == "" {
			return fmt.Errorf("script condition requires script")
		}

	default:
		return fmt.Errorf("unknown condition type: %s", condition.Type)
	}

	return nil
}

// validateAction validates action configuration
func (l *Loader) validateAction(action *ActionDefinition) error {
	// Validate on_pass action
	switch action.OnPass {
	case ActionAllow, ActionLog, "":
		// Valid or will be defaulted
	default:
		return fmt.Errorf("invalid on_pass action: %s", action.OnPass)
	}

	// Validate on_fail action
	switch action.OnFail {
	case ActionBlock, ActionWarn, ActionRetry, ActionRollback, ActionEscalate, "":
		// Valid or will be defaulted
	default:
		return fmt.Errorf("invalid on_fail action: %s", action.OnFail)
	}

	// Validate on_warn action
	switch action.OnWarn {
	case ActionContinue, ActionPrompt, ActionLog, "":
		// Valid or optional
	default:
		return fmt.Errorf("invalid on_warn action: %s", action.OnWarn)
	}

	// Validate retry config if action is retry
	if action.OnFail == ActionRetry && action.RetryConfig != nil {
		if action.RetryConfig.MaxAttempts < 1 || action.RetryConfig.MaxAttempts > 5 {
			return fmt.Errorf("retry max_attempts must be between 1 and 5")
		}
		if action.RetryConfig.DelaySeconds < 1 || action.RetryConfig.DelaySeconds > 60 {
			return fmt.Errorf("retry delay_seconds must be between 1 and 60")
		}
	}

	return nil
}

// applyDefaults applies default values to the configuration
func (l *Loader) applyDefaults(config *GateConfiguration) {
	for i := range config.Gates {
		gate := &config.Gates[i]

		// Default enabled to true
		if !gate.Enabled {
			gate.Enabled = true
		}

		// Default priority
		if gate.Priority == 0 {
			gate.Priority = 50
		}

		// Default rule severity
		for j := range gate.Rules {
			rule := &gate.Rules[j]
			if rule.Severity == "" {
				rule.Severity = SeverityError
			}

			// Default condition values
			condition := &rule.Condition
			l.applyConditionDefaults(condition)
		}

		// Default actions
		if gate.Action.OnPass == "" {
			gate.Action.OnPass = ActionAllow
		}
		if gate.Action.OnFail == "" {
			gate.Action.OnFail = ActionBlock
		}
		if gate.Action.OnWarn == "" {
			gate.Action.OnWarn = ActionContinue
		}

		// Default retry config
		if gate.Action.OnFail == ActionRetry && gate.Action.RetryConfig == nil {
			gate.Action.RetryConfig = &RetryConfig{
				MaxAttempts:        3,
				DelaySeconds:       5,
				ExponentialBackoff: false,
			}
		}

		// Default metrics
		if gate.Metrics.Enabled {
			if gate.Metrics.Tags == nil {
				gate.Metrics.Tags = make(map[string]string)
			}
		}
	}
}

// applyConditionDefaults applies defaults to a condition
func (l *Loader) applyConditionDefaults(condition *RuleCondition) {
	// Default case sensitivity
	if condition.Type == ConditionFieldValidation && !condition.CaseSensitive {
		condition.CaseSensitive = true
	}

	// Default script language
	if condition.Type == ConditionScript && condition.Language == "" {
		condition.Language = "bash"
	}

	// Default script timeout
	if condition.Type == ConditionScript && condition.TimeoutSeconds == 0 {
		condition.TimeoutSeconds = 30
	}

	// Default resource scope
	if condition.Type == ConditionResourceLimit && condition.Scope == "" {
		condition.Scope = "task"
	}

	// Recursively apply to sub-conditions
	for i := range condition.Conditions {
		l.applyConditionDefaults(&condition.Conditions[i])
	}
}

// hasExtension checks if a file has one of the specified extensions
func hasExtension(path string, extensions ...string) bool {
	ext := filepath.Ext(path)
	for _, e := range extensions {
		if ext == e {
			return true
		}
	}
	return false
}

// LoadFromFile loads gate definitions from a YAML file with caching
func (l *Loader) LoadFromFile(path string) (*GateConfiguration, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	// Check if file exists
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("failed to stat file %s: %w", path, err)
	}

	// Check if already loaded and not modified
	if lastLoad, exists := l.loadedFiles[path]; exists {
		if info.ModTime().Before(lastLoad) || info.ModTime().Equal(lastLoad) {
			if config, ok := l.configurations[path]; ok {
				return config, nil
			}
		}
	}

	// Read file
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open file %s: %w", path, err)
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("failed to read file %s: %w", path, err)
	}

	// Parse YAML
	config, err := l.parseYAML(data)
	if err != nil {
		return nil, fmt.Errorf("failed to parse YAML from %s: %w", path, err)
	}

	// Validate configuration
	if err := l.validateConfiguration(config); err != nil {
		return nil, fmt.Errorf("invalid configuration in %s: %w", path, err)
	}

	// Apply defaults
	l.applyDefaults(config)

	// Compile patterns
	if err := l.compilePatterns(config); err != nil {
		return nil, fmt.Errorf("failed to compile patterns in %s: %w", path, err)
	}

	// Store in cache
	l.configurations[path] = config
	l.loadedFiles[path] = time.Now()

	return config, nil
}

// LoadFromBytes loads gate definitions from YAML bytes
func (l *Loader) LoadFromBytes(data []byte) (*GateConfiguration, error) {
	config, err := l.parseYAML(data)
	if err != nil {
		return nil, fmt.Errorf("failed to parse YAML: %w", err)
	}

	if err := l.validateConfiguration(config); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	l.applyDefaults(config)

	if err := l.compilePatterns(config); err != nil {
		return nil, fmt.Errorf("failed to compile patterns: %w", err)
	}

	return config, nil
}

// parseYAML parses YAML data into a GateConfiguration
func (l *Loader) parseYAML(data []byte) (*GateConfiguration, error) {
	var config GateConfiguration

	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true) // Strict mode - fail on unknown fields

	if err := decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("YAML decode error: %w", err)
	}

	return &config, nil
}

// ReloadFile reloads a specific file if it has been modified
func (l *Loader) ReloadFile(path string) (*GateConfiguration, bool, error) {
	l.mu.RLock()
	lastLoad, exists := l.loadedFiles[path]
	l.mu.RUnlock()

	if !exists {
		// File not previously loaded, load it now
		config, err := l.LoadFromFile(path)
		return config, true, err
	}

	info, err := os.Stat(path)
	if err != nil {
		return nil, false, fmt.Errorf("failed to stat file %s: %w", path, err)
	}

	// Check if file has been modified
	if info.ModTime().After(lastLoad) {
		config, err := l.LoadFromFile(path)
		return config, true, err
	}

	l.mu.RLock()
	config := l.configurations[path]
	l.mu.RUnlock()

	return config, false, nil
}

// LoadDirectory loads all YAML files from a directory
func (l *Loader) LoadDirectory(dir string) ([]*GateConfiguration, error) {
	pattern := filepath.Join(dir, "*.yaml")
	files, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("failed to glob directory %s: %w", dir, err)
	}

	// Also check for .yml files
	ymlPattern := filepath.Join(dir, "*.yml")
	ymlFiles, err := filepath.Glob(ymlPattern)
	if err != nil {
		return nil, fmt.Errorf("failed to glob yml files in %s: %w", dir, err)
	}
	files = append(files, ymlFiles...)

	var configs []*GateConfiguration
	for _, file := range files {
		config, err := l.LoadFromFile(file)
		if err != nil {
			// Log error but continue loading other files
			fmt.Fprintf(os.Stderr, "warning: failed to load %s: %v\n", file, err)
			continue
		}
		configs = append(configs, config)
	}

	return configs, nil
}

// GetDefaultGates returns the default gate configuration
func (l *Loader) GetDefaultGates() *GateConfiguration {
	l.mu.RLock()
	defer l.mu.RUnlock()

	if l.defaultGates != nil {
		return l.defaultGates
	}

	// Return embedded default gates
	return l.loadDefaultGates()
}

// loadDefaultGates loads the embedded default gate configuration
func (l *Loader) loadDefaultGates() *GateConfiguration {
	return &GateConfiguration{
		SchemaVersion: "1.0.0",
		Metadata: &GateMetadata{
			Name:        "Default Gates",
			Description: "Default quality gates for Maestro",
			Author:      "Maestro System",
			CreatedAt:   time.Now(),
		},
		Gates: []GateDefinition{
			{
				ID:          "default_required_fields",
				Name:        "Required Fields Check",
				Description: "Ensures required fields are present",
				Enabled:     true,
				Type:        GateTypePreTask,
				Priority:    10,
				Trigger:     TriggerDefinition{},
				Rules: []RuleDefinition{
					{
						ID:          "check_task_id",
						Description: "Task must have an ID",
						Condition: RuleCondition{
							Type:     ConditionFieldValidation,
							Field:    "task.id",
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
}

// compilePatterns compiles regex patterns in the configuration
func (l *Loader) compilePatterns(config *GateConfiguration) error {
	for i := range config.Gates {
		gate := &config.Gates[i]

		// Compile trigger patterns
		for j := range gate.Trigger.Patterns {
			pattern := &gate.Trigger.Patterns[j]
			if pattern.Regex != "" {
				re, err := l.getOrCompileRegex(pattern.Regex)
				if err != nil {
					return fmt.Errorf("gate[%s]: invalid trigger pattern regex: %w", gate.ID, err)
				}
				// Store compiled regex (would need to add field to struct)
				_ = re
			}
		}

		// Compile rule conditions
		for j := range gate.Rules {
			rule := &gate.Rules[j]
			if err := l.compileCondition(&rule.Condition, gate.ID, rule.ID); err != nil {
				return err
			}
		}
	}

	return nil
}

// compileCondition compiles patterns in a condition recursively
func (l *Loader) compileCondition(condition *RuleCondition, gateID, ruleID string) error {
	switch condition.Type {
	case ConditionFieldValidation:
		if condition.Operator == OpMatches || condition.Operator == OpNotMatches {
			if str, ok := condition.Value.(string); ok {
				re, err := l.getOrCompileRegex(str)
				if err != nil {
					return fmt.Errorf("gate[%s].rule[%s]: invalid regex pattern: %w", gateID, ruleID, err)
				}
				condition.CompiledRegex = re
			}
		}

	case ConditionAnd, ConditionOr, ConditionNot:
		for i := range condition.Conditions {
			if err := l.compileCondition(&condition.Conditions[i], gateID, fmt.Sprintf("%s.%d", ruleID, i)); err != nil {
				return err
			}
		}
	}

	return nil
}

// getOrCompileRegex compiles and caches regex patterns
func (l *Loader) getOrCompileRegex(pattern string) (*regexp.Regexp, error) {
	if cached, exists := l.validationCache[pattern]; exists {
		return cached, nil
	}

	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}

	l.validationCache[pattern] = re
	return re, nil
}