package formation

import (
	"reflect"
	"testing"
	"time"
)

// TestTrustDialogWindowCoversSlowStartups locks in the invariant that
// autoAcceptTrustDialog keeps sending Enter for long enough to cover the
// worst-case dialog appearance time.
//
// Claude Code can take over 30 seconds to show the trust dialog on first
// launch (binary initialization, network auth, model handshake). The window
// must be at least 60 seconds so that the dialog is caught even on slow
// machines. Regressing to a short window (e.g. 10 s) would silently leave
// all panes stuck when startup is slow.
func TestTrustDialogWindowCoversSlowStartups(t *testing.T) {
	t.Parallel()
	const minWindow = 60 * time.Second
	if trustDialogWindow < minWindow {
		t.Fatalf("trustDialogWindow = %v; must be >= %v to cover slow startups "+
			"(Claude Code initialization, network auth, model handshake). "+
			"See the constant's doc comment.",
			trustDialogWindow, minWindow)
	}
}

// TestTrustDialogSendIntervalCatchesDialogQuickly locks in the invariant that
// the auto-accept loop sends Enter frequently enough to catch the trust dialog
// within a reasonable time of it appearing.
//
// The dialog waits indefinitely for user input, so it will not disappear on
// its own — but a long send interval means the operator (or automated test)
// waits longer than necessary. The interval must be short enough that in the
// worst case (Enter arrives just after the dialog appears) the acceptance
// latency is acceptable. 10 seconds is the upper bound.
func TestTrustDialogSendIntervalCatchesDialogQuickly(t *testing.T) {
	t.Parallel()
	const maxInterval = 10 * time.Second
	if trustDialogSendInterval > maxInterval {
		t.Fatalf("trustDialogSendInterval = %v; must be <= %v so the trust "+
			"dialog is accepted quickly after it appears. A longer interval "+
			"means the operator sees the stuck pane for an unnecessarily long "+
			"time. See the constant's doc comment.",
			trustDialogSendInterval, maxInterval)
	}
}

func TestStartupDialogKeys_BypassPermissionsSelectsAcceptForManagedRoles(t *testing.T) {
	t.Parallel()
	content := `
  WARNING: Claude Code running in
  Bypass Permissions mode

	  ❯ 1. No, exit
	    2. Yes, I accept
	`
	for _, role := range []string{"orchestrator", "planner", "worker"} {
		role := role
		t.Run(role, func(t *testing.T) {
			t.Parallel()
			got := startupDialogKeys(role, content)
			want := []string{"2", "Enter"}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("startupDialogKeys() = %#v, want %#v", got, want)
			}
		})
	}
}

func TestStartupDialogKeys_DefaultTrustDialogEnterOnly(t *testing.T) {
	t.Parallel()
	content := `Is this a project you created or one you trust?`
	got := startupDialogKeys("orchestrator", content)
	want := []string{"Enter"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("startupDialogKeys() = %#v, want %#v", got, want)
	}
}

func TestStartupDialogKeys_ManagedRoleWithoutKnownDialogSendsNothing(t *testing.T) {
	t.Parallel()
	for _, role := range []string{"orchestrator", "planner", "worker"} {
		role := role
		t.Run(role, func(t *testing.T) {
			t.Parallel()
			got := startupDialogKeys(role, `Claude prompt is ready`)
			if got != nil {
				t.Fatalf("startupDialogKeys() = %#v, want nil", got)
			}
		})
	}
}

func TestStartupDialogKeys_WorkerTrustDialogEnterOnly(t *testing.T) {
	t.Parallel()
	content := `Is this a project you created or one you trust?`
	got := startupDialogKeys("worker", content)
	want := []string{"Enter"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("startupDialogKeys() = %#v, want %#v", got, want)
	}
}

func TestStartupDialogKeys_WorkerTrustDialogWrappedText(t *testing.T) {
	t.Parallel()
	content := "Is this a project\n       you created or one you trust?"
	got := startupDialogKeys("worker", content)
	want := []string{"Enter"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("startupDialogKeys() = %#v, want %#v", got, want)
	}
}

func TestStartupDialogVisibleDetectsWrappedMarkers(t *testing.T) {
	t.Parallel()
	for _, content := range []string{
		"Bypass\nPermissions   mode",
		"project\n       you created or one you trust",
	} {
		if !startupDialogVisible(content) {
			t.Fatalf("startupDialogVisible(%q) = false, want true", content)
		}
	}
}
