package bridge

import (
	"encoding/json"
	"testing"
)

func TestSubmitParams_Unmarshal(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
		check   func(p submitParams) error
	}{
		{
			name:  "valid params",
			input: `{"command_id":"cmd1","tasks_file":"plan.yaml","phase_name":"phase1","dry_run":true}`,
			check: func(p submitParams) error {
				if p.CommandID != "cmd1" {
					t.Errorf("CommandID = %q, want cmd1", p.CommandID)
				}
				if p.TasksFile != "plan.yaml" {
					t.Errorf("TasksFile = %q, want plan.yaml", p.TasksFile)
				}
				if p.PhaseName != "phase1" {
					t.Errorf("PhaseName = %q, want phase1", p.PhaseName)
				}
				if !p.DryRun {
					t.Error("DryRun = false, want true")
				}
				return nil
			},
		},
		{
			name:  "minimal params",
			input: `{"command_id":"cmd2","tasks_file":"t.yaml"}`,
			check: func(p submitParams) error {
				if p.CommandID != "cmd2" {
					t.Errorf("CommandID = %q, want cmd2", p.CommandID)
				}
				if p.DryRun {
					t.Error("DryRun should default to false")
				}
				return nil
			},
		},
		{
			name:    "invalid JSON",
			input:   `{invalid`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var p submitParams
			err := json.Unmarshal([]byte(tt.input), &p)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr = %v", err, tt.wantErr)
			}
			if tt.check != nil {
				tt.check(p)
			}
		})
	}
}

func TestCompleteParams_Unmarshal(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
		check   func(p completeParams)
	}{
		{
			name:  "valid params",
			input: `{"command_id":"cmd1","summary":"all done"}`,
			check: func(p completeParams) {
				if p.CommandID != "cmd1" {
					t.Errorf("CommandID = %q, want cmd1", p.CommandID)
				}
				if p.Summary != "all done" {
					t.Errorf("Summary = %q, want 'all done'", p.Summary)
				}
			},
		},
		{
			name:    "invalid JSON",
			input:   `not json`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var p completeParams
			err := json.Unmarshal([]byte(tt.input), &p)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr = %v", err, tt.wantErr)
			}
			if tt.check != nil {
				tt.check(p)
			}
		})
	}
}

func TestRetryParams_Unmarshal(t *testing.T) {
	input := `{
		"command_id": "cmd1",
		"retry_of": "t1",
		"purpose": "fix bug",
		"content": "retry content",
		"acceptance_criteria": "tests pass",
		"constraints": ["no side effects"],
		"blocked_by": ["t0"],
		"bloom_level": 3,
		"tools_hint": ["bash"]
	}`

	var p retryParams
	if err := json.Unmarshal([]byte(input), &p); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if p.CommandID != "cmd1" {
		t.Errorf("CommandID = %q, want cmd1", p.CommandID)
	}
	if p.RetryOf != "t1" {
		t.Errorf("RetryOf = %q, want t1", p.RetryOf)
	}
	if p.BloomLevel != 3 {
		t.Errorf("BloomLevel = %d, want 3", p.BloomLevel)
	}
	if len(p.Constraints) != 1 || p.Constraints[0] != "no side effects" {
		t.Errorf("Constraints = %v, want [no side effects]", p.Constraints)
	}
	if len(p.BlockedBy) != 1 || p.BlockedBy[0] != "t0" {
		t.Errorf("BlockedBy = %v, want [t0]", p.BlockedBy)
	}
	if len(p.ToolsHint) != 1 || p.ToolsHint[0] != "bash" {
		t.Errorf("ToolsHint = %v, want [bash]", p.ToolsHint)
	}
}

func TestRebuildParams_Unmarshal(t *testing.T) {
	input := `{"command_id": "cmd_rebuild"}`

	var p rebuildParams
	if err := json.Unmarshal([]byte(input), &p); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if p.CommandID != "cmd_rebuild" {
		t.Errorf("CommandID = %q, want cmd_rebuild", p.CommandID)
	}
}

func TestPlanExecutorImpl_SubmitInvalidJSON(t *testing.T) {
	pe := &PlanExecutorImpl{
		MaestroDir: t.TempDir(),
	}

	_, err := pe.Submit(json.RawMessage(`{invalid`))
	if err == nil {
		t.Fatal("Submit with invalid JSON should return error")
	}
}

func TestPlanExecutorImpl_CompleteInvalidJSON(t *testing.T) {
	pe := &PlanExecutorImpl{
		MaestroDir: t.TempDir(),
	}

	_, err := pe.Complete(json.RawMessage(`{invalid`))
	if err == nil {
		t.Fatal("Complete with invalid JSON should return error")
	}
}

func TestPlanExecutorImpl_AddRetryTaskInvalidJSON(t *testing.T) {
	pe := &PlanExecutorImpl{
		MaestroDir: t.TempDir(),
	}

	_, err := pe.AddRetryTask(json.RawMessage(`{invalid`))
	if err == nil {
		t.Fatal("AddRetryTask with invalid JSON should return error")
	}
}

func TestPlanExecutorImpl_RebuildInvalidJSON(t *testing.T) {
	pe := &PlanExecutorImpl{
		MaestroDir: t.TempDir(),
	}

	_, err := pe.Rebuild(json.RawMessage(`{invalid`))
	if err == nil {
		t.Fatal("Rebuild with invalid JSON should return error")
	}
}
