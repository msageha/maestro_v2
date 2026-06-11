package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/uds"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// --- mock PlanExecutor ---

type mockPlanExecutor struct {
	submitFunc       func(json.RawMessage) (json.RawMessage, error)
	completeFunc     func(json.RawMessage) (json.RawMessage, error)
	addRetryTaskFunc func(json.RawMessage) (json.RawMessage, error)
	addTaskFunc      func(json.RawMessage) (json.RawMessage, error)
	rebuildFunc      func(json.RawMessage) (json.RawMessage, error)
}

func (m *mockPlanExecutor) Submit(params json.RawMessage) (json.RawMessage, error) {
	if m.submitFunc != nil {
		return m.submitFunc(params)
	}
	return json.RawMessage(`{"ok":true}`), nil
}

func (m *mockPlanExecutor) Complete(params json.RawMessage) (json.RawMessage, error) {
	if m.completeFunc != nil {
		return m.completeFunc(params)
	}
	return json.RawMessage(`{"ok":true}`), nil
}

func (m *mockPlanExecutor) AddRetryTask(params json.RawMessage) (json.RawMessage, error) {
	if m.addRetryTaskFunc != nil {
		return m.addRetryTaskFunc(params)
	}
	return json.RawMessage(`{"ok":true}`), nil
}

func (m *mockPlanExecutor) AddTask(params json.RawMessage) (json.RawMessage, error) {
	if m.addTaskFunc != nil {
		return m.addTaskFunc(params)
	}
	return json.RawMessage(`{"ok":true}`), nil
}

func (m *mockPlanExecutor) Rebuild(params json.RawMessage) (json.RawMessage, error) {
	if m.rebuildFunc != nil {
		return m.rebuildFunc(params)
	}
	return json.RawMessage(`{"ok":true}`), nil
}

// --- mock validationFormatter ---

type mockValidationError struct {
	msg    string
	stderr string
}

func (e *mockValidationError) Error() string        { return e.msg }
func (e *mockValidationError) FormatStderr() string { return e.stderr }

// --- mock codedFormatter ---

type mockCodedError struct {
	msg    string
	stderr string
	code   string
}

func (e *mockCodedError) Error() string        { return e.msg }
func (e *mockCodedError) FormatStderr() string { return e.stderr }
func (e *mockCodedError) ErrorCode() string    { return e.code }

// --- helpers ---

func newPlanTestDaemon(t *testing.T, pe PlanExecutor) *Daemon {
	t.Helper()
	d := newTestDaemon(t)
	d.SetPlanExecutor(pe)
	return d
}

func makePlanRequest(t *testing.T, op string, data any) *uds.Request {
	t.Helper()
	var rawData json.RawMessage
	if data != nil {
		b, err := json.Marshal(data)
		require.NoError(t, err)
		rawData = b
	} else {
		rawData = json.RawMessage(`{}`)
	}
	params, err := json.Marshal(struct {
		Operation string          `json:"operation"`
		Data      json.RawMessage `json:"data"`
	}{
		Operation: op,
		Data:      rawData,
	})
	require.NoError(t, err)
	return &uds.Request{CallerRole: uds.RolePlanner, Params: params}
}

// --- Tests ---

// TestHandlePlan_RecoveryRoleCheck verifies the trust boundary on the
// operator-recovery operations: requests whose CallerRole is "worker" must be
// rejected, empty/unknown CallerRole must be rejected, and valid non-worker
// roles pass the gate and reach the next layer (here: "worktree manager not
// configured" because the test daemon has none).
func TestHandlePlan_RecoveryRoleCheck(t *testing.T) {
	t.Parallel()
	d := newPlanTestDaemon(t, &mockPlanExecutor{})
	ops := []string{"unquarantine", "resume_merge", "resolve_conflict"}
	for _, op := range ops {
		t.Run("worker_rejected_"+op, func(t *testing.T) {
			t.Parallel()
			req := makePlanRequest(t, op, map[string]string{"command_id": "cmd_x"})
			req.CallerRole = "worker"
			resp := d.api.handlePlan(req)
			assert.False(t, resp.Success)
			require.NotNil(t, resp.Error)
			assert.Equal(t, uds.ErrCodeValidation, resp.Error.Code)
			assert.Contains(t, resp.Error.Message, "not permitted")
			assert.Contains(t, resp.Error.Message, "worker")
		})
		for _, role := range []string{"", "unknown_role", "admin", "root", "operator", "ORCHESTRATOR"} {
			t.Run("invalid_role_rejected_"+op+"_"+role, func(t *testing.T) {
				t.Parallel()
				req := makePlanRequest(t, op, map[string]string{"command_id": "cmd_x"})
				req.CallerRole = role
				resp := d.api.handlePlan(req)
				assert.False(t, resp.Success)
				require.NotNil(t, resp.Error)
				assert.Equal(t, uds.ErrCodeValidation, resp.Error.Code)
				assert.Contains(t, resp.Error.Message, "requires a valid caller role")
			})
		}
		for _, role := range []string{"orchestrator", "planner", "cli"} {
			// unquarantine は Planner も拒否される (operator 専用)。
			// CLI 層 (cmd/maestro/cmd_plan_ops.go) に加えて daemon 側でも
			// 強制することで、CLI を迂回した直接 UDS 呼び出しでも防御できる。
			if op == "unquarantine" && role == "planner" {
				continue
			}
			t.Run("valid_role_passes_"+op+"_"+role, func(t *testing.T) {
				t.Parallel()
				req := makePlanRequest(t, op, map[string]string{"command_id": "cmd_x"})
				req.CallerRole = role
				resp := d.api.handlePlan(req)
				// Passes the role check; fails at the worktree manager gate.
				assert.False(t, resp.Success)
				require.NotNil(t, resp.Error)
				assert.Equal(t, uds.ErrCodeInternal, resp.Error.Code)
				assert.Contains(t, resp.Error.Message, "worktree manager not configured")
			})
		}
	}
}

// TestHandlePlan_UnquarantineRejectsPlanner verifies the Planner-rejection
// rule for unquarantine: Planner must not be able to clear quarantine flags
// even when calling daemon directly (bypassing the CLI's role gate).
func TestHandlePlan_UnquarantineRejectsPlanner(t *testing.T) {
	t.Parallel()
	d := newPlanTestDaemon(t, &mockPlanExecutor{})
	req := makePlanRequest(t, "unquarantine", map[string]string{"command_id": "cmd_x"})
	req.CallerRole = uds.RolePlanner

	resp := d.api.handlePlan(req)

	assert.False(t, resp.Success)
	require.NotNil(t, resp.Error)
	assert.Equal(t, uds.ErrCodeValidation, resp.Error.Code)
	assert.Contains(t, resp.Error.Message, "operator")
	assert.Contains(t, resp.Error.Message, "Planner")
}

// TestHandlePlan_UnquarantineAllowsOrchestratorAndCLI verifies that
// orchestrator and cli roles still pass the role gate for unquarantine.
func TestHandlePlan_UnquarantineAllowsOrchestratorAndCLI(t *testing.T) {
	t.Parallel()
	for _, role := range []string{uds.RoleOrchestrator, uds.RoleCLI} {
		role := role
		t.Run(role, func(t *testing.T) {
			t.Parallel()
			d := newPlanTestDaemon(t, &mockPlanExecutor{})
			req := makePlanRequest(t, "unquarantine", map[string]string{"command_id": "cmd_x"})
			req.CallerRole = role
			resp := d.api.handlePlan(req)
			// Passes role check, fails at worktree manager gate.
			assert.False(t, resp.Success)
			require.NotNil(t, resp.Error)
			assert.Equal(t, uds.ErrCodeInternal, resp.Error.Code)
			assert.Contains(t, resp.Error.Message, "worktree manager not configured")
		})
	}
}

func TestHandlePlan_ControlPlaneRejectsWorkerAndOrchestrator(t *testing.T) {
	t.Parallel()
	for _, op := range []string{"submit", "complete", "add_retry_task", "add_task", "rebuild"} {
		op := op
		for _, role := range []string{uds.RoleWorker, uds.RoleOrchestrator} {
			role := role
			t.Run(op+"_"+role, func(t *testing.T) {
				t.Parallel()
				d := newPlanTestDaemon(t, &mockPlanExecutor{})
				req := makePlanRequest(t, op, map[string]string{"command_id": "cmd_x"})
				req.CallerRole = role

				resp := d.api.handlePlan(req)

				assert.False(t, resp.Success)
				require.NotNil(t, resp.Error)
				assert.Equal(t, uds.ErrCodeValidation, resp.Error.Code)
				assert.Contains(t, resp.Error.Message, "not permitted")
				assert.Contains(t, resp.Error.Message, role)
			})
		}
	}
}

func TestHandlePlan_NilExecutor(t *testing.T) {
	t.Parallel()
	d := newTestDaemon(t)
	// Do not set plan executor
	req := makePlanRequest(t, "submit", nil)

	resp := d.api.handlePlan(req)

	assert.False(t, resp.Success)
	assert.NotNil(t, resp.Error)
	assert.Equal(t, uds.ErrCodeInternal, resp.Error.Code)
	assert.Contains(t, resp.Error.Message, "plan executor not configured")
}

func TestHandlePlan_InvalidJSON(t *testing.T) {
	t.Parallel()
	d := newPlanTestDaemon(t, &mockPlanExecutor{})
	req := &uds.Request{CallerRole: uds.RolePlanner, Params: json.RawMessage(`{invalid`)}

	resp := d.api.handlePlan(req)

	assert.False(t, resp.Success)
	assert.NotNil(t, resp.Error)
	assert.Equal(t, uds.ErrCodeValidation, resp.Error.Code)
	assert.Contains(t, resp.Error.Message, "invalid params")
}

func TestHandlePlan_UnknownOperation(t *testing.T) {
	t.Parallel()
	d := newPlanTestDaemon(t, &mockPlanExecutor{})
	req := makePlanRequest(t, "nonexistent", nil)

	resp := d.api.handlePlan(req)

	assert.False(t, resp.Success)
	assert.NotNil(t, resp.Error)
	assert.Equal(t, uds.ErrCodeValidation, resp.Error.Code)
	assert.Contains(t, resp.Error.Message, "unknown plan operation")
	assert.Contains(t, resp.Error.Message, "nonexistent")
}

func TestHandlePlan_SubmitSuccess(t *testing.T) {
	t.Parallel()
	pe := &mockPlanExecutor{
		submitFunc: func(data json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`{"command_id":"cmd_001"}`), nil
		},
	}
	d := newPlanTestDaemon(t, pe)
	req := makePlanRequest(t, "submit", map[string]string{"purpose": "test"})

	resp := d.api.handlePlan(req)

	assert.True(t, resp.Success)
	assert.Contains(t, string(resp.Data), "cmd_001")
}

func TestHandlePlan_CompleteSuccess(t *testing.T) {
	t.Parallel()
	pe := &mockPlanExecutor{
		completeFunc: func(data json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`{"status":"completed"}`), nil
		},
	}
	d := newPlanTestDaemon(t, pe)
	req := makePlanRequest(t, "complete", map[string]string{"command_id": "cmd_001"})

	resp := d.api.handlePlan(req)

	assert.True(t, resp.Success)
	assert.Contains(t, string(resp.Data), "completed")
}

func TestHandlePlan_AddRetryTaskSuccess(t *testing.T) {
	t.Parallel()
	pe := &mockPlanExecutor{
		addRetryTaskFunc: func(data json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`{"task_id":"task_retry_001"}`), nil
		},
	}
	d := newPlanTestDaemon(t, pe)
	req := makePlanRequest(t, "add_retry_task", map[string]string{"task_id": "task_001"})

	resp := d.api.handlePlan(req)

	assert.True(t, resp.Success)
	assert.Contains(t, string(resp.Data), "task_retry_001")
}

func TestHandlePlan_AddTaskRejectsQuarantinedCommand(t *testing.T) {
	pe := &mockPlanExecutor{
		addTaskFunc: func(data json.RawMessage) (json.RawMessage, error) {
			t.Fatal("AddTask must not be called for quarantined commands")
			return nil, nil
		},
	}
	d := newPlanTestDaemon(t, pe)
	commandID := "cmd_quarantined"
	path := filepath.Join(d.maestroDir, "state", "worktrees", commandID+".yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, yamlutil.AtomicWrite(path, model.WorktreeCommandState{
		SchemaVersion: 1,
		FileType:      "state_worktree",
		CommandID:     commandID,
		Integration: model.IntegrationState{
			Status: model.IntegrationStatusQuarantined,
		},
	}))
	req := makePlanRequest(t, "add_task", map[string]string{"command_id": commandID})

	resp := d.api.handlePlan(req)

	require.False(t, resp.Success)
	assert.Equal(t, uds.ErrCodeActionRequired, resp.Error.Code)
	assert.Contains(t, resp.Error.Message, "quarantined")
}

func TestHandlePlan_RebuildSuccess(t *testing.T) {
	t.Parallel()
	pe := &mockPlanExecutor{
		rebuildFunc: func(data json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`{"rebuilt":true}`), nil
		},
	}
	d := newPlanTestDaemon(t, pe)
	req := makePlanRequest(t, "rebuild", map[string]string{"command_id": "cmd_001"})

	resp := d.api.handlePlan(req)

	assert.True(t, resp.Success)
	assert.Contains(t, string(resp.Data), "rebuilt")
}

func TestHandlePlan_GenericError(t *testing.T) {
	t.Parallel()
	pe := &mockPlanExecutor{
		submitFunc: func(data json.RawMessage) (json.RawMessage, error) {
			return nil, fmt.Errorf("disk full")
		},
	}
	d := newPlanTestDaemon(t, pe)
	req := makePlanRequest(t, "submit", nil)

	resp := d.api.handlePlan(req)

	assert.False(t, resp.Success)
	assert.NotNil(t, resp.Error)
	assert.Equal(t, uds.ErrCodeInternal, resp.Error.Code)
	assert.Contains(t, resp.Error.Message, "disk full")
}

func TestHandlePlan_ValidationError(t *testing.T) {
	t.Parallel()
	pe := &mockPlanExecutor{
		submitFunc: func(data json.RawMessage) (json.RawMessage, error) {
			return nil, &mockValidationError{
				msg:    "validation failed",
				stderr: "missing required field: purpose",
			}
		},
	}
	d := newPlanTestDaemon(t, pe)
	req := makePlanRequest(t, "submit", nil)

	resp := d.api.handlePlan(req)

	assert.False(t, resp.Success)
	assert.NotNil(t, resp.Error)
	assert.Equal(t, uds.ErrCodeValidation, resp.Error.Code)
	assert.Contains(t, resp.Error.Message, "missing required field: purpose")
}

func TestHandlePlan_CodedError(t *testing.T) {
	t.Parallel()
	pe := &mockPlanExecutor{
		completeFunc: func(data json.RawMessage) (json.RawMessage, error) {
			return nil, &mockCodedError{
				msg:    "action required",
				stderr: "deferred tasks need filling",
				code:   uds.ErrCodeActionRequired,
			}
		},
	}
	d := newPlanTestDaemon(t, pe)
	req := makePlanRequest(t, "complete", nil)

	resp := d.api.handlePlan(req)

	assert.False(t, resp.Success)
	assert.NotNil(t, resp.Error)
	assert.Equal(t, uds.ErrCodeActionRequired, resp.Error.Code)
	assert.Contains(t, resp.Error.Message, "deferred tasks need filling")
}

func TestHandlePlan_CodedErrorPrecedesValidation(t *testing.T) {
	t.Parallel()
	// codedFormatter satisfies both interfaces; the coded branch should win
	pe := &mockPlanExecutor{
		submitFunc: func(data json.RawMessage) (json.RawMessage, error) {
			return nil, &mockCodedError{
				msg:    "coded",
				stderr: "coded stderr",
				code:   "CUSTOM_CODE",
			}
		},
	}
	d := newPlanTestDaemon(t, pe)
	req := makePlanRequest(t, "submit", nil)

	resp := d.api.handlePlan(req)

	assert.False(t, resp.Success)
	assert.Equal(t, "CUSTOM_CODE", resp.Error.Code)
	assert.Equal(t, "coded stderr", resp.Error.Message)
}

func TestHandlePlan_AllOperations(t *testing.T) {
	t.Parallel()
	operations := []string{"submit", "complete", "add_retry_task", "rebuild"}

	for _, op := range operations {
		t.Run(op, func(t *testing.T) {
			t.Parallel()
			pe := &mockPlanExecutor{}
			d := newPlanTestDaemon(t, pe)
			req := makePlanRequest(t, op, nil)

			resp := d.api.handlePlan(req)

			assert.True(t, resp.Success, "operation %s should succeed", op)
			assert.Nil(t, resp.Error)
		})
	}
}

func TestHandlePlan_DataPassthrough(t *testing.T) {
	t.Parallel()
	var received json.RawMessage
	pe := &mockPlanExecutor{
		submitFunc: func(data json.RawMessage) (json.RawMessage, error) {
			received = data
			return json.RawMessage(`{}`), nil
		},
	}
	d := newPlanTestDaemon(t, pe)

	inputData := map[string]interface{}{
		"purpose": "test purpose",
		"tasks":   []string{"t1", "t2"},
	}
	req := makePlanRequest(t, "submit", inputData)

	resp := d.api.handlePlan(req)

	assert.True(t, resp.Success)
	// Verify the data was forwarded to the executor
	var parsed map[string]interface{}
	require.NoError(t, json.Unmarshal(received, &parsed))
	assert.Equal(t, "test purpose", parsed["purpose"])
}

func TestHandlePlan_EmptyParams(t *testing.T) {
	t.Parallel()
	d := newPlanTestDaemon(t, &mockPlanExecutor{})
	// Empty JSON object — missing operation field
	req := &uds.Request{CallerRole: uds.RolePlanner, Params: json.RawMessage(`{}`)}

	resp := d.api.handlePlan(req)

	// Empty operation string triggers "unknown plan operation"
	assert.False(t, resp.Success)
	assert.Equal(t, uds.ErrCodeValidation, resp.Error.Code)
	assert.Contains(t, resp.Error.Message, "unknown plan operation")
}

func TestHandlePlan_NilParams(t *testing.T) {
	t.Parallel()
	d := newPlanTestDaemon(t, &mockPlanExecutor{})
	req := &uds.Request{CallerRole: uds.RolePlanner, Params: nil}

	resp := d.api.handlePlan(req)

	assert.False(t, resp.Success)
	assert.Equal(t, uds.ErrCodeValidation, resp.Error.Code)
}

func TestHandlePlan_ErrorAllOperations(t *testing.T) {
	t.Parallel()
	expectedErr := errors.New("some error")
	operations := map[string]func(*mockPlanExecutor){
		"submit": func(pe *mockPlanExecutor) {
			pe.submitFunc = func(json.RawMessage) (json.RawMessage, error) { return nil, expectedErr }
		},
		"complete": func(pe *mockPlanExecutor) {
			pe.completeFunc = func(json.RawMessage) (json.RawMessage, error) { return nil, expectedErr }
		},
		"add_retry_task": func(pe *mockPlanExecutor) {
			pe.addRetryTaskFunc = func(json.RawMessage) (json.RawMessage, error) { return nil, expectedErr }
		},
		"rebuild": func(pe *mockPlanExecutor) {
			pe.rebuildFunc = func(json.RawMessage) (json.RawMessage, error) { return nil, expectedErr }
		},
	}

	for op, setup := range operations {
		t.Run(op, func(t *testing.T) {
			t.Parallel()
			pe := &mockPlanExecutor{}
			setup(pe)
			d := newPlanTestDaemon(t, pe)
			req := makePlanRequest(t, op, nil)

			resp := d.api.handlePlan(req)

			assert.False(t, resp.Success, "operation %s should fail", op)
			assert.Equal(t, uds.ErrCodeInternal, resp.Error.Code)
			assert.Contains(t, resp.Error.Message, "some error")
		})
	}
}

func TestSetPlanExecutor(t *testing.T) {
	t.Parallel()
	d := newTestDaemon(t)
	assert.Nil(t, d.planExecutor)

	pe := &mockPlanExecutor{}
	d.SetPlanExecutor(pe)
	assert.Equal(t, pe, d.planExecutor)
}

// TestHandlePlan_OperationDataPassthrough verifies that each operation forwards
// the input data to the correct executor method without mutation.
func TestHandlePlan_OperationDataPassthrough(t *testing.T) {
	t.Parallel()

	type testCase struct {
		name      string
		operation string
		input     map[string]interface{}
		setupMock func(pe *mockPlanExecutor, ch chan json.RawMessage)
	}

	cases := []testCase{
		{
			name:      "submit",
			operation: "submit",
			input:     map[string]interface{}{"purpose": "deploy", "version": 42},
			setupMock: func(pe *mockPlanExecutor, ch chan json.RawMessage) {
				pe.submitFunc = func(data json.RawMessage) (json.RawMessage, error) {
					ch <- data
					return json.RawMessage(`{}`), nil
				}
			},
		},
		{
			name:      "complete",
			operation: "complete",
			input:     map[string]interface{}{"command_id": "cmd_abc", "final": true},
			setupMock: func(pe *mockPlanExecutor, ch chan json.RawMessage) {
				pe.completeFunc = func(data json.RawMessage) (json.RawMessage, error) {
					ch <- data
					return json.RawMessage(`{}`), nil
				}
			},
		},
		{
			name:      "add_retry_task",
			operation: "add_retry_task",
			input:     map[string]interface{}{"task_id": "task_99", "attempt": 3},
			setupMock: func(pe *mockPlanExecutor, ch chan json.RawMessage) {
				pe.addRetryTaskFunc = func(data json.RawMessage) (json.RawMessage, error) {
					ch <- data
					return json.RawMessage(`{}`), nil
				}
			},
		},
		{
			name:      "rebuild",
			operation: "rebuild",
			input:     map[string]interface{}{"command_id": "cmd_xyz", "force": false},
			setupMock: func(pe *mockPlanExecutor, ch chan json.RawMessage) {
				pe.rebuildFunc = func(data json.RawMessage) (json.RawMessage, error) {
					ch <- data
					return json.RawMessage(`{}`), nil
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ch := make(chan json.RawMessage, 1)
			pe := &mockPlanExecutor{}
			tc.setupMock(pe, ch)
			d := newPlanTestDaemon(t, pe)

			req := makePlanRequest(t, tc.operation, tc.input)
			resp := d.api.handlePlan(req)

			require.True(t, resp.Success, "operation %s should succeed", tc.operation)
			assert.Nil(t, resp.Error)

			// Verify the executor received the correct data.
			received := <-ch
			var got map[string]interface{}
			require.NoError(t, json.Unmarshal(received, &got))

			// Marshal+unmarshal the expected input through the same path so
			// numeric types match (json.Number vs float64 is avoided by
			// comparing canonical JSON bytes).
			expectedBytes, err := json.Marshal(tc.input)
			require.NoError(t, err)
			var expected map[string]interface{}
			require.NoError(t, json.Unmarshal(expectedBytes, &expected))

			assert.Equal(t, expected, got,
				"operation %s should forward input data unchanged", tc.operation)
		})
	}
}

// TestHandlePlan_ValidationErrorAllOperations verifies that a validationFormatter
// error from any operation is mapped to ErrCodeValidation with the stderr message.
func TestHandlePlan_ValidationErrorAllOperations(t *testing.T) {
	t.Parallel()

	type testCase struct {
		name      string
		operation string
		errMsg    string
		stderr    string
		setupMock func(pe *mockPlanExecutor, ve *mockValidationError)
	}

	cases := []testCase{
		{
			name:      "submit_validation",
			operation: "submit",
			errMsg:    "submit validation failed",
			stderr:    "missing field: purpose",
			setupMock: func(pe *mockPlanExecutor, ve *mockValidationError) {
				pe.submitFunc = func(json.RawMessage) (json.RawMessage, error) { return nil, ve }
			},
		},
		{
			name:      "complete_validation",
			operation: "complete",
			errMsg:    "complete validation failed",
			stderr:    "command_id must not be empty",
			setupMock: func(pe *mockPlanExecutor, ve *mockValidationError) {
				pe.completeFunc = func(json.RawMessage) (json.RawMessage, error) { return nil, ve }
			},
		},
		{
			name:      "add_retry_task_validation",
			operation: "add_retry_task",
			errMsg:    "retry validation failed",
			stderr:    "task_id is invalid",
			setupMock: func(pe *mockPlanExecutor, ve *mockValidationError) {
				pe.addRetryTaskFunc = func(json.RawMessage) (json.RawMessage, error) { return nil, ve }
			},
		},
		{
			name:      "rebuild_validation",
			operation: "rebuild",
			errMsg:    "rebuild validation failed",
			stderr:    "no plan to rebuild",
			setupMock: func(pe *mockPlanExecutor, ve *mockValidationError) {
				pe.rebuildFunc = func(json.RawMessage) (json.RawMessage, error) { return nil, ve }
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ve := &mockValidationError{msg: tc.errMsg, stderr: tc.stderr}
			pe := &mockPlanExecutor{}
			tc.setupMock(pe, ve)
			d := newPlanTestDaemon(t, pe)

			req := makePlanRequest(t, tc.operation, nil)
			resp := d.api.handlePlan(req)

			assert.False(t, resp.Success)
			require.NotNil(t, resp.Error)
			assert.Equal(t, uds.ErrCodeValidation, resp.Error.Code,
				"operation %s validation error should map to ErrCodeValidation", tc.operation)
			assert.Equal(t, tc.stderr, resp.Error.Message,
				"operation %s should use FormatStderr() as the error message", tc.operation)
		})
	}
}

// TestHandlePlan_CodedErrorAllOperations verifies that a codedFormatter error
// from any operation preserves the custom error code and stderr message.
func TestHandlePlan_CodedErrorAllOperations(t *testing.T) {
	t.Parallel()

	type testCase struct {
		name      string
		operation string
		errMsg    string
		stderr    string
		code      string
		setupMock func(pe *mockPlanExecutor, ce *mockCodedError)
	}

	cases := []testCase{
		{
			name:      "submit_coded",
			operation: "submit",
			errMsg:    "submit action required",
			stderr:    "approve the plan first",
			code:      uds.ErrCodeActionRequired,
			setupMock: func(pe *mockPlanExecutor, ce *mockCodedError) {
				pe.submitFunc = func(json.RawMessage) (json.RawMessage, error) { return nil, ce }
			},
		},
		{
			name:      "complete_coded",
			operation: "complete",
			errMsg:    "complete fencing",
			stderr:    "stale fencing token",
			code:      uds.ErrCodeFencingReject,
			setupMock: func(pe *mockPlanExecutor, ce *mockCodedError) {
				pe.completeFunc = func(json.RawMessage) (json.RawMessage, error) { return nil, ce }
			},
		},
		{
			name:      "add_retry_task_coded",
			operation: "add_retry_task",
			errMsg:    "retry not found",
			stderr:    "original task does not exist",
			code:      uds.ErrCodeNotFound,
			setupMock: func(pe *mockPlanExecutor, ce *mockCodedError) {
				pe.addRetryTaskFunc = func(json.RawMessage) (json.RawMessage, error) { return nil, ce }
			},
		},
		{
			name:      "rebuild_coded",
			operation: "rebuild",
			errMsg:    "rebuild duplicate",
			stderr:    "plan already being rebuilt",
			code:      uds.ErrCodeDuplicate,
			setupMock: func(pe *mockPlanExecutor, ce *mockCodedError) {
				pe.rebuildFunc = func(json.RawMessage) (json.RawMessage, error) { return nil, ce }
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ce := &mockCodedError{msg: tc.errMsg, stderr: tc.stderr, code: tc.code}
			pe := &mockPlanExecutor{}
			tc.setupMock(pe, ce)
			d := newPlanTestDaemon(t, pe)

			req := makePlanRequest(t, tc.operation, nil)
			resp := d.api.handlePlan(req)

			assert.False(t, resp.Success)
			require.NotNil(t, resp.Error)
			assert.Equal(t, tc.code, resp.Error.Code,
				"operation %s coded error should preserve custom error code", tc.operation)
			assert.Equal(t, tc.stderr, resp.Error.Message,
				"operation %s should use FormatStderr() as the error message", tc.operation)
		})
	}
}

// TestHandlePlan_SuccessResponseStructure verifies that the response Data field
// is exactly the JSON the mock returned, using deep equality rather than string
// containment. Each operation returns a distinct, non-trivial structure.
func TestHandlePlan_SuccessResponseStructure(t *testing.T) {
	t.Parallel()

	type testCase struct {
		name       string
		operation  string
		mockReturn string
		setupMock  func(pe *mockPlanExecutor, ret json.RawMessage)
	}

	cases := []testCase{
		{
			name:       "submit_response",
			operation:  "submit",
			mockReturn: `{"command_id":"cmd_777","plan_version":3,"tasks_created":5}`,
			setupMock: func(pe *mockPlanExecutor, ret json.RawMessage) {
				pe.submitFunc = func(json.RawMessage) (json.RawMessage, error) { return ret, nil }
			},
		},
		{
			name:       "complete_response",
			operation:  "complete",
			mockReturn: `{"status":"completed","completed_at":"2026-01-15T10:30:00Z","summary":"all tasks done"}`,
			setupMock: func(pe *mockPlanExecutor, ret json.RawMessage) {
				pe.completeFunc = func(json.RawMessage) (json.RawMessage, error) { return ret, nil }
			},
		},
		{
			name:       "add_retry_task_response",
			operation:  "add_retry_task",
			mockReturn: `{"task_id":"task_retry_042","original_task_id":"task_042","attempt":2}`,
			setupMock: func(pe *mockPlanExecutor, ret json.RawMessage) {
				pe.addRetryTaskFunc = func(json.RawMessage) (json.RawMessage, error) { return ret, nil }
			},
		},
		{
			name:       "rebuild_response",
			operation:  "rebuild",
			mockReturn: `{"rebuilt":true,"new_version":4,"removed_tasks":["task_old_1","task_old_2"]}`,
			setupMock: func(pe *mockPlanExecutor, ret json.RawMessage) {
				pe.rebuildFunc = func(json.RawMessage) (json.RawMessage, error) { return ret, nil }
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ret := json.RawMessage(tc.mockReturn)
			pe := &mockPlanExecutor{}
			tc.setupMock(pe, ret)
			d := newPlanTestDaemon(t, pe)

			req := makePlanRequest(t, tc.operation, map[string]string{"key": "value"})
			resp := d.api.handlePlan(req)

			require.True(t, resp.Success, "operation %s should succeed", tc.operation)
			assert.Nil(t, resp.Error)

			// Deep-compare the response Data against the mock return value.
			var expectedData interface{}
			require.NoError(t, json.Unmarshal([]byte(tc.mockReturn), &expectedData),
				"mock return value should be valid JSON")

			var actualData interface{}
			require.NoError(t, json.Unmarshal(resp.Data, &actualData),
				"response Data should be valid JSON")

			assert.Equal(t, expectedData, actualData,
				"operation %s response Data should exactly match the executor return value", tc.operation)
		})
	}
}
