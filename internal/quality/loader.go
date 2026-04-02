package quality

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Loader loads and validates gate configurations
type Loader struct {
	configDir string
}

// NewLoader creates a new configuration loader
func NewLoader(configDir string) *Loader {
	return &Loader{
		configDir: configDir,
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

		// Verify file permissions before loading
		if err := validateFilePermissions(path); err != nil {
			return fmt.Errorf("unsafe file permissions on %s: %w", path, err)
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

	// Set source file path on script conditions for permission re-verification
	setSourceFile(&config, path)

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
	case ActionBlock, ActionWarn, "":
		// Valid or will be defaulted
	default:
		return fmt.Errorf("invalid on_fail action: %s", action.OnFail)
	}

	// Validate on_warn action
	switch action.OnWarn {
	case ActionContinue, ActionLog, "":
		// Valid or optional
	default:
		return fmt.Errorf("invalid on_warn action: %s", action.OnWarn)
	}

	return nil
}

// applyDefaults applies default values to the configuration
func (l *Loader) applyDefaults(config *GateConfiguration) {
	for i := range config.Gates {
		gate := &config.Gates[i]

		// Default enabled to true (only if not explicitly set in YAML)
		if gate.Enabled == nil {
			gate.Enabled = boolPtr(true)
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

// setSourceFile sets the SourceFile field on all script conditions in the configuration
func setSourceFile(config *GateConfiguration, path string) {
	for i := range config.Gates {
		for j := range config.Gates[i].Rules {
			setConditionSourceFile(&config.Gates[i].Rules[j].Condition, path)
		}
	}
}

// setConditionSourceFile recursively sets SourceFile on script conditions
func setConditionSourceFile(condition *RuleCondition, path string) {
	if condition.Type == ConditionScript {
		condition.SourceFile = path
	}
	for i := range condition.Conditions {
		setConditionSourceFile(&condition.Conditions[i], path)
	}
}

// boolPtr returns a pointer to the given bool value
func boolPtr(b bool) *bool {
	return &b
}

// validateFilePermissions checks that a config file is not writable by group or others.
// This mitigates command injection via tampered config files (e.g., script conditions).
func validateFilePermissions(path string) error {
	// Lstat first to reject symlinks before opening
	lfi, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("failed to lstat file: %w", err)
	}
	if lfi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("config file must not be a symlink")
	}

	// Open the file and stat via fd to avoid TOCTOU between stat and read
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat file: %w", err)
	}

	// Reject non-regular files
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("config file must be a regular file")
	}

	// Check that group and other write bits are not set (0o022)
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("config file must not be writable by group or others (current permissions: %o)", fi.Mode().Perm())
	}

	return nil
}
