package tmux

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// testContentHash returns a hex SHA-256 hash of s (local version for this test file).
func testContentHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h)
}

// --- Data types ---

type submissionEntry struct {
	ID        int
	Timestamp string
	Kind      string // "TYPED" or "PASTE"
	Content   string
}

// --- Helper functions ---

// buildInputRecorder compiles the inputrecorder test binary and returns its path.
func buildInputRecorder(t *testing.T) string {
	t.Helper()
	binPath := filepath.Join(t.TempDir(), "inputrecorder")

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot determine caller for inputrecorder source")
	}
	srcDir := filepath.Join(filepath.Dir(thisFile), "testdata", "inputrecorder")

	cmd := exec.Command("go", "build", "-o", binPath, ".")
	cmd.Dir = srcDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build inputrecorder: %v\n%s", err, out)
	}
	return binPath
}

// createTestSession creates a uniquely-named tmux session for test isolation.
// Returns the session name. Cleanup is registered automatically.
func createTestSession(t *testing.T, name string) string {
	t.Helper()
	sessionName := fmt.Sprintf("test-input-%s-%d", name, time.Now().UnixNano()%100000)
	cmd := exec.Command("tmux", "new-session", "-d", "-s", sessionName, "-n", "test", "-x", "120", "-y", "40")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create test session %s: %v\n%s", sessionName, err, out)
	}
	t.Cleanup(func() {
		exec.Command("tmux", "kill-session", "-t", sessionName).Run()
	})
	return sessionName
}

// waitForOutput polls the pane until the marker string appears or timeout expires.
func waitForOutput(t *testing.T, paneTarget, marker string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		content, err := CapturePane(paneTarget, 30)
		if err == nil && strings.Contains(content, marker) {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

// parseSubmissionLog reads the inputrecorder log file and returns parsed entries.
func parseSubmissionLog(t *testing.T, logPath string) []submissionEntry {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read submission log %s: %v", logPath, err)
	}

	var entries []submissionEntry
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 4)
		if len(parts) < 4 {
			t.Logf("skipping malformed log line: %q", line)
			continue
		}
		id := 0
		fmt.Sscanf(parts[0], "%d", &id)
		content := parts[3]
		// Opportunistically unquote %q-encoded content (handles both old and new format)
		if unquoted, err := strconv.Unquote(content); err == nil {
			content = unquoted
		}
		entries = append(entries, submissionEntry{
			ID:        id,
			Timestamp: parts[1],
			Kind:      parts[2],
			Content:   content,
		})
	}
	return entries
}

// verifySubmissionsNotMerged checks that command and instruction appear in separate submissions.
func verifySubmissionsNotMerged(t *testing.T, entries []submissionEntry, command, instruction string) {
	t.Helper()

	if len(entries) < 2 {
		t.Errorf("BUG DETECTED: expected at least 2 submissions, got %d — inputs likely merged", len(entries))
		for i, e := range entries {
			t.Logf("  entry[%d]: kind=%s content=%q", i, e.Kind, e.Content)
		}
		return
	}

	foundCommand := false
	foundInstruction := false
	for _, e := range entries {
		if strings.Contains(e.Content, command) {
			foundCommand = true
			if strings.Contains(e.Content, instruction) {
				t.Errorf("BUG DETECTED: submission %d contains BOTH %q and %q — inputs merged\n  content=%q",
					e.ID, command, instruction, e.Content)
			}
		}
		if strings.Contains(e.Content, instruction) {
			foundInstruction = true
		}
	}

	if !foundCommand {
		t.Errorf("command %q not found in any submission", command)
	}
	if !foundInstruction {
		t.Errorf("instruction %q not found in any submission", instruction)
	}
}

// --- Layer 1: cat tests ---

func TestInputSeparation_Cat_BasicSeparation(t *testing.T) {
	requireTmux(t)

	session := createTestSession(t, "cat-basic")
	pane := session + ":0.0"

	// Wait for shell
	waitForShell(t, pane)

	// Start cat
	if err := SendCommand(pane, "cat"); err != nil {
		t.Fatalf("start cat: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	// Send /clear via send-keys (same as execWithClear Step 2)
	if err := SendCommand(pane, "/clear"); err != nil {
		t.Fatalf("send /clear: %v", err)
	}

	// Cooldown (same as CooldownAfterClear default)
	time.Sleep(3 * time.Second)

	// Send instruction text via paste-buffer (same as sendAndConfirm)
	instruction := "INSTRUCTION_MARKER_LINE1\nINSTRUCTION_LINE2"
	if err := SendTextAndSubmit(context.Background(), pane, instruction); err != nil {
		t.Fatalf("send instruction: %v", err)
	}
	time.Sleep(1 * time.Second)

	// Capture and verify
	content, err := CapturePane(pane, 30)
	if err != nil {
		t.Fatalf("capture pane: %v", err)
	}
	t.Logf("pane content:\n%s", content)

	// /clear and instruction text should appear on separate lines
	lines := strings.Split(content, "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.Contains(trimmed, "/clear") && strings.Contains(trimmed, "INSTRUCTION_MARKER") {
			t.Errorf("BUG DETECTED: /clear and instruction on same line: %q", trimmed)
		}
	}

	if !strings.Contains(content, "/clear") {
		t.Error("/clear not found in pane content")
	}
	if !strings.Contains(content, "INSTRUCTION_MARKER_LINE1") {
		t.Error("INSTRUCTION_MARKER_LINE1 not found in pane content")
	}
}

func TestInputSeparation_Cat_TimingGaps(t *testing.T) {
	requireTmux(t)

	gaps := []struct {
		name string
		gap  time.Duration
	}{
		{"0ms", 0},
		{"100ms", 100 * time.Millisecond},
		{"500ms", 500 * time.Millisecond},
		{"3s", 3 * time.Second},
	}

	for _, tc := range gaps {
		t.Run(tc.name, func(t *testing.T) {
			session := createTestSession(t, "cat-gap-"+tc.name)
			pane := session + ":0.0"
			waitForShell(t, pane)

			// Start cat
			if err := SendCommand(pane, "cat"); err != nil {
				t.Fatalf("start cat: %v", err)
			}
			time.Sleep(500 * time.Millisecond)

			// Send /clear
			if err := SendCommand(pane, "/clear"); err != nil {
				t.Fatalf("send /clear: %v", err)
			}

			// Variable gap
			if tc.gap > 0 {
				time.Sleep(tc.gap)
			}

			// Send instruction
			marker := fmt.Sprintf("INSTRUCTION_%s", tc.name)
			if err := SendTextAndSubmit(context.Background(), pane, marker); err != nil {
				t.Fatalf("send instruction: %v", err)
			}
			time.Sleep(1 * time.Second)

			content, err := CapturePane(pane, 30)
			if err != nil {
				t.Fatalf("capture: %v", err)
			}
			t.Logf("pane content (gap=%s):\n%s", tc.name, content)

			// Verify separation
			for _, line := range strings.Split(content, "\n") {
				trimmed := strings.TrimSpace(line)
				if strings.Contains(trimmed, "/clear") && strings.Contains(trimmed, marker) {
					t.Errorf("BUG: /clear and instruction merged on line: %q", trimmed)
				}
			}

			if !strings.Contains(content, "/clear") {
				t.Error("/clear not found")
			}
			if !strings.Contains(content, marker) {
				t.Errorf("instruction %q not found", marker)
			}
		})
	}
}

// --- Layer 2: inputrecorder bracketed paste tests ---

func TestInputSeparation_BracketedPaste_BasicSeparation(t *testing.T) {
	requireTmux(t)

	bin := buildInputRecorder(t)
	logPath := filepath.Join(t.TempDir(), "submissions.log")

	session := createTestSession(t, "bp-basic")
	pane := session + ":0.0"
	waitForShell(t, pane)

	// Start inputrecorder
	if err := SendCommand(pane, fmt.Sprintf("%s %s 0", bin, logPath)); err != nil {
		t.Fatalf("start inputrecorder: %v", err)
	}

	// Wait for READY>
	if !waitForOutput(t, pane, "READY>", 5*time.Second) {
		t.Fatal("inputrecorder did not start (READY> not found)")
	}

	// Send /clear via send-keys
	if err := SendKeys(pane, "/clear", "Enter"); err != nil {
		t.Fatalf("send /clear: %v", err)
	}

	// Wait for /clear to be processed
	if !waitForOutput(t, pane, "RECV[1]:", 5*time.Second) {
		t.Log("WARNING: RECV[1] not detected in pane, proceeding")
	}

	// Cooldown (same as CooldownAfterClear)
	time.Sleep(3 * time.Second)

	// Send instruction via paste-buffer
	if err := SendTextAndSubmit(context.Background(), pane, "INSTRUCTION_BASIC\nLINE2\nLINE3"); err != nil {
		t.Fatalf("send instruction: %v", err)
	}

	// Wait for processing
	time.Sleep(2 * time.Second)

	// Parse log and verify
	entries := parseSubmissionLog(t, logPath)
	t.Logf("submissions: %d entries", len(entries))
	for i, e := range entries {
		t.Logf("  [%d] kind=%s content=%q", i, e.Kind, e.Content)
	}

	verifySubmissionsNotMerged(t, entries, "/clear", "INSTRUCTION_BASIC")

	// Verify kinds
	for _, e := range entries {
		if strings.Contains(e.Content, "/clear") && e.Kind != "TYPED" {
			t.Errorf("expected /clear to be TYPED, got %s", e.Kind)
		}
	}
}

func TestInputSeparation_BracketedPaste_WithProcessingDelay(t *testing.T) {
	requireTmux(t)

	bin := buildInputRecorder(t)
	logPath := filepath.Join(t.TempDir(), "submissions.log")

	session := createTestSession(t, "bp-delay")
	pane := session + ":0.0"
	waitForShell(t, pane)

	// Start inputrecorder with 2s processing delay
	if err := SendCommand(pane, fmt.Sprintf("%s %s 2000", bin, logPath)); err != nil {
		t.Fatalf("start inputrecorder: %v", err)
	}
	if !waitForOutput(t, pane, "READY>", 5*time.Second) {
		t.Fatal("inputrecorder did not start")
	}

	// Send /clear
	if err := SendKeys(pane, "/clear", "Enter"); err != nil {
		t.Fatalf("send /clear: %v", err)
	}

	// CooldownAfterClear — the app is still processing /clear (2s delay)
	time.Sleep(3 * time.Second)

	// Send instruction
	if err := SendTextAndSubmit(context.Background(), pane, "INSTRUCTION_DELAYED"); err != nil {
		t.Fatalf("send instruction: %v", err)
	}

	// Wait for all processing (2s delay for each submission)
	time.Sleep(5 * time.Second)

	entries := parseSubmissionLog(t, logPath)
	t.Logf("submissions: %d entries", len(entries))
	for i, e := range entries {
		t.Logf("  [%d] kind=%s content=%q", i, e.Kind, e.Content)
	}

	verifySubmissionsNotMerged(t, entries, "/clear", "INSTRUCTION_DELAYED")
}

func TestInputSeparation_BracketedPaste_RapidFire(t *testing.T) {
	requireTmux(t)

	bin := buildInputRecorder(t)
	logPath := filepath.Join(t.TempDir(), "submissions.log")

	session := createTestSession(t, "bp-rapid")
	pane := session + ":0.0"
	waitForShell(t, pane)

	// Start inputrecorder with 5s processing delay (app is "stuck")
	if err := SendCommand(pane, fmt.Sprintf("%s %s 5000", bin, logPath)); err != nil {
		t.Fatalf("start inputrecorder: %v", err)
	}
	if !waitForOutput(t, pane, "READY>", 5*time.Second) {
		t.Fatal("inputrecorder did not start")
	}

	// Send /clear — no delay before paste!
	if err := SendKeys(pane, "/clear", "Enter"); err != nil {
		t.Fatalf("send /clear: %v", err)
	}

	// Immediately send instruction (0ms gap)
	if err := SendTextAndSubmit(context.Background(), pane, "INSTRUCTION_RAPID"); err != nil {
		t.Fatalf("send instruction: %v", err)
	}

	// Wait for all processing (5s delay × 2 submissions + buffer)
	time.Sleep(12 * time.Second)

	entries := parseSubmissionLog(t, logPath)
	t.Logf("submissions (rapid fire): %d entries", len(entries))
	for i, e := range entries {
		t.Logf("  [%d] kind=%s content=%q", i, e.Kind, e.Content)
	}

	// This is the key test: even with 0ms gap and 5s processing delay,
	// are the inputs still separated at the pty/application level?
	if len(entries) < 2 {
		t.Errorf("FINDING: rapid fire produced %d submission(s) instead of 2 — stdin batching confirmed", len(entries))
		for i, e := range entries {
			t.Logf("  merged entry[%d]: kind=%s content=%q", i, e.Kind, e.Content)
		}
	} else {
		verifySubmissionsNotMerged(t, entries, "/clear", "INSTRUCTION_RAPID")
		t.Log("rapid fire: inputs correctly separated despite 0ms gap and processing delay")
	}
}

// --- Layer 3: execWithClear flow simulation ---

func TestInputSeparation_ExecWithClearFlow_BashPrompt(t *testing.T) {
	requireTmux(t)

	session := createTestSession(t, "flow-bash")
	pane := session + ":0.0"
	waitForShell(t, pane)

	// Set custom prompt so isPromptReady would detect it
	if err := SendCommand(pane, `export PS1="❯ "`); err != nil {
		t.Fatalf("set PS1: %v", err)
	}
	time.Sleep(1 * time.Second)

	// Step 1: Simulate waitReady — verify prompt is visible
	content, err := CapturePane(pane, 10)
	if err != nil {
		t.Fatalf("capture for prompt check: %v", err)
	}
	if !strings.Contains(content, "❯") {
		t.Log("WARNING: prompt ❯ not found in initial capture, proceeding")
	}

	// Step 2: SendCommand("/clear") — bash will show "command not found"
	if err := SendCommand(pane, "/clear"); err != nil {
		t.Fatalf("send /clear: %v", err)
	}

	// Step 3: CooldownAfterClear
	time.Sleep(3 * time.Second)

	// Step 4: Simulate waitStable — hash comparison
	content1, err := CapturePaneJoined(pane, 10)
	if err != nil {
		t.Fatalf("capture 1: %v", err)
	}
	h1 := testContentHash(content1)
	time.Sleep(2 * time.Second)
	content2, err := CapturePaneJoined(pane, 10)
	if err != nil {
		t.Fatalf("capture 2: %v", err)
	}
	h2 := testContentHash(content2)
	if h1 != h2 {
		t.Log("WARNING: pane content not stable after /clear, hashes differ")
	}

	// Step 5: Simulate detectBusy — check pane_current_command
	cmd, err := GetPaneCurrentCommand(pane)
	if err != nil {
		t.Fatalf("get pane command: %v", err)
	}
	t.Logf("pane_current_command: %s", cmd)

	// Step 6: sendAndConfirm — paste instruction
	instruction := "echo FLOW_TEST_MARKER"
	if err := SendTextAndSubmit(context.Background(), pane, instruction); err != nil {
		t.Fatalf("send instruction: %v", err)
	}
	time.Sleep(2 * time.Second)

	// Verify
	finalContent, err := CapturePane(pane, 30)
	if err != nil {
		t.Fatalf("final capture: %v", err)
	}
	t.Logf("final pane content:\n%s", finalContent)

	// /clear should have produced "command not found" or similar
	// instruction should have produced "FLOW_TEST_MARKER"
	if !strings.Contains(finalContent, "FLOW_TEST_MARKER") {
		t.Error("instruction marker not found in pane output")
	}

	// Check they're not on the same command line
	for _, line := range strings.Split(finalContent, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.Contains(trimmed, "/clear") && strings.Contains(trimmed, "FLOW_TEST_MARKER") {
			t.Errorf("BUG: /clear and instruction merged on same line: %q", trimmed)
		}
	}
}

func TestInputSeparation_ExecWithClearFlow_InputRecorder(t *testing.T) {
	requireTmux(t)

	bin := buildInputRecorder(t)
	logPath := filepath.Join(t.TempDir(), "submissions.log")

	session := createTestSession(t, "flow-recorder")
	pane := session + ":0.0"
	waitForShell(t, pane)

	// Start inputrecorder with 500ms processing delay
	if err := SendCommand(pane, fmt.Sprintf("%s %s 500", bin, logPath)); err != nil {
		t.Fatalf("start inputrecorder: %v", err)
	}
	if !waitForOutput(t, pane, "READY>", 5*time.Second) {
		t.Fatal("inputrecorder did not start")
	}

	// Simulate full execWithClear timing:
	// Step 1: waitReady — prompt detected (READY> present)
	// Step 2: SendCommand("/clear")
	if err := SendKeys(pane, "/clear", "Enter"); err != nil {
		t.Fatalf("send /clear: %v", err)
	}

	// Step 3: CooldownAfterClear (3s)
	time.Sleep(3 * time.Second)

	// Step 4: waitStable — hash comparison (simplified: just check stability)
	c1, _ := CapturePaneJoined(pane, 10)
	time.Sleep(2 * time.Second)
	c2, _ := CapturePaneJoined(pane, 10)
	if testContentHash(c1) != testContentHash(c2) {
		t.Log("WARNING: pane not stable after /clear")
	}

	// Step 5: detectBusy — skip (inputrecorder is not a shell)

	// Step 6: sendAndConfirm
	multiLineInstruction := "[maestro] task_id:task_001 command_id:cmd_001\npurpose: Test task\ncontent: Do something useful\nacceptance_criteria: Tests pass"
	if err := SendTextAndSubmit(context.Background(), pane, multiLineInstruction); err != nil {
		t.Fatalf("send instruction: %v", err)
	}

	time.Sleep(3 * time.Second)

	entries := parseSubmissionLog(t, logPath)
	t.Logf("submissions (flow): %d entries", len(entries))
	for i, e := range entries {
		t.Logf("  [%d] kind=%s content=%q", i, e.Kind, e.Content)
	}

	verifySubmissionsNotMerged(t, entries, "/clear", "[maestro]")
}

// --- Layer 4: stress tests ---

func TestInputSeparation_ConcurrentPanes(t *testing.T) {
	requireTmux(t)

	bin := buildInputRecorder(t)
	const numPanes = 4

	session := createTestSession(t, "concurrent")
	panes := make([]string, numPanes)
	logPaths := make([]string, numPanes)

	// First pane already exists
	panes[0] = session + ":0.0"
	logPaths[0] = filepath.Join(t.TempDir(), "sub_0.log")

	// Create additional panes via split
	for i := 1; i < numPanes; i++ {
		if err := SplitPane(session+":0", i%2 == 1); err != nil {
			t.Fatalf("split pane %d: %v", i, err)
		}
		logPaths[i] = filepath.Join(t.TempDir(), fmt.Sprintf("sub_%d.log", i))
	}

	// Get actual pane targets
	allPanes, err := ListPanes(session+":0", "#{session_name}:#{window_index}.#{pane_index}")
	if err != nil {
		t.Fatalf("list panes: %v", err)
	}
	if len(allPanes) < numPanes {
		t.Fatalf("expected %d panes, got %d", numPanes, len(allPanes))
	}
	for i := 0; i < numPanes; i++ {
		panes[i] = strings.TrimSpace(allPanes[i])
	}

	// Start inputrecorder in each pane
	for i := 0; i < numPanes; i++ {
		waitForShell(t, panes[i])
		if err := SendCommand(panes[i], fmt.Sprintf("%s %s 0", bin, logPaths[i])); err != nil {
			t.Fatalf("start inputrecorder in pane %d: %v", i, err)
		}
	}

	// Wait for all to be ready
	for i := 0; i < numPanes; i++ {
		if !waitForOutput(t, panes[i], "READY>", 5*time.Second) {
			t.Fatalf("pane %d inputrecorder did not start", i)
		}
	}

	// Concurrent dispatch to all panes
	var wg sync.WaitGroup
	for i := 0; i < numPanes; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			// Send /clear
			if err := SendKeys(panes[idx], "/clear", "Enter"); err != nil {
				t.Errorf("pane %d send /clear: %v", idx, err)
				return
			}
			time.Sleep(3 * time.Second)

			// Send instruction
			msg := fmt.Sprintf("INSTRUCTION_PANE_%d", idx)
			if err := SendTextAndSubmit(context.Background(), panes[idx], msg); err != nil {
				t.Errorf("pane %d send instruction: %v", idx, err)
			}
		}(i)
	}
	wg.Wait()
	time.Sleep(2 * time.Second)

	// Verify each pane
	for i := 0; i < numPanes; i++ {
		entries := parseSubmissionLog(t, logPaths[i])
		t.Logf("pane %d: %d entries", i, len(entries))
		for j, e := range entries {
			t.Logf("  pane[%d][%d] kind=%s content=%q", i, j, e.Kind, e.Content)
		}

		marker := fmt.Sprintf("INSTRUCTION_PANE_%d", i)
		verifySubmissionsNotMerged(t, entries, "/clear", marker)

		// Check no cross-pane contamination
		for _, e := range entries {
			for other := 0; other < numPanes; other++ {
				if other != i {
					otherMarker := fmt.Sprintf("INSTRUCTION_PANE_%d", other)
					if strings.Contains(e.Content, otherMarker) {
						t.Errorf("CROSS-PANE: pane %d contains pane %d's marker: %q", i, other, e.Content)
					}
				}
			}
		}
	}
}

func TestInputSeparation_RepeatedDispatches(t *testing.T) {
	requireTmux(t)

	bin := buildInputRecorder(t)
	logPath := filepath.Join(t.TempDir(), "submissions.log")

	session := createTestSession(t, "repeated")
	pane := session + ":0.0"
	waitForShell(t, pane)

	// Start inputrecorder with 100ms delay
	if err := SendCommand(pane, fmt.Sprintf("%s %s 100", bin, logPath)); err != nil {
		t.Fatalf("start inputrecorder: %v", err)
	}
	if !waitForOutput(t, pane, "READY>", 5*time.Second) {
		t.Fatal("inputrecorder did not start")
	}

	const iterations = 5
	for i := 0; i < iterations; i++ {
		// Wait for READY> before each dispatch
		if i > 0 {
			readyMarker := fmt.Sprintf("RECV[%d]:", i*2)
			if !waitForOutput(t, pane, readyMarker, 5*time.Second) {
				t.Logf("WARNING: RECV marker for iteration %d not found, proceeding", i)
			}
			time.Sleep(500 * time.Millisecond)
		}

		// Send /clear
		if err := SendKeys(pane, "/clear", "Enter"); err != nil {
			t.Fatalf("iter %d send /clear: %v", i, err)
		}
		time.Sleep(1 * time.Second)

		// Send instruction
		msg := fmt.Sprintf("TASK_%d_INSTRUCTION", i)
		if err := SendTextAndSubmit(context.Background(), pane, msg); err != nil {
			t.Fatalf("iter %d send instruction: %v", i, err)
		}
		time.Sleep(1 * time.Second)
	}

	time.Sleep(2 * time.Second)

	entries := parseSubmissionLog(t, logPath)
	t.Logf("total submissions: %d (expected %d)", len(entries), iterations*2)
	for i, e := range entries {
		t.Logf("  [%d] kind=%s content=%q", i, e.Kind, e.Content)
	}

	if len(entries) < iterations*2 {
		t.Errorf("expected %d submissions, got %d — some inputs were merged", iterations*2, len(entries))
	}

	// Verify no /clear is merged with any task instruction
	for _, e := range entries {
		if strings.Contains(e.Content, "/clear") {
			for j := 0; j < iterations; j++ {
				taskMarker := fmt.Sprintf("TASK_%d_INSTRUCTION", j)
				if strings.Contains(e.Content, taskMarker) {
					t.Errorf("BUG: /clear merged with %s in submission %d: %q", taskMarker, e.ID, e.Content)
				}
			}
		}
	}
}
