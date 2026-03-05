package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/msageha/maestro_v2/internal/uds"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- mock PlanExecutor ---

type mockPlanExecutor struct {
	submitFunc       func(json.RawMessage) (json.RawMessage, error)
	completeFunc     func(json.RawMessage) (json.RawMessage, error)
	addRetryTaskFunc func(json.RawMessage) (json.RawMessage, error)
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

func (e *mockValidationError) Error() string       { return e.msg }
func (e *mockValidationError) FormatStderr() string { return e.stderr }

// --- mock codedFormatter ---

type mockCodedError struct {
	msg    string
	stderr string
	code   string
}

func (e *mockCodedError) Error() string       { return e.msg }
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
	return &uds.Request{Params: params}
}

// --- Tests ---

func TestHandlePlan_NilExecutor(t *testing.T) {
	d := newTestDaemon(t)
	// Do not set plan executor
	req := makePlanRequest(t, "submit", nil)

	resp := d.handlePlan(req)

	assert.False(t, resp.Success)
	assert.NotNil(t, resp.Error)
	assert.Equal(t, uds.ErrCodeInternal, resp.Error.Code)
	assert.Contains(t, resp.Error.Message, "plan executor not configured")
}

func TestHandlePlan_InvalidJSON(t *testing.T) {
	d := newPlanTestDaemon(t, &mockPlanExecutor{})
	req := &uds.Request{Params: json.RawMessage(`{invalid`)}

	resp := d.handlePlan(req)

	assert.False(t, resp.Success)
	assert.NotNil(t, resp.Error)
	assert.Equal(t, uds.ErrCodeValidation, resp.Error.Code)
	assert.Contains(t, resp.Error.Message, "invalid params")
}

func TestHandlePlan_UnknownOperation(t *testing.T) {
	d := newPlanTestDaemon(t, &mockPlanExecutor{})
	req := makePlanRequest(t, "nonexistent", nil)

	resp := d.handlePlan(req)

	assert.False(t, resp.Success)
	assert.NotNil(t, resp.Error)
	assert.Equal(t, uds.ErrCodeValidation, resp.Error.Code)
	assert.Contains(t, resp.Error.Message, "unknown plan operation")
	assert.Contains(t, resp.Error.Message, "nonexistent")
}

func TestHandlePlan_SubmitSuccess(t *testing.T) {
	pe := &mockPlanExecutor{
		submitFunc: func(data json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`{"command_id":"cmd_001"}`), nil
		},
	}
	d := newPlanTestDaemon(t, pe)
	req := makePlanRequest(t, "submit", map[string]string{"purpose": "test"})

	resp := d.handlePlan(req)

	assert.True(t, resp.Success)
	assert.Nil(t, resp.Error)
	assert.Contains(t, string(resp.Data), "cmd_001")
}

func TestHandlePlan_CompleteSuccess(t *testing.T) {
	pe := &mockPlanExecutor{
		completeFunc: func(data json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`{"status":"completed"}`), nil
		},
	}
	d := newPlanTestDaemon(t, pe)
	req := makePlanRequest(t, "complete", map[string]string{"command_id": "cmd_001"})

	resp := d.handlePlan(req)

	assert.True(t, resp.Success)
	assert.Nil(t, resp.Error)
	assert.Contains(t, string(resp.Data), "completed")
}

func TestHandlePlan_AddRetryTaskSuccess(t *testing.T) {
	pe := &mockPlanExecutor{
		addRetryTaskFunc: func(data json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`{"task_id":"task_retry_001"}`), nil
		},
	}
	d := newPlanTestDaemon(t, pe)
	req := makePlanRequest(t, "add_retry_task", map[string]string{"task_id": "task_001"})

	resp := d.handlePlan(req)

	assert.True(t, resp.Success)
	assert.Nil(t, resp.Error)
	assert.Contains(t, string(resp.Data), "task_retry_001")
}

func TestHandlePlan_RebuildSuccess(t *testing.T) {
	pe := &mockPlanExecutor{
		rebuildFunc: func(data json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`{"rebuilt":true}`), nil
		},
	}
	d := newPlanTestDaemon(t, pe)
	req := makePlanRequest(t, "rebuild", map[string]string{"command_id": "cmd_001"})

	resp := d.handlePlan(req)

	assert.True(t, resp.Success)
	assert.Nil(t, resp.Error)
	assert.Contains(t, string(resp.Data), "rebuilt")
}

func TestHandlePlan_GenericError(t *testing.T) {
	pe := &mockPlanExecutor{
		submitFunc: func(data json.RawMessage) (json.RawMessage, error) {
			return nil, fmt.Errorf("disk full")
		},
	}
	d := newPlanTestDaemon(t, pe)
	req := makePlanRequest(t, "submit", nil)

	resp := d.handlePlan(req)

	assert.False(t, resp.Success)
	assert.NotNil(t, resp.Error)
	assert.Equal(t, uds.ErrCodeInternal, resp.Error.Code)
	assert.Contains(t, resp.Error.Message, "disk full")
}

func TestHandlePlan_ValidationError(t *testing.T) {
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

	resp := d.handlePlan(req)

	assert.False(t, resp.Success)
	assert.NotNil(t, resp.Error)
	assert.Equal(t, uds.ErrCodeValidation, resp.Error.Code)
	assert.Contains(t, resp.Error.Message, "missing required field: purpose")
}

func TestHandlePlan_CodedError(t *testing.T) {
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

	resp := d.handlePlan(req)

	assert.False(t, resp.Success)
	assert.NotNil(t, resp.Error)
	assert.Equal(t, uds.ErrCodeActionRequired, resp.Error.Code)
	assert.Contains(t, resp.Error.Message, "deferred tasks need filling")
}

func TestHandlePlan_CodedErrorPrecedesValidation(t *testing.T) {
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

	resp := d.handlePlan(req)

	assert.False(t, resp.Success)
	assert.Equal(t, "CUSTOM_CODE", resp.Error.Code)
	assert.Equal(t, "coded stderr", resp.Error.Message)
}

func TestHandlePlan_AllOperations(t *testing.T) {
	operations := []string{"submit", "complete", "add_retry_task", "rebuild"}

	for _, op := range operations {
		t.Run(op, func(t *testing.T) {
			pe := &mockPlanExecutor{}
			d := newPlanTestDaemon(t, pe)
			req := makePlanRequest(t, op, nil)

			resp := d.handlePlan(req)

			assert.True(t, resp.Success, "operation %s should succeed", op)
			assert.Nil(t, resp.Error)
		})
	}
}

func TestHandlePlan_DataPassthrough(t *testing.T) {
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

	resp := d.handlePlan(req)

	assert.True(t, resp.Success)
	// Verify the data was forwarded to the executor
	var parsed map[string]interface{}
	require.NoError(t, json.Unmarshal(received, &parsed))
	assert.Equal(t, "test purpose", parsed["purpose"])
}

func TestHandlePlan_EmptyParams(t *testing.T) {
	d := newPlanTestDaemon(t, &mockPlanExecutor{})
	// Empty JSON object — missing operation field
	req := &uds.Request{Params: json.RawMessage(`{}`)}

	resp := d.handlePlan(req)

	// Empty operation string triggers "unknown plan operation"
	assert.False(t, resp.Success)
	assert.Equal(t, uds.ErrCodeValidation, resp.Error.Code)
	assert.Contains(t, resp.Error.Message, "unknown plan operation")
}

func TestHandlePlan_NilParams(t *testing.T) {
	d := newPlanTestDaemon(t, &mockPlanExecutor{})
	req := &uds.Request{Params: nil}

	resp := d.handlePlan(req)

	assert.False(t, resp.Success)
	assert.Equal(t, uds.ErrCodeValidation, resp.Error.Code)
}

func TestHandlePlan_ErrorAllOperations(t *testing.T) {
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
			pe := &mockPlanExecutor{}
			setup(pe)
			d := newPlanTestDaemon(t, pe)
			req := makePlanRequest(t, op, nil)

			resp := d.handlePlan(req)

			assert.False(t, resp.Success, "operation %s should fail", op)
			assert.Equal(t, uds.ErrCodeInternal, resp.Error.Code)
			assert.Contains(t, resp.Error.Message, "some error")
		})
	}
}

func TestSetPlanExecutor(t *testing.T) {
	d := newTestDaemon(t)
	assert.Nil(t, d.planExecutor)

	pe := &mockPlanExecutor{}
	d.SetPlanExecutor(pe)
	assert.NotNil(t, d.planExecutor)
}
