// Package status provides command and task status display utilities.
package status

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/tmux"
	"github.com/msageha/maestro_v2/internal/uds"
	maestroyaml "github.com/msageha/maestro_v2/internal/yaml"
)

type formationStatus struct {
	Daemon  daemonStatus   `json:"daemon"`
	Agents  []agentStatus  `json:"agents,omitempty"`
	Queues  []queueStatus  `json:"queues,omitempty"`
	Signals *signalsStatus `json:"signals,omitempty"`
}

// signalsStatus surfaces planner_signals.yaml health for operators. The
// daemon's tmux meta-circuit (queue_scan_phase_b.go:recordCascadeBreakOutcome)
// runs in-memory so it cannot be inspected from outside the process; this
// status block reads the persisted queue file directly so the same shape of
// information — "are signals piling up; is something stuck retrying" — is
// available to a `maestro status` caller without an extra UDS round-trip.
type signalsStatus struct {
	// Total counts every entry in planner_signals.yaml. A non-zero value
	// after a few seconds of quiescence means deliveries are stalled.
	Total int `json:"total"`
	// MaxAttempts is the highest Attempts among current entries. Together
	// with Total it points operators at the same condition the daemon's
	// phase_b_signal_cascade_break log line surfaces (signals retried >> 0
	// times = tmux delivery degraded).
	MaxAttempts int `json:"max_attempts,omitempty"`
	// LastAttemptAt is the most recent LastAttemptAt across entries; an
	// empty value means no signal has ever been retried (fresh queue).
	LastAttemptAt string `json:"last_attempt_at,omitempty"`
	// LastError is the LastError of the entry whose LastAttemptAt is the
	// most recent. Surfaces the dominant failure reason without forcing
	// the operator to grep the full file.
	LastError string `json:"last_error,omitempty"`
	// Degraded is the file-derived approximation of the daemon's in-memory
	// `tmux_delivery_sustained_degradation` ERROR. The daemon raises that
	// log line when the per-tick cascade-break tracker has tripped 3 scan
	// ticks in a row; here we cannot read the in-memory counter without a
	// UDS extension, so we infer "delivery is stuck" from MaxAttempts >=
	// signalDegradationAttemptsThreshold. A signal that has been retried
	// that many times is, by construction, evidence that the daemon's
	// per-tick gate has been firing repeatedly. Lets `maestro status`
	// callers detect the same condition operators currently learn about
	// only by tail -f daemon.log.
	Degraded bool `json:"degraded,omitempty"`
}

// signalDegradationAttemptsThreshold is the Attempts count at which the
// status command's signalsStatus.Degraded flag flips on. Aligned to the
// daemon's per-tick signalCascadeBreakThreshold (5) so that any signal
// stuck near that boundary surfaces as degraded. Lower values would alarm
// on healthy retries; higher values would lag the actual incident.
const signalDegradationAttemptsThreshold = 5

type daemonStatus struct {
	Running bool   `json:"running"`
	Pid     string `json:"pid,omitempty"`
}

type agentStatus struct {
	ID     string `json:"id"`
	Role   string `json:"role"`
	Model  string `json:"model"`
	Status string `json:"status"`
}

type queueStatus struct {
	Name       string `json:"name"`
	Pending    int    `json:"pending"`
	InProgress int    `json:"in_progress"`
}

// Run checks the formation status and prints it.
func Run(maestroDir string, jsonOutput bool) error {
	status := formationStatus{}

	// Check daemon status via UDS ping
	sockPath, err := uds.SocketPath(maestroDir)
	if err != nil {
		return fmt.Errorf("resolve daemon socket path: %w", err)
	}
	status.Daemon = checkDaemon(sockPath)

	// Get agent status from tmux
	status.Agents = getAgentStatuses()

	// Get queue depth
	status.Queues = getQueueDepths(maestroDir)

	// Cross-reference @status (tmux pane variable) against the queue
	// depths so a stale busy is observable to operators even when the
	// daemon has not yet reset it. See annotateStaleBusyAgents for the
	// reasoning.
	status.Agents = annotateStaleBusyAgents(status.Agents, status.Queues)

	// Get planner signal queue health (R-3 observability).
	status.Signals = getSignalsStatus(maestroDir)

	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(status)
	}

	printStatus(status)
	return nil
}

func checkDaemon(sockPath string) daemonStatus {
	client := uds.NewClient(sockPath)
	resp, err := client.SendCommand("ping", nil)
	if err != nil {
		return daemonStatus{Running: false}
	}
	if resp.Success {
		return daemonStatus{Running: true}
	}
	return daemonStatus{Running: false}
}

func getAgentStatuses() []agentStatus {
	// Include pane_current_command to detect crashed agents (pane returned to shell).
	// Also include @runtime so the model column can fall back to the runtime name
	// for non-claude-code runtimes that launch with no explicit model (e.g.
	// runtime=codex, @model=""). Without this fallback the status output showed
	// an empty model column for those agents.
	// @agent_state lets the daemon tell us "this pane is shell on purpose"
	// (set during respawn-to-project-root before worktree cleanup) so the
	// shell-detection branch below does not flip a daemon-evicted pane to
	// "dead". Without this, every successful command ends with worker rows
	// showing "dead" until the next dispatch — a misleading false alarm.
	lines, err := tmux.ListAllPanes("#{@agent_id}\t#{@role}\t#{@model}\t#{@status}\t#{pane_current_command}\t#{@runtime}\t#{@agent_state}")
	if err != nil {
		return nil
	}
	return parseAgentStatusLines(lines, tmux.IsShellCommand)
}

// parseAgentStatusLines turns the tab-delimited output of
// `tmux list-panes -aF '#{@agent_id}\t...'` into agentStatus rows.
// Extracted so the live-status override logic (shell + agent_state →
// dead vs evicted) can be unit-tested without a tmux session.
func parseAgentStatusLines(lines []string, isShell func(string) bool) []agentStatus {
	agents := make([]agentStatus, 0, len(lines))
	for _, line := range lines {
		parts := strings.SplitN(line, "\t", 7)
		if len(parts) < 5 {
			continue
		}
		if parts[0] == "" {
			continue
		}
		status := parts[3]
		// Override status to "dead" if the pane's foreground command is a shell,
		// which indicates the agent process crashed and returned to the shell prompt.
		// @status (parts[3]) is only updated by the daemon/agent and stays "idle"
		// even after a crash, so we must check the live process state here.
		paneCmd := parts[4]
		agentState := ""
		if len(parts) >= 7 {
			agentState = parts[6]
		}
		if isShell(paneCmd) && parts[0] != "" {
			if agentState == "evicted" {
				// Daemon-driven respawn between cleanup and the next
				// dispatch. The pane is intentionally in shell; the
				// next dispatch's ensureWorkingDir will relaunch the
				// agent and clear @agent_state. Surface as the string
				// "idle (evicted)" so operators see why it differs
				// from a normal idle row without confusing it with a
				// crash.
				status = "idle (evicted)"
			} else {
				status = "dead"
			}
		}
		// Model display: prefer @model, fall back to @runtime for non-claude-code
		// runtimes that launch with no explicit model override. This keeps the
		// `maestro status` table informative for codex/gemini agents instead of
		// rendering a blank model column.
		modelDisplay := parts[2]
		if modelDisplay == "" && len(parts) >= 6 {
			runtime := parts[5]
			if runtime != "" && runtime != model.RuntimeClaudeCode {
				modelDisplay = runtime
			}
		}
		agents = append(agents, agentStatus{
			ID:     parts[0],
			Role:   parts[1],
			Model:  modelDisplay,
			Status: status,
		})
	}
	return agents
}

// annotateStaleBusyAgents flips an agent's Status from "busy" to
// "busy (stale)" when no queue file shows an in-progress entry for that
// agent. The 2026-04-29 e2e regression that prompted this rule had
// worker4 stuck at @status=busy with no queue, no command, no dispatch
// — `maestro status` faithfully reported "busy" because that's what
// the tmux pane variable said, even though the daemon's view of the
// queues showed nothing for the worker. Cross-referencing here gives
// operators an immediate signal ("stale") without needing to grep
// daemon.log or inspect queue files manually.
//
// Mapping rules:
//   - workerN  → look for a queue named workerN with InProgress > 0
//   - planner  → look for queue named planner (commands)
//   - orchestrator → look for queue named orchestrator (notifications)
//
// Anything that doesn't fit (custom agent IDs, agents whose queue
// doesn't exist) is left untouched: we only annotate when the busy
// claim is *contradicted* by a parsed queue file. This prevents the
// annotation from firing for legitimate setups where the queue file
// happens to live under a different name.
func annotateStaleBusyAgents(agents []agentStatus, queues []queueStatus) []agentStatus {
	queueByName := make(map[string]queueStatus, len(queues))
	for _, q := range queues {
		queueByName[q.Name] = q
	}
	for i := range agents {
		if agents[i].Status != "busy" {
			continue
		}
		queueName := queueNameForAgent(agents[i].ID, agents[i].Role)
		q, exists := queueByName[queueName]
		if !exists {
			// No queue file with that name means we cannot disprove the
			// busy claim — leave the row alone.
			continue
		}
		if q.InProgress > 0 {
			continue
		}
		agents[i].Status = "busy (stale)"
	}
	return agents
}

// queueNameForAgent maps an agent ID/role to the queue file name we
// expect to back its busy claim. Workers map by ID (worker1..workerN);
// planner and orchestrator map by role since their canonical agent IDs
// match the queue name already.
func queueNameForAgent(agentID, role string) string {
	if role == "planner" {
		return "planner"
	}
	if role == "orchestrator" {
		return "orchestrator"
	}
	// Default: worker IDs are also queue file names.
	return agentID
}

type queueFile struct {
	Commands      []queueEntry `yaml:"commands,omitempty"`
	Tasks         []queueEntry `yaml:"tasks,omitempty"`
	Notifications []queueEntry `yaml:"notifications,omitempty"`
}

type queueEntry struct {
	Status string `yaml:"status"`
}

func getQueueDepths(maestroDir string) []queueStatus {
	queueDir := filepath.Join(maestroDir, "queue")
	entries, err := os.ReadDir(queueDir)
	if err != nil {
		return nil
	}

	queues := make([]queueStatus, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}

		filePath := filepath.Join(queueDir, entry.Name())

		// Size guard: skip files exceeding the maximum YAML file size to prevent OOM
		info, err := entry.Info()
		if err != nil {
			slog.Warn("status: failed to stat file", "file", entry.Name(), "error", err)
			continue
		}
		if info.Size() > int64(model.DefaultMaxYAMLFileBytes) {
			slog.Warn("status: skipping file too large", "file", entry.Name(), "size_bytes", info.Size())
			continue
		}

		data, err := os.ReadFile(filePath) //nolint:gosec // filePath is constructed from a controlled application state directory
		if err != nil {
			slog.Warn("status: failed to read file", "file", entry.Name(), "error", err)
			continue
		}

		// Validate schema header (accept any queue file type)
		if err := maestroyaml.ValidateSchemaHeaderFromBytes(data, ""); err != nil {
			slog.Warn("status: invalid schema", "file", entry.Name(), "error", err)
			continue
		}

		var qf queueFile
		if err := maestroyaml.SafeUnmarshal(data, &qf); err != nil {
			slog.Warn("status: failed to parse file", "file", entry.Name(), "error", err)
			continue
		}

		// Collect all entries from whichever field is populated
		allEntries := make([]queueEntry, 0, len(qf.Commands)+len(qf.Tasks)+len(qf.Notifications))
		allEntries = append(allEntries, qf.Commands...)
		allEntries = append(allEntries, qf.Tasks...)
		allEntries = append(allEntries, qf.Notifications...)

		var pending, inProgress int
		for _, e := range allEntries {
			switch e.Status {
			case "pending":
				pending++
			case "in_progress":
				inProgress++
			}
		}

		name := strings.TrimSuffix(entry.Name(), ".yaml")
		queues = append(queues, queueStatus{
			Name:       name,
			Pending:    pending,
			InProgress: inProgress,
		})
	}

	return queues
}

// getSignalsStatus reads planner_signals.yaml and summarises its retry
// state. Returns nil when the file is missing or unparseable so the JSON
// output stays compact for a healthy daemon. R-3 observability: matches
// the meta-circuit fired in queue_scan_phase_b.recordCascadeBreakOutcome
// from the operator side without requiring a UDS extension.
func getSignalsStatus(maestroDir string) *signalsStatus {
	path := filepath.Join(maestroDir, "queue", "planner_signals.yaml")
	info, statErr := os.Stat(path)
	if statErr != nil || info.IsDir() {
		return nil
	}
	if info.Size() == 0 || info.Size() > int64(model.DefaultMaxYAMLFileBytes) {
		return nil
	}
	data, err := os.ReadFile(path) //nolint:gosec // controlled application queue path
	if err != nil {
		slog.Warn("status: failed to read planner_signals", "error", err)
		return nil
	}
	if err := maestroyaml.ValidateSchemaHeaderFromBytes(data, ""); err != nil {
		slog.Warn("status: invalid planner_signals schema", "error", err)
		return nil
	}
	var sq model.PlannerSignalQueue
	if err := maestroyaml.SafeUnmarshal(data, &sq); err != nil {
		slog.Warn("status: failed to parse planner_signals", "error", err)
		return nil
	}
	if len(sq.Signals) == 0 {
		return &signalsStatus{Total: 0}
	}
	out := &signalsStatus{Total: len(sq.Signals)}
	for i := range sq.Signals {
		s := &sq.Signals[i]
		if s.Attempts > out.MaxAttempts {
			out.MaxAttempts = s.Attempts
		}
		if s.LastAttemptAt != nil && *s.LastAttemptAt > out.LastAttemptAt {
			out.LastAttemptAt = *s.LastAttemptAt
			if s.LastError != nil {
				out.LastError = *s.LastError
			} else {
				out.LastError = ""
			}
		}
	}
	if out.MaxAttempts >= signalDegradationAttemptsThreshold {
		out.Degraded = true
	}
	return out
}

func printStatus(s formationStatus) {
	// Daemon
	if s.Daemon.Running {
		fmt.Println("Daemon: running")
	} else {
		fmt.Println("Daemon: stopped")
	}

	// Agents
	if len(s.Agents) > 0 {
		fmt.Println("\nAgents:")
		for _, a := range s.Agents {
			fmt.Printf("  %-14s  role=%-12s  model=%-6s  status=%s\n",
				a.ID, a.Role, a.Model, a.Status)
		}
	} else {
		fmt.Println("\nAgents: none")
	}

	// Queues
	if len(s.Queues) > 0 {
		fmt.Println("\nQueues:")
		fmt.Printf("  %-14s  %7s  %11s\n", "NAME", "PENDING", "IN_PROGRESS")
		for _, q := range s.Queues {
			fmt.Printf("  %-14s  %7d  %11d\n", q.Name, q.Pending, q.InProgress)
		}
	}

	// Signals — surfaces the daemon's tmux delivery health from the
	// persisted queue file (R-3). Only printed when there is something to
	// say; healthy daemons stay quiet here.
	if s.Signals != nil && (s.Signals.Total > 0 || s.Signals.MaxAttempts > 0) {
		fmt.Println("\nSignals:")
		fmt.Printf("  total=%d  max_attempts=%d", s.Signals.Total, s.Signals.MaxAttempts)
		if s.Signals.LastAttemptAt != "" {
			fmt.Printf("  last_attempt_at=%s", s.Signals.LastAttemptAt)
		}
		fmt.Println()
		if s.Signals.LastError != "" {
			fmt.Printf("  last_error=%s\n", s.Signals.LastError)
		}
		if s.Signals.Degraded {
			fmt.Printf("  status=degraded  (signal retries >= %d; tmux delivery may need operator attention — see daemon.log for tmux_delivery_sustained_degradation)\n",
				signalDegradationAttemptsThreshold)
		}
	}
}
