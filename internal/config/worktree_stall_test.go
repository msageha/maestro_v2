package config

import (
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
)

func TestWorktreeStallTimeoutMinutes_Default(t *testing.T) {
	w := model.WorktreeConfig{}
	if got := WorktreeStallTimeoutMinutes(w); got != DefaultWorktreeStallTimeoutMinutes {
		t.Fatalf("default stall timeout = %d, want %d", got, DefaultWorktreeStallTimeoutMinutes)
	}
}

func TestWorktreeStallTimeoutMinutes_Override(t *testing.T) {
	v := 5
	w := model.WorktreeConfig{StallTimeoutMinutes: &v}
	if got := WorktreeStallTimeoutMinutes(w); got != 5 {
		t.Fatalf("override stall timeout = %d, want 5", got)
	}
}

func TestWorktreeStallTimeoutMinutes_ExplicitZeroDisables(t *testing.T) {
	v := 0
	w := model.WorktreeConfig{StallTimeoutMinutes: &v}
	if got := WorktreeStallTimeoutMinutes(w); got != 0 {
		t.Fatalf("explicit-zero stall timeout = %d, want 0", got)
	}
}
