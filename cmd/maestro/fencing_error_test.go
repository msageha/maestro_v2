package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/uds"
)

func TestClassifyFencingExitCode_StructuredKindWins(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		kind string
		code string
		want int
	}{
		{"epoch via Kind", "fencing_epoch_mismatch", uds.ErrCodeFencingReject, ExitCodeFencingEpoch},
		{"status via Kind", "fencing_status_mismatch", uds.ErrCodeFencingReject, ExitCodeFencingStatus},
		{"max_runtime via Kind", "max_runtime_exceeded", uds.ErrCodeMaxRuntimeExceeded, ExitCodeMaxRuntimeExceeded},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			resp := uds.ErrorResponseWithDetails(tc.code, "msg", uds.FencingDetails{Kind: tc.kind})
			if got := classifyFencingExitCode(resp); got != tc.want {
				t.Errorf("classifyFencingExitCode = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestClassifyFencingExitCode_FallsBackToLegacyCode(t *testing.T) {
	t.Parallel()
	// No structured Details — must fall back to UDS error code mapping so
	// older daemons keep working.
	cases := []struct {
		code string
		want int
	}{
		{uds.ErrCodeFencingRejectEpoch, ExitCodeFencingEpoch},
		{uds.ErrCodeFencingRejectStatus, ExitCodeFencingStatus},
		{uds.ErrCodeFencingReject, ExitCodeFencingStatus},
		{uds.ErrCodeMaxRuntimeExceeded, ExitCodeMaxRuntimeExceeded},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.code, func(t *testing.T) {
			t.Parallel()
			resp := uds.ErrorResponse(tc.code, "msg")
			if got := classifyFencingExitCode(resp); got != tc.want {
				t.Errorf("classifyFencingExitCode = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestClassifyFencingExitCode_NonFencing(t *testing.T) {
	t.Parallel()
	if got := classifyFencingExitCode(uds.ErrorResponse(uds.ErrCodeInternal, "boom")); got != 0 {
		t.Errorf("non-fencing code = %d, want 0", got)
	}
	if got := classifyFencingExitCode(nil); got != 0 {
		t.Errorf("nil response = %d, want 0", got)
	}
	if got := classifyFencingExitCode(&uds.Response{Success: true}); got != 0 {
		t.Errorf("success response = %d, want 0", got)
	}
}

func TestEmitFencingStderrEnvelope_WritesSingleLine(t *testing.T) {
	t.Parallel()
	resp := uds.ErrorResponseWithDetails(uds.ErrCodeFencingRejectEpoch, "epoch mismatch",
		uds.FencingDetails{
			Kind:          "fencing_epoch_mismatch",
			TaskID:        "task_x",
			WorkerID:      "worker1",
			CurrentEpoch:  5,
			RequestEpoch:  3,
			CurrentStatus: "in_progress",
		})
	var buf bytes.Buffer
	if !emitFencingStderrEnvelope(resp, &buf) {
		t.Fatal("expected envelope to be emitted")
	}
	out := buf.String()
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("expected trailing newline, got %q", out)
	}
	line := strings.TrimSuffix(out, "\n")
	if strings.Count(line, "\n") != 0 {
		t.Errorf("expected single line, got %q", out)
	}

	var env fencingErrorEnvelope
	if err := json.Unmarshal([]byte(line), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if env.Code != uds.ErrCodeFencingRejectEpoch {
		t.Errorf("code = %q, want %q", env.Code, uds.ErrCodeFencingRejectEpoch)
	}
	if env.Details.CurrentEpoch != 5 || env.Details.RequestEpoch != 3 {
		t.Errorf("details round-trip mismatch: %+v", env.Details)
	}

	// Discriminator key must be `maestro_error` so worker.md's
	// `grep -F maestro_error` and `jq .maestro_error` patterns keep working.
	if !strings.Contains(line, `"maestro_error":"`+uds.ErrCodeFencingRejectEpoch+`"`) {
		t.Errorf("expected maestro_error discriminator with code value, got %q", line)
	}
}

func TestEmitFencingStderrEnvelope_NoDetails(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	resp := uds.ErrorResponse(uds.ErrCodeInternal, "boom")
	if emitFencingStderrEnvelope(resp, &buf) {
		t.Error("envelope should not be emitted when Details are absent")
	}
	if buf.Len() != 0 {
		t.Errorf("buffer must be empty, got %q", buf.String())
	}
}
