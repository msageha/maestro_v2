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

func TestStartupDialogKeys_BypassPermissionsSelectsAccept(t *testing.T) {
	t.Parallel()
	content := `
  WARNING: Claude Code running in
  Bypass Permissions mode

	  ❯ 1. No, exit
	    2. Yes, I accept
	`
	got := startupDialogKeys(content)
	want := []string{"2", "Enter"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("startupDialogKeys() = %#v, want %#v", got, want)
	}
}

func TestStartupDialogKeys_DefaultTrustDialogEnterOnly(t *testing.T) {
	t.Parallel()
	content := `Is this a project you created or one you trust?`
	got := startupDialogKeys(content)
	want := []string{"Enter"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("startupDialogKeys() = %#v, want %#v", got, want)
	}
}

// TestStartupDialogKeys_NoKnownDialogSendsNothing locks in the fail-closed
// behavior (Report 2026-06-10 P0-3): when no known dialog marker is visible
// there is NO blind Enter fallback. A periodic Enter against a running agent
// submits empty input, and against the Bypass Permissions confirmation it
// selects the default "No, exit" and terminates the agent.
func TestStartupDialogKeys_NoKnownDialogSendsNothing(t *testing.T) {
	t.Parallel()
	for _, content := range []string{
		`Claude prompt is ready`,
		``,
		`some unrelated shell output`,
	} {
		if got := startupDialogKeys(content); got != nil {
			t.Fatalf("startupDialogKeys(%q) = %#v, want nil (fail closed)", content, got)
		}
	}
}

func TestStartupDialogKeys_ReadyPromptSuppressesStaleDialogScrollback(t *testing.T) {
	t.Parallel()
	content := `
  WARNING: Claude Code running in
  Bypass Permissions mode

  ╭─── Claude Code v2.1.96 ─╮
  │ Welcome back mzk!       │
  ╰─────────────────────────╯
  ❯
  ⏵⏵ bypass permissions on (shift+tab to cycle)
`
	if got := startupDialogKeys(content); got != nil {
		t.Fatalf("startupDialogKeys() = %#v, want nil once prompt is ready", got)
	}
	if startupDialogVisible(content) {
		t.Fatal("startupDialogVisible() = true, want false once prompt is ready")
	}
}

func TestStartupDialogKeys_TrustDialogWrappedText(t *testing.T) {
	t.Parallel()
	content := "Is this a project\n       you created or one you trust?"
	got := startupDialogKeys(content)
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
