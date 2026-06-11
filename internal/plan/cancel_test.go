package plan

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/msageha/maestro_v2/internal/model"
)

func TestValidateNotCancelled(t *testing.T) {
	t.Run("not cancelled returns nil", func(t *testing.T) {
		state := &model.CommandState{CommandID: "cmd-100"}
		if err := ValidateNotCancelled(state); err != nil {
			t.Errorf("ValidateNotCancelled returned error for non-cancelled state: %v", err)
		}
	})

	t.Run("cancelled returns error", func(t *testing.T) {
		state := &model.CommandState{CommandID: "cmd-101"}
		state.Cancel.Requested = true
		err := ValidateNotCancelled(state)
		if err == nil {
			t.Fatalf("ValidateNotCancelled returned nil, want error for cancelled state")
		}
		if got := err.Error(); got == "" {
			t.Errorf("error message is empty")
		}
	})
}

func TestValidateNotCancelled_ErrorMessageContainsCommandID(t *testing.T) {
	commandID := "cmd-abc-123"
	state := &model.CommandState{
		CommandID: commandID,
		Cancel:    model.CancelState{Requested: true},
	}

	err := ValidateNotCancelled(state)
	require.Error(t, err)
	assert.Contains(t, err.Error(), commandID)
	assert.Contains(t, err.Error(), "cancelled")
}

func TestValidateNotCancelled_ErrorType(t *testing.T) {
	state := &model.CommandState{
		CommandID: "cmd-type-check",
		Cancel:    model.CancelState{Requested: true},
	}

	err := ValidateNotCancelled(state)
	require.Error(t, err)

	var pve *planValidationError
	assert.True(t, errors.As(err, &pve), "error should be *planValidationError")
	assert.Equal(t, "command cmd-type-check has been cancelled", pve.Msg)
}

func TestValidateNotCancelled_FormatStderr(t *testing.T) {
	state := &model.CommandState{
		CommandID: "cmd-stderr",
		Cancel:    model.CancelState{Requested: true},
	}

	err := ValidateNotCancelled(state)
	require.Error(t, err)

	var pve *planValidationError
	require.True(t, errors.As(err, &pve))

	stderr := pve.FormatStderr()
	assert.True(t, strings.HasPrefix(stderr, "error:"), "FormatStderr should start with 'error:'")
	assert.Contains(t, stderr, "cmd-stderr")
	assert.True(t, strings.HasSuffix(stderr, "\n"), "FormatStderr should end with newline")
}

func TestValidateNotCancelled_VariousCommandIDs(t *testing.T) {
	tests := []struct {
		name      string
		commandID string
	}{
		{name: "empty command ID", commandID: ""},
		{name: "special characters", commandID: "cmd-!@#$%^&*()"},
		{name: "unicode characters", commandID: "cmd-日本語-テスト"},
		{name: "very long ID", commandID: strings.Repeat("a", 1000)},
		{name: "spaces in ID", commandID: "cmd with spaces"},
		{name: "newlines in ID", commandID: "cmd\nwith\nnewlines"},
		{name: "single character", commandID: "x"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := &model.CommandState{
				CommandID: tt.commandID,
				Cancel:    model.CancelState{Requested: true},
			}

			err := ValidateNotCancelled(state)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.commandID)

			var pve *planValidationError
			require.True(t, errors.As(err, &pve))
		})
	}
}

func TestValidateNotCancelled_CancelMetadataDoesNotAffectValidation(t *testing.T) {
	requestedAt := "2026-04-14T10:00:00Z"
	requestedBy := "user@example.com"
	reason := "no longer needed"

	tests := []struct {
		name      string
		cancel    model.CancelState
		wantError bool
	}{
		{
			name: "all metadata set but not requested",
			cancel: model.CancelState{
				Requested:   false,
				RequestedAt: &requestedAt,
				RequestedBy: &requestedBy,
				Reason:      &reason,
			},
			wantError: false,
		},
		{
			name: "requested with all metadata",
			cancel: model.CancelState{
				Requested:   true,
				RequestedAt: &requestedAt,
				RequestedBy: &requestedBy,
				Reason:      &reason,
			},
			wantError: true,
		},
		{
			name: "requested with no metadata",
			cancel: model.CancelState{
				Requested: true,
			},
			wantError: true,
		},
		{
			name: "not requested with partial metadata",
			cancel: model.CancelState{
				Requested:   false,
				RequestedBy: &requestedBy,
			},
			wantError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := &model.CommandState{
				CommandID: "cmd-meta-test",
				Cancel:    tt.cancel,
			}

			err := ValidateNotCancelled(state)
			if tt.wantError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestValidateNotCancelled_ZeroValueCommandState(t *testing.T) {
	state := &model.CommandState{}

	err := ValidateNotCancelled(state)
	assert.NoError(t, err, "zero-value CommandState should not be considered cancelled")
}
