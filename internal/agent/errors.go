package agent

import "errors"

// Sentinel errors for process management and message delivery.
// These enable callers and tests to use errors.Is for robust error checking.
var (
	// Process management errors (ClaudeProcessManager)
	ErrControlChars      = errors.New("working dir contains control characters")
	ErrRespawnPane       = errors.New("respawn pane")
	ErrRelaunch          = errors.New("re-launch claude")
	ErrWaitClaudeReady   = errors.New("wait for claude ready")
	ErrCheckPaneCommand  = errors.New("check pane command")
	ErrNotStable   = errors.New("pane content not stable")
	ErrCapturePane = errors.New("capture pane")
	// ErrNoPrompt is returned by waitStable when the pane is stable but no
	// prompt marker is found. Used in strict mode (softPromptCheck=false).
	ErrNoPrompt = errors.New("no prompt detected")
	// ErrPromptNotDetected is returned by waitReadyStrict when the prompt
	// readiness polling exhausts all retries. Used after re-launching Claude.
	ErrPromptNotDetected = errors.New("prompt not detected")
	ErrConsecutiveErrors = errors.New("consecutive errors")

	// Message delivery errors (messageDeliverer)
	ErrClearSendFailed   = errors.New("send /clear failed")
	ErrClearNotConfirmed = errors.New("/clear not confirmed")
	ErrSecondEnterFailed = errors.New("send second Enter failed")
)
