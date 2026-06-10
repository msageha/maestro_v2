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

// TestStartupDialogKeys_EnterpriseManagedReadyMarkers locks in readiness
// detection for environments where managed settings disable bypass
// permissions mode: the footer never reads "bypass permissions on" (it shows
// "accept edits on" plus a "Bypass permissions mode was disabled by settings"
// notice instead). Before this fix, readiness was unreachable in such
// environments and any dialog text lingering in the capture kept healthy
// panes classified as "stuck at startup dialog" until formation rollback
// (2026-06-10 E2E finding).
func TestStartupDialogKeys_EnterpriseManagedReadyMarkers(t *testing.T) {
	t.Parallel()
	contents := map[string]string{
		"accept edits footer": `
  ╭─── Claude Code v2.1.153 ─╮
  │ Welcome back mzk!        │
  ╰──────────────────────────╯
  ❯
  ⏵⏵ accept edits on (shift+tab to cycle)
`,
		"bypass disabled notice": `
  ❯
  Bypass permissions mode was disabled by settings
`,
		"plan mode footer": `
  ❯
  ⏸ plan mode on (shift+tab to cycle)
`,
		"stale trust dialog above ready footer": `
  Is this a project you created or one you trust?
  ❯ 1. Yes, I trust this folder
  ╭─── Claude Code v2.1.153 ─╮
  │ Welcome back mzk!        │
  ╰──────────────────────────╯
  ❯
  ⏵⏵ accept edits on (shift+tab to cycle)
`,
	}
	for name, content := range contents {
		if got := startupDialogKeys(content); got != nil {
			t.Errorf("%s: startupDialogKeys() = %#v, want nil (ready)", name, got)
		}
		if startupDialogVisible(content) {
			t.Errorf("%s: startupDialogVisible() = true, want false (ready)", name)
		}
	}
}

// TestStartupDialogKeys_StaleReadyFooterAboveActiveDialog locks in the
// position-based disambiguation: when an agent is relaunched in a pane whose
// visible screen still shows the PREVIOUS instance's ready footer, the fresh
// startup dialog renders BELOW that stale footer. The dialog marker appearing
// after the ready marker must win — the dialog is the current state and needs
// its keys, otherwise auto-accept never fires and checkAgentsLaunched cannot
// classify the pane as waiting (codex review P1, 2026-06-10).
func TestStartupDialogKeys_StaleReadyFooterAboveActiveDialog(t *testing.T) {
	t.Parallel()
	bypassBelow := `
  ❯
  ⏵⏵ accept edits on (shift+tab to cycle)
  WARNING: Claude Code running in
  Bypass Permissions mode
  ❯ 1. No, exit
    2. Yes, I accept
`
	if got, want := startupDialogKeys(bypassBelow), []string{"2", "Enter"}; !reflect.DeepEqual(got, want) {
		t.Errorf("bypass below stale footer: startupDialogKeys() = %#v, want %#v", got, want)
	}
	if !startupDialogVisible(bypassBelow) {
		t.Error("bypass below stale footer: startupDialogVisible() = false, want true")
	}

	trustBelow := `
  ⏵⏵ bypass permissions on (shift+tab to cycle)
  Is this a project you created or one you trust?
  ❯ 1. Yes, I trust this folder
`
	if got, want := startupDialogKeys(trustBelow), []string{"Enter"}; !reflect.DeepEqual(got, want) {
		t.Errorf("trust below stale footer: startupDialogKeys() = %#v, want %#v", got, want)
	}
	if !startupDialogVisible(trustBelow) {
		t.Error("trust below stale footer: startupDialogVisible() = false, want true")
	}
}
