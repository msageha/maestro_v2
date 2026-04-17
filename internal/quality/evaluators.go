package quality

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	// maxScriptLength is the maximum allowed script length in bytes (4KB).
	maxScriptLength = 4 * 1024

	// defaultScriptTimeout is the default timeout for script condition evaluation.
	defaultScriptTimeout = 30 * time.Second
)

// Dangerous pattern categories: each group contains raw regex strings that
// match shell/interpreter commands which must never be executed by the
// quality-gate script evaluator.
var (
	// privilegeEscalationPatterns match commands that escalate privileges.
	privilegeEscalationPatterns = []string{
		`(?i)\b(?:sudo|su|doas|pkexec)\b`,
		`(?i)\bchmod\s+(?:\+s|u\+s|[0-7]*4[0-7]{3})\b`, // setuid bit
		`(?i)\bchown\s+(?:root\b|0(?::|0|\s))`,         // ownership to root
	}

	// destructiveCommandPatterns match commands that destroy files, disks, or system state.
	destructiveCommandPatterns = []string{
		`(?i)\brm\s+-[^\n;|&]*\brf?\b`,                                  // recursive force remove
		`(?i)\b(?:mkfs(?:\.\w+)?|fdisk|parted|wipefs)\b`,                // disk-level destructive
		`(?i)\bdd\s+[^;\n]*\bof=/dev/`,                                  // raw disk write
		`(?i)\b(?:shutdown|reboot|poweroff|halt|init\s+[06])\b`,         // system shutdown/reboot
		`(?i)\bkill\s+-9\s+-1\b`,                                        // mass kill all processes
		`(?i)\b(?:killall|pkill)\b`,                                     // mass process kill
		`(?i)(?:>|>>|\btee\b)\s*/etc/(?:sudoers|passwd|shadow|group)\b`, // critical system files
	}

	// remoteCodeExecutionPatterns match commands that fetch and execute remote code
	// or open reverse shells.
	remoteCodeExecutionPatterns = []string{
		`(?i)\b(?:curl|wget)\b[^|\n]*\|\s*(?:bash|sh|zsh)\b`, // pipe download to shell
		`(?i)\|\s*(?:(?:/(?:usr/)?bin/)?(?:ba)?sh)\b`,        // generic pipe to shell
		`(?i)(?:^|[;\r\n|&])\s*/(?:usr/)?bin/(?:ba)?sh\b`,    // absolute path shell invocation
		`(?i)\b(?:nc|ncat|netcat|socat)\b[^;\n]*\s-e\s`,      // reverse shell via netcat
		`(?i)\b(?:bash|sh)\b[^;\n]*/dev/(?:tcp|udp)/`,        // reverse shell via /dev/tcp
		`:\(\)\s*\{\s*:\|:\s*&\s*\};:`,                       // fork bomb
	}

	// bypassPreventionPatterns match obfuscation and restricted-mode bypass techniques.
	bypassPreventionPatterns = []string{
		// Payload decoding
		`(?i)\b(?:base64|openssl\s+base64)\b[^\n;]*(?:-d|--decode)`,
		// eval with variable/substitution (also catches quoted eval like 'eval' $VAR)
		`(?i)\b(?:eval|builtin\s+eval|command\s+eval)\b['"]*\s+\S`,
		// Heredoc into shell or interpreter
		`(?i)\b(?:bash|sh|zsh|dash|ksh|python[0-9.]*|perl|ruby|node)\b[^\n;|&]*<<-?\s*`,
		// Inline interpreter code execution
		`(?i)\bpython[0-9.]*\b[^\n;|&]*\s+-c\b`,
		`(?i)\bperl\b[^\n;|&]*\s+-e\b`,
		`(?i)\bruby\b[^\n;|&]*\s+-e\b`,
		`(?i)\bnode(?:js)?\b[^\n;|&]*\s+(?:-e|--eval)\b`,
		`(?i)\bphp\b[^\n;|&]*\s+-r\b`,
		`(?i)\blua\b[^\n;|&]*\s+-e\b`,
		// Shell variable expansion / obfuscation
		`\$\{!`,                                 // indirect variable expansion (${!var})
		`\$\{[^}]+:\s*[0-9]`,                    // substring extraction (${var:0:1}, ${var: 0:1})
		`(?i)\bprintf\b[^\n;|&]*\\x[0-9a-fA-F]`, // printf with hex escapes
		`(?i)\bexport\s+PATH\b`,                 // PATH override
		`(?i)(?:^|[;\r\n|&])\s*source\s+\S`,     // source external scripts
		`(?:^|[;\r\n|&]\s*)\.\s+\S`,             // dot-source (. /path)
		// Unrestricted shell spawning (bypasses bash --restricted)
		`(?i)\bsh\b[^\n;|&]*\s+-c\b`,
		`(?i)\bdash\b[^\n;|&]*\s+-c\b`,
		`(?i)\bksh\b[^\n;|&]*\s+-c\b`,
		`(?i)\bzsh\b[^\n;|&]*\s+-c\b`,
		`(?i)\bbash\b[^\n;|&]*\s+-c\b`,
		// Interpreter system() calls
		`(?i)\bawk\b[^\n;|&]*\bsystem\s*\(`,
		`(?i)\bgawk\b[^\n;|&]*\bsystem\s*\(`,
		`(?i)\bmawk\b[^\n;|&]*\bsystem\s*\(`,
	}

	// pythonDangerousPatterns match Python-specific dangerous constructs.
	pythonDangerousPatterns = []string{
		`(?i)\bos\.(?:system|popen|exec[lv]*[pe]*)\s*\(`, // os.system(), os.popen(), os.exec*()
		`(?i)\bsubprocess\b`,                             // subprocess module
		`(?i)\b__import__\s*\(`,                          // dynamic import
		`(?i)\bimportlib\b`,                              // importlib
		`(?i)\beval\s*\(`,                                // eval builtin
		`(?i)\bexec\s*\(`,                                // exec builtin
		`(?i)\bsocket\.socket\s*\(`,                      // raw socket creation
		`(?i)\bctypes\b`,                                 // ctypes FFI
	}
)

// dangerousPatterns are compiled regexps that match shell commands which
// must never be executed by the quality-gate script evaluator.
// Compiled once at init time via sync.Once.
var dangerousPatterns []*regexp.Regexp

// Language-specific compiled pattern sets for validateScriptForLanguage.
var (
	commonDangerousPatterns []*regexp.Regexp // privilege escalation, destructive, RCE
	shellBypassPatterns     []*regexp.Regexp // shell-specific bypass prevention
	pythonSpecificPatterns  []*regexp.Regexp // Python-specific dangers
)

var dangerousPatternsOnce sync.Once

// compilePatterns compiles a slice of raw regex strings into []*regexp.Regexp.
func compilePatterns(raw []string) []*regexp.Regexp {
	patterns := make([]*regexp.Regexp, 0, len(raw))
	for _, r := range raw {
		patterns = append(patterns, regexp.MustCompile(r))
	}
	return patterns
}

func initDangerousPatterns() {
	dangerousPatternsOnce.Do(func() {
		// Common patterns (applicable to all languages)
		commonRaw := make([]string, 0, len(privilegeEscalationPatterns)+len(destructiveCommandPatterns)+len(remoteCodeExecutionPatterns))
		commonRaw = append(commonRaw, privilegeEscalationPatterns...)
		commonRaw = append(commonRaw, destructiveCommandPatterns...)
		commonRaw = append(commonRaw, remoteCodeExecutionPatterns...)
		commonDangerousPatterns = compilePatterns(commonRaw)

		// Shell-specific bypass prevention patterns
		shellBypassPatterns = compilePatterns(bypassPreventionPatterns)

		// Python-specific patterns
		pythonSpecificPatterns = compilePatterns(pythonDangerousPatterns)

		// Combined list (backward compatibility for validateScript)
		dangerousPatterns = make([]*regexp.Regexp, 0, len(commonDangerousPatterns)+len(shellBypassPatterns)+len(pythonSpecificPatterns))
		dangerousPatterns = append(dangerousPatterns, commonDangerousPatterns...)
		dangerousPatterns = append(dangerousPatterns, shellBypassPatterns...)
		dangerousPatterns = append(dangerousPatterns, pythonSpecificPatterns...)
	})
}

func init() {
	initDangerousPatterns()
}

// fieldValidationEvaluator evaluates field validation conditions
type fieldValidationEvaluator struct{}

// Evaluate checks if a field meets the specified condition
func (e *fieldValidationEvaluator) Evaluate(_ context.Context, condition *RuleCondition, evalCtx EvaluationContext) (bool, error) {
	value, exists := evalCtx.GetField(condition.Field)

	switch condition.Operator {
	case OpExists:
		return e.evalExists(value, exists), nil
	case OpNotExists:
		return e.evalNotExists(value, exists), nil
	case OpEquals:
		return e.evalEquals(value, condition.Value, condition.CaseSensitive, exists), nil
	case OpNotEquals:
		return e.evalNotEquals(value, condition.Value, condition.CaseSensitive, exists), nil
	case OpContains:
		return e.evalContains(value, condition.Value, condition.CaseSensitive, exists), nil
	case OpNotContains:
		return e.evalNotContains(value, condition.Value, condition.CaseSensitive, exists), nil
	case OpMatches, OpNotMatches:
		return e.evalMatches(value, condition, exists)
	case OpGT, OpGTE, OpLT, OpLTE:
		if !exists {
			return false, nil
		}
		return e.compareNumeric(value, condition.Value, condition.Operator)
	case OpIn:
		return e.evalIn(value, condition.Value, condition.CaseSensitive, exists), nil
	case OpNotIn:
		return e.evalNotIn(value, condition.Value, condition.CaseSensitive, exists), nil
	default:
		return false, fmt.Errorf("unknown operator: %s", condition.Operator)
	}
}

func (e *fieldValidationEvaluator) evalExists(value interface{}, exists bool) bool {
	return exists && value != nil && value != ""
}

func (e *fieldValidationEvaluator) evalNotExists(value interface{}, exists bool) bool {
	return !exists || value == nil || value == ""
}

func (e *fieldValidationEvaluator) evalEquals(value, target interface{}, caseSensitive, exists bool) bool {
	if !exists {
		return false
	}
	return e.compareValues(value, target, caseSensitive)
}

func (e *fieldValidationEvaluator) evalNotEquals(value, target interface{}, caseSensitive, exists bool) bool {
	if !exists {
		return true
	}
	return !e.compareValues(value, target, caseSensitive)
}

func (e *fieldValidationEvaluator) evalContains(value, target interface{}, caseSensitive, exists bool) bool {
	if !exists {
		return false
	}
	return e.containsValue(value, target, caseSensitive)
}

func (e *fieldValidationEvaluator) evalNotContains(value, target interface{}, caseSensitive, exists bool) bool {
	if !exists {
		return true
	}
	return !e.containsValue(value, target, caseSensitive)
}

func (e *fieldValidationEvaluator) evalMatches(value interface{}, condition *RuleCondition, exists bool) (bool, error) {
	if !exists {
		return condition.Operator == OpNotMatches, nil
	}

	if condition.CompiledRegex == nil {
		return false, fmt.Errorf("regex not pre-compiled for operator %s; this indicates a missing compileCondition call", condition.Operator)
	}

	matched := condition.CompiledRegex.MatchString(fmt.Sprintf("%v", value))
	if condition.Operator == OpMatches {
		return matched, nil
	}
	return !matched, nil
}

func (e *fieldValidationEvaluator) evalIn(value, list interface{}, caseSensitive, exists bool) bool {
	if !exists {
		return false
	}
	return e.isInList(value, list, caseSensitive)
}

func (e *fieldValidationEvaluator) evalNotIn(value, list interface{}, caseSensitive, exists bool) bool {
	if !exists {
		return true
	}
	return !e.isInList(value, list, caseSensitive)
}

// compareValues compares two values for equality with type-aware comparison.
// Numeric types are compared numerically, booleans are compared directly,
// and all other types fall back to string comparison.
func (e *fieldValidationEvaluator) compareValues(a, b interface{}, caseSensitive bool) bool {
	// Numeric comparison: avoids false mismatches like 1 (int) vs 1.0 (float64)
	if aNum, aErr := toFloat64(a); aErr == nil {
		if bNum, bErr := toFloat64(b); bErr == nil {
			return aNum == bNum
		}
	}

	// Boolean comparison: avoids "true" == true via string coercion
	if aBool, aOk := a.(bool); aOk {
		if bBool, bOk := b.(bool); bOk {
			return aBool == bBool
		}
	}

	// Fall back to string comparison
	aStr := fmt.Sprintf("%v", a)
	bStr := fmt.Sprintf("%v", b)

	if !caseSensitive {
		aStr = strings.ToLower(aStr)
		bStr = strings.ToLower(bStr)
	}

	return aStr == bStr
}

// containsValue checks if a contains b
func (e *fieldValidationEvaluator) containsValue(a, b interface{}, caseSensitive bool) bool {
	aStr := fmt.Sprintf("%v", a)
	bStr := fmt.Sprintf("%v", b)

	if !caseSensitive {
		aStr = strings.ToLower(aStr)
		bStr = strings.ToLower(bStr)
	}

	return strings.Contains(aStr, bStr)
}

// compareNumeric performs numeric comparison
func (e *fieldValidationEvaluator) compareNumeric(a, b interface{}, op FieldOperator) (bool, error) {
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
func (e *fieldValidationEvaluator) isInList(value, list interface{}, caseSensitive bool) bool {
	// Handle different list types
	switch l := list.(type) {
	case []interface{}:
		for _, item := range l {
			if e.compareValues(value, item, caseSensitive) {
				return true
			}
		}
	case []string:
		valStr := fmt.Sprintf("%v", value)
		if !caseSensitive {
			valStr = strings.ToLower(valStr)
		}
		for _, item := range l {
			itemStr := item
			if !caseSensitive {
				itemStr = strings.ToLower(itemStr)
			}
			if valStr == itemStr {
				return true
			}
		}
	default:
		// Try to parse as comma-separated string
		listStr := fmt.Sprintf("%v", list)
		items := strings.Split(listStr, ",")
		valStr := strings.TrimSpace(fmt.Sprintf("%v", value))
		if !caseSensitive {
			valStr = strings.ToLower(valStr)
		}
		for _, item := range items {
			itemStr := strings.TrimSpace(item)
			if !caseSensitive {
				itemStr = strings.ToLower(itemStr)
			}
			if valStr == itemStr {
				return true
			}
		}
	}
	return false
}

// logicalAndEvaluator evaluates AND conditions
type logicalAndEvaluator struct {
	engine *Engine
}

// Evaluate checks if all sub-conditions are true
func (e *logicalAndEvaluator) Evaluate(ctx context.Context, condition *RuleCondition, evalCtx EvaluationContext) (bool, error) {
	for _, subCond := range condition.Conditions {
		// Get the evaluator for the sub-condition type (with proper lock)
		evaluator, exists := e.engine.getEvaluator(subCond.Type)
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

// logicalOrEvaluator evaluates OR conditions
type logicalOrEvaluator struct {
	engine *Engine
}

// Evaluate checks if any sub-condition is true
func (e *logicalOrEvaluator) Evaluate(ctx context.Context, condition *RuleCondition, evalCtx EvaluationContext) (bool, error) {
	for _, subCond := range condition.Conditions {
		// Get the evaluator for the sub-condition type (with proper lock)
		evaluator, exists := e.engine.getEvaluator(subCond.Type)
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

// logicalNotEvaluator evaluates NOT conditions
type logicalNotEvaluator struct {
	engine *Engine
}

// Evaluate negates the sub-condition
func (e *logicalNotEvaluator) Evaluate(ctx context.Context, condition *RuleCondition, evalCtx EvaluationContext) (bool, error) {
	if len(condition.Conditions) != 1 {
		return false, fmt.Errorf("NOT condition must have exactly one sub-condition")
	}

	subCond := condition.Conditions[0]
	evaluator, exists := e.engine.getEvaluator(subCond.Type)
	if !exists {
		return false, fmt.Errorf("unknown condition type: %s", subCond.Type)
	}

	passed, err := evaluator.Evaluate(ctx, &subCond, evalCtx)
	if err != nil {
		return false, err
	}
	return !passed, nil
}

// scriptEvaluator evaluates custom script conditions
type scriptEvaluator struct{}

// Evaluate runs a script and checks its exit code
func (e *scriptEvaluator) Evaluate(ctx context.Context, condition *RuleCondition, evalCtx EvaluationContext) (bool, error) {
	// If we have a compiled script function, use it
	if condition.CompiledScript != nil {
		if fn, ok := condition.CompiledScript.(func(context.Context, map[string]interface{}) (bool, error)); ok {
			// Convert EvaluationContext to map for script
			data := make(map[string]interface{})
			// This is a simplified conversion - in practice, you'd need to extract all relevant data
			if mapCtx, ok := evalCtx.(*mapEvaluationContext); ok {
				data = mapCtx.data
			}
			return fn(ctx, data)
		}
	}

	// Validate script content with language-specific patterns
	if err := validateScriptForLanguage(condition.Script, condition.Language); err != nil {
		return false, err
	}

	// Re-verify config file permissions before executing script
	if condition.SourceFile != "" {
		if err := validateFilePermissions(condition.SourceFile); err != nil {
			return false, fmt.Errorf("script execution blocked: source config file has unsafe permissions: %w", err)
		}
	}

	// Otherwise, execute the script as a shell command
	timeout := time.Duration(condition.TimeoutSeconds) * time.Second
	if timeout == 0 {
		timeout = defaultScriptTimeout
	}

	scriptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var cmd *exec.Cmd
	switch condition.Language {
	case "python":
		cmd = exec.CommandContext(scriptCtx, "python", "-c", condition.Script) //nolint:gosec // script content is from trusted quality gate config
	case "bash", "":
		// Use restricted mode (-r) to prevent directory changes, PATH modification,
		// commands with slashes, and output redirection. --noprofile/--norc prevent
		// loading startup files that could override restrictions.
		cmd = exec.CommandContext(scriptCtx, "bash", "--restricted", "--noprofile", "--norc", "-c", condition.Script) //nolint:gosec // script content is from trusted quality gate config
	default:
		return false, fmt.Errorf("unsupported script language: %s", condition.Language)
	}

	// Restrict environment variables to a minimal safe set
	cmd.Env = []string{
		"PATH=/usr/bin:/bin",
		"HOME=/tmp",
	}

	// Restrict working directory to a temporary directory
	tmpDir := os.TempDir()
	cmd.Dir = tmpDir

	// Audit log: record script execution
	slog.Info("quality/script: executing script", "language", scriptLanguage(condition.Language), "script_len", len(condition.Script), "timeout", timeout, "dir", tmpDir)

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

// validateScript checks that the script content is safe to execute.
// It applies all patterns regardless of language (backward compatibility).
func validateScript(script string) error {
	return validateScriptForLanguage(script, "")
}

// validateScriptForLanguage checks that the script content is safe for the
// given language. Shell scripts are checked against common + shell bypass +
// Python-invocation patterns. Python scripts are checked against common +
// Python-specific patterns. This prevents Python scripts from bypassing
// shell-oriented detection by using Python-native dangerous constructs.
func validateScriptForLanguage(script, language string) error {
	if strings.TrimSpace(script) == "" {
		return fmt.Errorf("script must not be empty")
	}
	if len(script) > maxScriptLength {
		return fmt.Errorf("script exceeds maximum length (%d > %d bytes)", len(script), maxScriptLength)
	}

	initDangerousPatterns()

	var patterns []*regexp.Regexp
	switch language {
	case "python":
		// Python scripts: common dangers + Python-specific patterns
		patterns = make([]*regexp.Regexp, 0, len(commonDangerousPatterns)+len(pythonSpecificPatterns))
		patterns = append(patterns, commonDangerousPatterns...)
		patterns = append(patterns, pythonSpecificPatterns...)
	default:
		// Shell scripts (bash, sh, empty): all patterns combined
		patterns = dangerousPatterns
	}

	for _, pat := range patterns {
		if pat.MatchString(script) {
			slog.Warn("quality/script: BLOCKED dangerous pattern in script", "pattern", pat.String())
			return fmt.Errorf("script contains dangerous command pattern: %s", pat.String())
		}
	}
	return nil
}

// scriptLanguage returns the effective language name for logging.
func scriptLanguage(lang string) string {
	if lang == "" {
		return "bash"
	}
	return lang
}
