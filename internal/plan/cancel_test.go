package plan

import (
	"testing"

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
