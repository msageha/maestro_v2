package uds

import (
	"encoding/json"
	"testing"
)

// TestErrorResponseWithDetails_RoundTrip pins the F-019 wire shape: the
// Details payload must round-trip as a structured JSON object so CLI
// consumers (and the Worker shell wrapper through them) can read fields
// like current_epoch / current_status by JSON path instead of grepping
// Message.
func TestErrorResponseWithDetails_RoundTrip(t *testing.T) {
	t.Parallel()
	want := FencingDetails{
		Kind:          "fencing_epoch_mismatch",
		TaskID:        "task_test",
		WorkerID:      "worker1",
		CurrentEpoch:  5,
		RequestEpoch:  3,
		CurrentStatus: "in_progress",
	}
	resp := ErrorResponseWithDetails(ErrCodeFencingRejectEpoch, "epoch mismatch", want)

	if resp.Success {
		t.Fatal("Success must be false on error response")
	}
	if resp.Error.Code != ErrCodeFencingRejectEpoch {
		t.Errorf("Code = %q", resp.Error.Code)
	}
	raw := resp.ErrorDetails()
	if len(raw) == 0 {
		t.Fatal("ErrorDetails empty — Details was not attached")
	}

	var got FencingDetails
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal Details: %v", err)
	}
	if got != want {
		t.Errorf("Details round-trip mismatch: got %+v, want %+v", got, want)
	}
}

// TestErrorResponseWithDetails_NilDetails ensures that callers passing a nil
// payload fall back to the plain ErrorResponse shape — older clients that
// always read Message keep working unchanged.
func TestErrorResponseWithDetails_NilDetails(t *testing.T) {
	t.Parallel()
	resp := ErrorResponseWithDetails(ErrCodeInternal, "boom", nil)
	if resp.Error.Details != nil {
		t.Errorf("expected Details to be nil for nil payload, got %s", string(resp.Error.Details))
	}
}

// TestResponse_ErrorDetails_NoErrorOrNilResponse asserts the helper handles
// the boundary cases used by CLI defensive wrappers.
func TestResponse_ErrorDetails_NoErrorOrNilResponse(t *testing.T) {
	t.Parallel()
	if (*Response)(nil).ErrorDetails() != nil {
		t.Error("nil receiver must return nil")
	}
	if (&Response{Success: true}).ErrorDetails() != nil {
		t.Error("success response must return nil")
	}
}
