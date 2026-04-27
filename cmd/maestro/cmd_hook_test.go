package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestRunHookPolicyCheck_AllowsReadTool(t *testing.T) {
	app := newCLIApp()
	var out bytes.Buffer
	err := app.runHookPolicyCheck(nil, strings.NewReader(`{"tool_name":"Read","tool_input":{"command":"rm -rf /"}}`), &out)
	if err != nil {
		t.Fatalf("runHookPolicyCheck: %v", err)
	}
	var got hookPolicyDecisionOutput
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if !got.Allow {
		t.Fatalf("Allow = false, reason=%q", got.Reason)
	}
	if got.HookSpecificOutput != nil {
		t.Fatalf("HookSpecificOutput must be nil for allow: %+v", got.HookSpecificOutput)
	}
}

func TestRunHookPolicyCheck_DeniesDangerousBash(t *testing.T) {
	app := newCLIApp()
	var out bytes.Buffer
	err := app.runHookPolicyCheck(nil, strings.NewReader(`{"tool_name":"Bash","tool_input":{"command":"git reset --hard HEAD"}}`), &out)
	if err != nil {
		t.Fatalf("runHookPolicyCheck: %v", err)
	}
	var got hookPolicyDecisionOutput
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if got.Allow {
		t.Fatal("Allow = true, want false")
	}
	if !strings.Contains(got.Reason, "D004") {
		t.Fatalf("Reason = %q, want D004", got.Reason)
	}
	if got.HookSpecificOutput == nil {
		t.Fatal("HookSpecificOutput must be present for deny")
	}
	if got.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("PermissionDecision = %q, want deny", got.HookSpecificOutput.PermissionDecision)
	}
	if got.HookSpecificOutput.PermissionDecisionReason != got.Reason {
		t.Fatalf("hook reason = %q, top-level reason = %q", got.HookSpecificOutput.PermissionDecisionReason, got.Reason)
	}
}

func TestRunHookPolicyCheck_InvalidJSON(t *testing.T) {
	app := newCLIApp()
	err := app.runHookPolicyCheck(nil, strings.NewReader(`{`), &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected error")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
	if !strings.Contains(ce.Msg, "invalid hook JSON") {
		t.Fatalf("Msg = %q", ce.Msg)
	}
}
