package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"flag"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/bridge"
	"github.com/msageha/maestro_v2/internal/daemon"
	"github.com/msageha/maestro_v2/internal/formation"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/plan"
	"github.com/msageha/maestro_v2/internal/setup"
	"github.com/msageha/maestro_v2/internal/status"
	"github.com/msageha/maestro_v2/internal/tmux"
	"github.com/msageha/maestro_v2/internal/uds"
	"github.com/msageha/maestro_v2/internal/validate"
	"github.com/msageha/maestro_v2/internal/worker"
)

const version = "2.0.0"

// stringSliceFlag implements flag.Value for flags that can be specified multiple times.
type stringSliceFlag []string

func (s *stringSliceFlag) String() string {
	if s == nil {
		return ""
	}
	return strings.Join(*s, ",")
}

func (s *stringSliceFlag) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// newFlagSet creates a flag.FlagSet that suppresses default output (errors are handled by callers).
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// modeSetter implements flag.Value as a boolean flag that sets a shared string variable.
// Used for shorthand mode flags (e.g., --interrupt sets mode to "interrupt").
// Since flag.FlagSet processes args left to right, argv-order precedence is preserved.
type modeSetter struct {
	target *string
	val    string
}

func (m *modeSetter) String() string  { return "" }
func (m *modeSetter) Set(string) error { *m.target = m.val; return nil }
func (m *modeSetter) IsBoolFlag() bool { return true }

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "daemon":
		runDaemon(os.Args[2:])
	case "setup":
		runSetup(os.Args[2:])
	case "up":
		runUp(os.Args[2:])
	case "down":
		runDown(os.Args[2:])
	case "status":
		runStatus(os.Args[2:])
	case "queue":
		runQueue(os.Args[2:])
	case "result":
		runResult(os.Args[2:])
	case "task":
		runTask(os.Args[2:])
	case "plan":
		runPlan(os.Args[2:])
	case "agent":
		runAgent(os.Args[2:])
	case "worker":
		runWorker(os.Args[2:])
	case "dashboard":
		runDashboard(os.Args[2:])
	case "version":
		fmt.Printf("maestro %s\n", version)
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func runQueue(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: maestro queue <write> [options]")
		os.Exit(1)
	}
	switch args[0] {
	case "write":
		runQueueWrite(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown queue subcommand: %s\n", args[0])
		fmt.Fprintln(os.Stderr, "usage: maestro queue write <target> [options]")
		os.Exit(1)
	}
}

func runResult(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: maestro result <write> [options]")
		os.Exit(1)
	}
	switch args[0] {
	case "write":
		runResultWrite(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown result subcommand: %s\n", args[0])
		fmt.Fprintln(os.Stderr, "usage: maestro result write <reporter> [options]")
		os.Exit(1)
	}
}

func runTask(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: maestro task <heartbeat> [options]")
		os.Exit(1)
	}
	switch args[0] {
	case "heartbeat":
		runTaskHeartbeat(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown task subcommand: %s\n", args[0])
		fmt.Fprintln(os.Stderr, "usage: maestro task heartbeat [options]")
		os.Exit(1)
	}
}

func runPlan(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: maestro plan <submit|complete|add-retry-task|request-cancel|rebuild> [options]")
		os.Exit(1)
	}
	switch args[0] {
	case "submit":
		runPlanSubmit(args[1:])
	case "complete":
		runPlanComplete(args[1:])
	case "add-retry-task":
		runPlanAddRetryTask(args[1:])
	case "request-cancel":
		runPlanRequestCancel(args[1:])
	case "rebuild":
		runPlanRebuild(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown plan subcommand: %s\n", args[0])
		fmt.Fprintln(os.Stderr, "usage: maestro plan <submit|complete|add-retry-task|request-cancel|rebuild> [options]")
		os.Exit(1)
	}
}

func runAgent(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: maestro agent <launch|exec> [options]")
		os.Exit(1)
	}
	switch args[0] {
	case "launch":
		runAgentLaunch(args[1:])
	case "exec":
		runAgentExec(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown agent subcommand: %s\n", args[0])
		fmt.Fprintln(os.Stderr, "usage: maestro agent <launch|exec> [options]")
		os.Exit(1)
	}
}

func runWorker(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: maestro worker <standby> [options]")
		os.Exit(1)
	}
	switch args[0] {
	case "standby":
		runWorkerStandby(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown worker subcommand: %s\n", args[0])
		fmt.Fprintln(os.Stderr, "usage: maestro worker standby [options]")
		os.Exit(1)
	}
}

// Stub functions — implementations will be added in later phases.

func runDaemon(_ []string) {
	maestroDir := findMaestroDir()
	if maestroDir == "" {
		fmt.Fprintln(os.Stderr, "error: .maestro/ directory not found. Run 'maestro setup <dir>' first.")
		os.Exit(1)
	}

	cfg, err := loadConfig(maestroDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		os.Exit(1)
	}

	// HIGH-16: Validate project name before use in tmux session name
	if err := validate.ValidateProjectName(cfg.Project.Name); err != nil {
		fmt.Fprintf(os.Stderr, "invalid project name: %v\n", err)
		os.Exit(1)
	}
	tmux.SetSessionName("maestro-" + cfg.Project.Name)

	d, err := daemon.New(maestroDir, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create daemon: %v\n", err)
		os.Exit(1)
	}

	// Wire Phase 6 state reader for dependency resolution (shared lockMap)
	sharedLockMap := d.LockMap()
	sm := plan.NewStateManager(maestroDir, sharedLockMap)
	reader := plan.NewPlanStateReader(sm)
	d.SetStateReader(reader)
	d.SetCanComplete(plan.CanComplete)

	// Wire plan executor for UDS plan operations (shared lockMap)
	d.SetPlanExecutor(&bridge.PlanExecutorImpl{
		MaestroDir: maestroDir,
		Config:     cfg,
		LockMap:    sharedLockMap,
	})

	if err := d.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "daemon: %v\n", err)
		os.Exit(1)
	}
}

func runSetup(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: maestro setup <project_dir> [project_name]")
		os.Exit(1)
	}
	projectDir := args[0]
	var projectName string
	if len(args) >= 2 {
		projectName = args[1]
	}
	if err := setup.Run(projectDir, projectName); err != nil {
		fmt.Fprintf(os.Stderr, "setup: %v\n", err)
		os.Exit(1)
	}
	absDir, _ := filepath.Abs(projectDir)
	fmt.Printf("Initialized .maestro/ in %s\n", absDir)
}

func runUp(args []string) {
	fs := newFlagSet("maestro up")
	var boost, continuous, detach, force bool
	fs.BoolVar(&boost, "boost", false, "")
	fs.BoolVar(&continuous, "continuous", false, "")
	fs.BoolVar(&detach, "detach", false, "")
	fs.BoolVar(&detach, "d", false, "")
	fs.BoolVar(&force, "force", false, "")
	fs.BoolVar(&force, "f", false, "")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "%v\nusage: maestro up [--boost] [--continuous] [--detach|-d] [--force|-f]\n", err)
		os.Exit(1)
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "unexpected argument: %s\nusage: maestro up [--boost] [--continuous] [--detach|-d] [--force|-f]\n", fs.Arg(0))
		os.Exit(1)
	}

	maestroDir := findMaestroDir()
	if maestroDir == "" {
		fmt.Fprintln(os.Stderr, "error: .maestro/ directory not found. Run 'maestro setup <dir>' first.")
		os.Exit(1)
	}

	cfg, err := loadConfig(maestroDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		os.Exit(1)
	}

	// Validate project name before use in tmux session name
	if err := validate.ValidateProjectName(cfg.Project.Name); err != nil {
		fmt.Fprintf(os.Stderr, "invalid project name: %v\n", err)
		os.Exit(1)
	}
	tmux.SetSessionName("maestro-" + cfg.Project.Name)

	opts := formation.UpOptions{
		MaestroDir:    maestroDir,
		Config:        cfg,
		Boost:         boost,
		Continuous:    continuous,
		Force:         force,
		BoostSet:      boost,
		ContinuousSet: continuous,
	}

	if err := formation.RunUp(opts); err != nil {
		// Clean up any partially-created resources (tmux session, daemon)
		fmt.Fprintln(os.Stderr, "Cleaning up after setup failure...")
		formation.CleanupOnFailure(maestroDir)
		fmt.Fprintf(os.Stderr, "up: %v\n", err)
		os.Exit(1)
	}

	if !detach {
		if os.Getenv("TMUX") != "" {
			fmt.Printf("Already inside tmux. Attach with: tmux switch-client -t %s\n", tmux.GetSessionName())
		} else {
			if err := tmux.AttachSession(); err != nil {
				fmt.Fprintf(os.Stderr, "attach: %v\n", err)
				os.Exit(1)
			}
		}
	}
}

func runDown(_ []string) {
	maestroDir := findMaestroDir()
	if maestroDir == "" {
		fmt.Fprintln(os.Stderr, "error: .maestro/ directory not found. Run 'maestro setup <dir>' first.")
		os.Exit(1)
	}

	cfg, err := loadConfig(maestroDir)
	if err != nil {
		// Config may be corrupt, but 'down' must still be able to stop the daemon.
		// Proceed with zero config — UDS/PID-based shutdown works without it.
		fmt.Fprintf(os.Stderr, "Warning: could not load config: %v\nProceeding with default config.\n", err)
	}

	if err := formation.RunDown(maestroDir, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "down: %v\n", err)
		os.Exit(1)
	}
}

func runStatus(args []string) {
	fs := newFlagSet("maestro status")
	var jsonOutput bool
	fs.BoolVar(&jsonOutput, "json", false, "")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "%v\nusage: maestro status [--json]\n", err)
		os.Exit(1)
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "unexpected argument: %s\nusage: maestro status [--json]\n", fs.Arg(0))
		os.Exit(1)
	}

	maestroDir := findMaestroDir()
	if maestroDir == "" {
		fmt.Fprintln(os.Stderr, "error: .maestro/ directory not found. Run 'maestro setup <dir>' first.")
		os.Exit(1)
	}

	cfg, err := loadConfig(maestroDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		os.Exit(1)
	}
	// HIGH-16: Validate project name before use in tmux session name
	if err := validate.ValidateProjectName(cfg.Project.Name); err != nil {
		fmt.Fprintf(os.Stderr, "invalid project name: %v\n", err)
		os.Exit(1)
	}
	tmux.SetSessionName("maestro-" + cfg.Project.Name)

	if err := status.Run(maestroDir, jsonOutput); err != nil {
		fmt.Fprintf(os.Stderr, "status: %v\n", err)
		os.Exit(1)
	}
}

func runQueueWrite(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: maestro queue write <target> --type <command|task|notification|cancel-request> [options]")
		os.Exit(1)
	}

	target := args[0]

	fs := newFlagSet("maestro queue write")
	var writeType, content, commandID, purpose, acceptanceCriteria, sourceResultID, notificationType, reason string
	var bloomLevel, priority int
	var blockedBy, constraints, toolsHint stringSliceFlag

	fs.StringVar(&writeType, "type", "", "")
	fs.StringVar(&content, "content", "", "")
	fs.StringVar(&commandID, "command-id", "", "")
	fs.StringVar(&purpose, "purpose", "", "")
	fs.StringVar(&acceptanceCriteria, "acceptance-criteria", "", "")
	fs.IntVar(&bloomLevel, "bloom-level", 0, "")
	fs.IntVar(&priority, "priority", 0, "")
	fs.StringVar(&sourceResultID, "source-result-id", "", "")
	fs.StringVar(&notificationType, "notification-type", "", "")
	fs.Var(&blockedBy, "blocked-by", "")
	fs.Var(&constraints, "constraint", "")
	fs.Var(&toolsHint, "tools-hint", "")
	fs.StringVar(&reason, "reason", "", "")

	if err := fs.Parse(args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "%v\nusage: maestro queue write <target> --type <command|task|notification|cancel-request> [options]\n", err)
		os.Exit(1)
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "unexpected argument: %s\nusage: maestro queue write <target> --type <command|task|notification|cancel-request> [options]\n", fs.Arg(0))
		os.Exit(1)
	}

	if writeType == "" {
		fmt.Fprintln(os.Stderr, "--type is required")
		fmt.Fprintln(os.Stderr, "usage: maestro queue write <target> --type <command|task|notification|cancel-request> [options]")
		os.Exit(1)
	}

	params := map[string]any{
		"target": target,
		"type":   writeType,
	}

	switch writeType {
	case "command":
		if content == "" {
			fmt.Fprintln(os.Stderr, "--content is required for type=command")
			os.Exit(1)
		}
		params["content"] = content
		if priority > 0 {
			params["priority"] = priority
		}
	case "task":
		if commandID == "" || content == "" || purpose == "" || acceptanceCriteria == "" || bloomLevel == 0 {
			fmt.Fprintln(os.Stderr, "required for type=task: --command-id, --content, --purpose, --acceptance-criteria, --bloom-level")
			os.Exit(1)
		}
		params["command_id"] = commandID
		params["content"] = content
		params["purpose"] = purpose
		params["acceptance_criteria"] = acceptanceCriteria
		params["bloom_level"] = bloomLevel
		if priority > 0 {
			params["priority"] = priority
		}
		if len(blockedBy) > 0 {
			params["blocked_by"] = blockedBy
		}
		if len(constraints) > 0 {
			params["constraints"] = constraints
		}
		if len(toolsHint) > 0 {
			params["tools_hint"] = toolsHint
		}
	case "notification":
		if commandID == "" || content == "" || sourceResultID == "" {
			fmt.Fprintln(os.Stderr, "required for type=notification: --command-id, --content, --source-result-id")
			os.Exit(1)
		}
		params["command_id"] = commandID
		params["content"] = content
		params["source_result_id"] = sourceResultID
		if notificationType != "" {
			params["notification_type"] = notificationType
		}
		if priority > 0 {
			params["priority"] = priority
		}
	case "cancel-request":
		if commandID == "" {
			fmt.Fprintln(os.Stderr, "required for type=cancel-request: --command-id")
			os.Exit(1)
		}
		params["command_id"] = commandID
		if reason != "" {
			params["reason"] = reason
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown type: %s\n", writeType)
		fmt.Fprintln(os.Stderr, "usage: maestro queue write <target> --type <command|task|notification|cancel-request> [options]")
		os.Exit(1)
	}

	sendQueueWrite(params)
}

func sendQueueWrite(params map[string]any) {
	maestroDir := findMaestroDir()
	if maestroDir == "" {
		fmt.Fprintln(os.Stderr, "error: .maestro/ directory not found. Run 'maestro setup <dir>' first.")
		os.Exit(1)
	}

	client := uds.NewClient(filepath.Join(maestroDir, uds.DefaultSocketName))
	resp, err := client.SendCommand("queue_write", params)
	if err != nil {
		fmt.Fprintf(os.Stderr, "queue write: %v\n", err)
		os.Exit(1)
	}

	if !resp.Success {
		code := ""
		msg := "unknown error"
		if resp.Error != nil {
			code = resp.Error.Code
			msg = resp.Error.Message
		}
		fmt.Fprintf(os.Stderr, "queue write failed [%s]: %s\n", code, msg)
		if code == "BACKPRESSURE" {
			os.Exit(2)
		}
		os.Exit(1)
	}

	var result map[string]string
	if err := json.Unmarshal(resp.Data, &result); err == nil {
		if id, ok := result["id"]; ok {
			fmt.Println(id)
			return
		}
		if cid, ok := result["command_id"]; ok {
			fmt.Println(cid)
			return
		}
	}
	out, _ := json.MarshalIndent(resp.Data, "", "  ")
	fmt.Println(string(out))
}

func runResultWrite(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: maestro result write <reporter> [options]")
		os.Exit(1)
	}

	reporter := args[0]

	fs := newFlagSet("maestro result write")
	var taskID, commandID, resultStatus, summary string
	var leaseEpoch int
	var filesChanged stringSliceFlag
	var partialChangesPossible, noRetrySafe bool

	fs.StringVar(&taskID, "task-id", "", "")
	fs.StringVar(&commandID, "command-id", "", "")
	fs.IntVar(&leaseEpoch, "lease-epoch", 0, "")
	fs.StringVar(&resultStatus, "status", "", "")
	fs.StringVar(&summary, "summary", "", "")
	fs.Var(&filesChanged, "files-changed", "")
	fs.BoolVar(&partialChangesPossible, "partial-changes", false, "")
	fs.BoolVar(&noRetrySafe, "no-retry-safe", false, "")

	if err := fs.Parse(args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "%v\nusage: maestro result write <reporter> --task-id <id> --command-id <id> --lease-epoch <n> --status <status> [--summary <text>] [--files-changed <file>]... [--partial-changes] [--no-retry-safe]\n", err)
		os.Exit(1)
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "unexpected argument: %s\nusage: maestro result write <reporter> --task-id <id> --command-id <id> --lease-epoch <n> --status <status> [--summary <text>] [--files-changed <file>]... [--partial-changes] [--no-retry-safe]\n", fs.Arg(0))
		os.Exit(1)
	}

	if taskID == "" || commandID == "" || resultStatus == "" {
		fmt.Fprintln(os.Stderr, "usage: maestro result write <reporter> --task-id <id> --command-id <id> --lease-epoch <n> --status <status> [--summary <text>]")
		os.Exit(1)
	}

	maestroDir := findMaestroDir()
	if maestroDir == "" {
		fmt.Fprintln(os.Stderr, "error: .maestro/ directory not found. Run 'maestro setup <dir>' first.")
		os.Exit(1)
	}

	params := map[string]any{
		"reporter":    reporter,
		"task_id":     taskID,
		"command_id":  commandID,
		"lease_epoch": leaseEpoch,
		"status":      resultStatus,
		"summary":     summary,
		"retry_safe":  !noRetrySafe,
	}
	if len(filesChanged) > 0 {
		params["files_changed"] = filesChanged
	}
	if partialChangesPossible {
		params["partial_changes_possible"] = true
	}

	client := uds.NewClient(filepath.Join(maestroDir, uds.DefaultSocketName))
	resp, err := client.SendCommand("result_write", params)
	if err != nil {
		fmt.Fprintf(os.Stderr, "result write: %v\n", err)
		os.Exit(1)
	}

	if !resp.Success {
		code := ""
		msg := "unknown error"
		if resp.Error != nil {
			code = resp.Error.Code
			msg = resp.Error.Message
		}
		fmt.Fprintf(os.Stderr, "result write failed [%s]: %s\n", code, msg)
		if code == "FENCING_REJECT" {
			os.Exit(2)
		}
		os.Exit(1)
	}

	out, _ := json.MarshalIndent(resp.Data, "", "  ")
	fmt.Println(string(out))
}

func runPlanSubmit(args []string) {
	fs := newFlagSet("maestro plan submit")
	var commandID, tasksFile, phaseName string
	var dryRun bool
	fs.StringVar(&commandID, "command-id", "", "")
	fs.StringVar(&tasksFile, "tasks-file", "", "")
	fs.StringVar(&phaseName, "phase", "", "")
	fs.BoolVar(&dryRun, "dry-run", false, "")

	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "%v\nusage: maestro plan submit --command-id <id> [--tasks-file <path>] [--phase <name>] [--dry-run]\n", err)
		os.Exit(1)
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "unexpected argument: %s\nusage: maestro plan submit --command-id <id> [--tasks-file <path>] [--phase <name>] [--dry-run]\n", fs.Arg(0))
		os.Exit(1)
	}

	if commandID == "" {
		fmt.Fprintln(os.Stderr, "usage: maestro plan submit --command-id <id> [--tasks-file <path>] [--phase <name>] [--dry-run]")
		os.Exit(1)
	}

	if tasksFile == "" {
		tasksFile = "-" // default to stdin
	}

	// CRIT-05: Validate non-stdin file path before passing to daemon
	if tasksFile != "-" && tasksFile != "/dev/stdin" {
		cleaned, err := validate.ValidateFilePath(tasksFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid tasks file: %v\n", err)
			os.Exit(1)
		}
		tasksFile = cleaned
	}

	maestroDir := findMaestroDir()
	if maestroDir == "" {
		fmt.Fprintln(os.Stderr, "error: .maestro/ directory not found. Run 'maestro setup <dir>' first.")
		os.Exit(1)
	}

	// If reading from stdin, materialize to a temp file so the daemon can read it
	// (daemon's stdin is not the CLI's stdin when using UDS)
	actualFile := tasksFile
	if tasksFile == "-" || tasksFile == "/dev/stdin" {
		data, err := io.ReadAll(io.LimitReader(os.Stdin, int64(model.DefaultMaxYAMLFileBytes)+1))
		if err != nil {
			fmt.Fprintf(os.Stderr, "read stdin: %v\n", err)
			os.Exit(1)
		}
		if len(data) > model.DefaultMaxYAMLFileBytes {
			fmt.Fprintf(os.Stderr, "stdin input exceeds maximum size of %d bytes\n", model.DefaultMaxYAMLFileBytes)
			os.Exit(1)
		}
		tmpFile, err := os.CreateTemp("", "maestro-plan-submit-*.yaml")
		if err != nil {
			fmt.Fprintf(os.Stderr, "create temp file: %v\n", err)
			os.Exit(1)
		}
		defer func() { _ = os.Remove(tmpFile.Name()) }()
		if _, err := tmpFile.Write(data); err != nil {
			_ = tmpFile.Close()
			fmt.Fprintf(os.Stderr, "write temp file: %v\n", err)
			os.Exit(1)
		}
		_ = tmpFile.Close()
		actualFile = tmpFile.Name()
	}

	params := map[string]any{
		"operation": "submit",
		"data": map[string]any{
			"command_id": commandID,
			"tasks_file": actualFile,
			"phase_name": phaseName,
			"dry_run":    dryRun,
		},
	}

	sendPlanCommand(maestroDir, params)
}

func runPlanComplete(args []string) {
	fs := newFlagSet("maestro plan complete")
	var commandID, summary string
	fs.StringVar(&commandID, "command-id", "", "")
	fs.StringVar(&summary, "summary", "", "")

	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "%v\nusage: maestro plan complete --command-id <id> --summary <text>\n", err)
		os.Exit(1)
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "unexpected argument: %s\nusage: maestro plan complete --command-id <id> --summary <text>\n", fs.Arg(0))
		os.Exit(1)
	}

	if commandID == "" {
		fmt.Fprintln(os.Stderr, "usage: maestro plan complete --command-id <id> --summary <text>")
		os.Exit(1)
	}

	maestroDir := findMaestroDir()
	if maestroDir == "" {
		fmt.Fprintln(os.Stderr, "error: .maestro/ directory not found. Run 'maestro setup <dir>' first.")
		os.Exit(1)
	}

	params := map[string]any{
		"operation": "complete",
		"data": map[string]any{
			"command_id": commandID,
			"summary":    summary,
		},
	}

	sendPlanCommand(maestroDir, params)
}

func runPlanAddRetryTask(args []string) {
	fs := newFlagSet("maestro plan add-retry-task")
	var commandID, retryOf, purpose, content, acceptanceCriteria string
	var bloomLevel int
	var blockedBy stringSliceFlag

	fs.StringVar(&commandID, "command-id", "", "")
	fs.StringVar(&retryOf, "retry-of", "", "")
	fs.StringVar(&purpose, "purpose", "", "")
	fs.StringVar(&content, "content", "", "")
	fs.StringVar(&acceptanceCriteria, "acceptance-criteria", "", "")
	fs.IntVar(&bloomLevel, "bloom-level", 0, "")
	fs.Var(&blockedBy, "blocked-by", "")

	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "%v\nusage: maestro plan add-retry-task --command-id <id> --retry-of <task_id> --purpose <text> --content <text> --acceptance-criteria <text> --bloom-level <n> [--blocked-by <task_id>]...\n", err)
		os.Exit(1)
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "unexpected argument: %s\nusage: maestro plan add-retry-task --command-id <id> --retry-of <task_id> --purpose <text> --content <text> --acceptance-criteria <text> --bloom-level <n> [--blocked-by <task_id>]...\n", fs.Arg(0))
		os.Exit(1)
	}

	if commandID == "" || retryOf == "" || purpose == "" || content == "" || acceptanceCriteria == "" || bloomLevel == 0 {
		fmt.Fprintln(os.Stderr, "usage: maestro plan add-retry-task --command-id <id> --retry-of <task_id> --purpose <text> --content <text> --acceptance-criteria <text> --bloom-level <n> [--blocked-by <task_id>]...")
		os.Exit(1)
	}

	maestroDir := findMaestroDir()
	if maestroDir == "" {
		fmt.Fprintln(os.Stderr, "error: .maestro/ directory not found. Run 'maestro setup <dir>' first.")
		os.Exit(1)
	}

	params := map[string]any{
		"operation": "add_retry_task",
		"data": map[string]any{
			"command_id":          commandID,
			"retry_of":            retryOf,
			"purpose":             purpose,
			"content":             content,
			"acceptance_criteria": acceptanceCriteria,
			"blocked_by":          blockedBy,
			"bloom_level":         bloomLevel,
		},
	}

	sendPlanCommand(maestroDir, params)
}

func runPlanRequestCancel(args []string) {
	fs := newFlagSet("maestro plan request-cancel")
	var commandID, requestedBy, reason string
	fs.StringVar(&commandID, "command-id", "", "")
	fs.StringVar(&requestedBy, "requested-by", "", "")
	fs.StringVar(&reason, "reason", "", "")

	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "%v\nusage: maestro plan request-cancel --command-id <id> [--requested-by <agent>] [--reason <text>]\n", err)
		os.Exit(1)
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "unexpected argument: %s\nusage: maestro plan request-cancel --command-id <id> [--requested-by <agent>] [--reason <text>]\n", fs.Arg(0))
		os.Exit(1)
	}

	if commandID == "" {
		fmt.Fprintln(os.Stderr, "usage: maestro plan request-cancel --command-id <id> [--requested-by <agent>] [--reason <text>]")
		os.Exit(1)
	}

	if requestedBy == "" {
		requestedBy = "cli"
	}

	maestroDir := findMaestroDir()
	if maestroDir == "" {
		fmt.Fprintln(os.Stderr, "error: .maestro/ directory not found. Run 'maestro setup <dir>' first.")
		os.Exit(1)
	}

	// Route through daemon UDS to respect single-writer architecture
	params := map[string]any{
		"target":       "planner",
		"type":         "cancel-request",
		"command_id":   commandID,
		"requested_by": requestedBy,
		"reason":       reason,
	}

	client := uds.NewClient(filepath.Join(maestroDir, uds.DefaultSocketName))
	resp, err := client.SendCommand("queue_write", params)
	if err != nil {
		fmt.Fprintf(os.Stderr, "request-cancel: %v\n", err)
		os.Exit(1)
	}

	if !resp.Success {
		code := ""
		msg := "unknown error"
		if resp.Error != nil {
			code = resp.Error.Code
			msg = resp.Error.Message
		}
		fmt.Fprintf(os.Stderr, "request-cancel failed [%s]: %s\n", code, msg)
		os.Exit(1)
	}

	fmt.Printf("cancel requested for command %s\n", commandID)
}

func runPlanRebuild(args []string) {
	fs := newFlagSet("maestro plan rebuild")
	var commandID string
	fs.StringVar(&commandID, "command-id", "", "")

	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "%v\nusage: maestro plan rebuild --command-id <id>\n", err)
		os.Exit(1)
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "unexpected argument: %s\nusage: maestro plan rebuild --command-id <id>\n", fs.Arg(0))
		os.Exit(1)
	}

	if commandID == "" {
		fmt.Fprintln(os.Stderr, "usage: maestro plan rebuild --command-id <id>")
		os.Exit(1)
	}

	maestroDir := findMaestroDir()
	if maestroDir == "" {
		fmt.Fprintln(os.Stderr, "error: .maestro/ directory not found. Run 'maestro setup <dir>' first.")
		os.Exit(1)
	}

	params := map[string]any{
		"operation": "rebuild",
		"data": map[string]any{
			"command_id": commandID,
		},
	}

	sendPlanCommand(maestroDir, params)
}

func sendPlanCommand(maestroDir string, params map[string]any) {
	client := uds.NewClient(filepath.Join(maestroDir, uds.DefaultSocketName))
	resp, err := client.SendCommand("plan", params)
	if err != nil {
		fmt.Fprintf(os.Stderr, "plan: %v\n", err)
		os.Exit(1)
	}

	if !resp.Success {
		code := ""
		msg := "unknown error"
		if resp.Error != nil {
			code = resp.Error.Code
			msg = resp.Error.Message
		}
		if code == uds.ErrCodeValidation || code == uds.ErrCodeActionRequired {
			fmt.Fprint(os.Stderr, msg)
		} else {
			fmt.Fprintf(os.Stderr, "plan failed [%s]: %s\n", code, msg)
		}
		os.Exit(1)
	}

	out, _ := json.MarshalIndent(resp.Data, "", "  ")
	fmt.Println(string(out))
}

func runAgentLaunch(_ []string) {
	maestroDir := findMaestroDir()
	if maestroDir == "" {
		fmt.Fprintln(os.Stderr, "error: .maestro/ directory not found. Run 'maestro setup <dir>' first.")
		os.Exit(1)
	}
	if err := agent.Launch(maestroDir); err != nil {
		fmt.Fprintf(os.Stderr, "agent launch: %v\n", err)
		os.Exit(1)
	}
}

func runAgentExec(args []string) {
	fs := newFlagSet("maestro agent exec")
	var agentID, message string
	mode := "deliver"
	fs.StringVar(&agentID, "agent-id", "", "")
	fs.StringVar(&message, "message", "", "")
	fs.StringVar(&mode, "mode", "deliver", "")
	fs.Var(&modeSetter{target: &mode, val: "with_clear"}, "with-clear", "")
	fs.Var(&modeSetter{target: &mode, val: "interrupt"}, "interrupt", "")
	fs.Var(&modeSetter{target: &mode, val: "is_busy"}, "is-busy", "")
	fs.Var(&modeSetter{target: &mode, val: "clear"}, "clear", "")

	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "%v\nusage: maestro agent exec --agent-id <id> [--mode <mode>] [--message <msg>]\n", err)
		os.Exit(1)
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "unexpected argument: %s\nusage: maestro agent exec --agent-id <id> [--mode <mode>] [--message <msg>]\n", fs.Arg(0))
		os.Exit(1)
	}

	if agentID == "" {
		fmt.Fprintln(os.Stderr, "usage: maestro agent exec --agent-id <id> [--mode <mode>] [--message <msg>]")
		os.Exit(1)
	}

	maestroDir := findMaestroDir()
	if maestroDir == "" {
		fmt.Fprintln(os.Stderr, "error: .maestro/ directory not found. Run 'maestro setup <dir>' first.")
		os.Exit(1)
	}

	cfg, err := loadConfig(maestroDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		os.Exit(1)
	}
	// HIGH-16: Validate project name before use in tmux session name
	if err := validate.ValidateProjectName(cfg.Project.Name); err != nil {
		fmt.Fprintf(os.Stderr, "invalid project name: %v\n", err)
		os.Exit(1)
	}
	tmux.SetSessionName("maestro-" + cfg.Project.Name)

	exec, err := agent.NewExecutor(maestroDir, cfg.Watcher, cfg.Logging.Level)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create executor: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = exec.Close() }()

	result := exec.Execute(agent.ExecRequest{
		AgentID: agentID,
		Message: message,
		Mode:    agent.ExecMode(mode),
	})

	if result.Error != nil {
		fmt.Fprintf(os.Stderr, "agent exec: %v\n", result.Error)
		if result.Retryable {
			os.Exit(2)
		}
		os.Exit(1)
	}

	if mode == "is_busy" {
		if result.Success {
			fmt.Println("busy")
			os.Exit(0)
		}
		fmt.Println("idle")
		os.Exit(1)
	}
}

func runWorkerStandby(args []string) {
	fs := newFlagSet("maestro worker standby")
	var modelFilter string
	fs.StringVar(&modelFilter, "model", "", "")

	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "%v\nusage: maestro worker standby [--model <model>]\n", err)
		os.Exit(1)
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "unexpected argument: %s\nusage: maestro worker standby [--model <model>]\n", fs.Arg(0))
		os.Exit(1)
	}

	maestroDir := findMaestroDir()
	if maestroDir == "" {
		fmt.Fprintln(os.Stderr, "error: .maestro/ directory not found. Run 'maestro setup <dir>' first.")
		os.Exit(1)
	}

	cfg, err := loadConfig(maestroDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		os.Exit(1)
	}

	output, err := worker.StandbyJSON(worker.StandbyOptions{
		MaestroDir:  maestroDir,
		Config:      cfg,
		ModelFilter: modelFilter,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "worker standby: %v\n", err)
		os.Exit(1)
	}

	fmt.Println(output)
}

func runDashboard(args []string) {
	fs := newFlagSet("maestro dashboard")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "%v\nusage: maestro dashboard\n", err)
		os.Exit(1)
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "unexpected argument: %s\nusage: maestro dashboard\n", fs.Arg(0))
		os.Exit(1)
	}

	maestroDir := findMaestroDir()
	if maestroDir == "" {
		fmt.Fprintln(os.Stderr, "error: .maestro/ directory not found. Run 'maestro setup <dir>' first.")
		os.Exit(1)
	}

	client := uds.NewClient(filepath.Join(maestroDir, uds.DefaultSocketName))
	resp, err := client.SendCommand("dashboard", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dashboard: %v\n", err)
		os.Exit(1)
	}

	if !resp.Success {
		msg := "unknown error"
		if resp.Error != nil {
			msg = resp.Error.Message
		}
		fmt.Fprintf(os.Stderr, "dashboard failed: %s\n", msg)
		os.Exit(1)
	}

	var result map[string]string
	if err := json.Unmarshal(resp.Data, &result); err != nil {
		fmt.Fprintf(os.Stderr, "parse response: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Dashboard regenerated: %s\n", result["path"])
}

// findMaestroDir searches for .maestro/ in the current directory and ancestors.
func findMaestroDir() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		candidate := filepath.Join(dir, ".maestro")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func loadConfig(maestroDir string) (model.Config, error) {
	data, err := os.ReadFile(filepath.Join(maestroDir, "config.yaml"))
	if err != nil {
		return model.Config{}, fmt.Errorf("read config.yaml: %w", err)
	}
	var cfg model.Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return model.Config{}, fmt.Errorf("parse config.yaml: %w", err)
	}
	return cfg, nil
}

func runTaskHeartbeat(args []string) {
	fs := newFlagSet("maestro task heartbeat")
	var taskID, workerID string
	var epoch int
	fs.StringVar(&taskID, "task-id", "", "")
	fs.StringVar(&workerID, "worker-id", "", "")
	fs.IntVar(&epoch, "epoch", 0, "")

	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "%v\nusage: maestro task heartbeat --task-id <id> --worker-id <id> --epoch <n>\n", err)
		os.Exit(1)
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "unexpected argument: %s\nusage: maestro task heartbeat --task-id <id> --worker-id <id> --epoch <n>\n", fs.Arg(0))
		os.Exit(1)
	}

	if taskID == "" {
		fmt.Fprintln(os.Stderr, "--task-id is required")
		os.Exit(1)
	}
	if workerID == "" {
		fmt.Fprintln(os.Stderr, "--worker-id is required")
		os.Exit(1)
	}

	maestroDir := findMaestroDir()
	if maestroDir == "" {
		fmt.Fprintln(os.Stderr, "error: .maestro/ directory not found")
		os.Exit(1)
	}

	params := map[string]interface{}{
		"task_id":   taskID,
		"worker_id": workerID,
		"epoch":     epoch,
	}

	client := uds.NewClient(filepath.Join(maestroDir, uds.DefaultSocketName))
	resp, err := client.SendCommand("task_heartbeat", params)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if !resp.Success {
		if resp.Error != nil {
			// Special handling for max runtime exceeded - exit silently with error code
			if resp.Error.Code == uds.ErrCodeMaxRuntimeExceeded {
				os.Exit(2)
			}
			fmt.Fprintf(os.Stderr, "error [%s]: %s\n", resp.Error.Code, resp.Error.Message)
		} else {
			fmt.Fprintln(os.Stderr, "error: heartbeat failed")
		}
		os.Exit(1)
	}

	// Success - no output (for use in scripts)
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `maestro %s — Multi-agent orchestration system

Usage: maestro <command> [options]

Formation:
  setup <dir> [name]  Initialize .maestro/ directory
  up [flags]        Start formation and attach (--detach|-d to skip)
  down              Graceful shutdown
  status [--json]   Show formation status

Agent Commands (CLI → Daemon):
  queue write <target> [options]   Write to queue
  result write <reporter> [options] Write result
  plan submit [options]            Submit task plan
  plan complete [options]          Report command completion
  plan add-retry-task [options]    Replace failed task
  plan request-cancel [options]    Request cancellation
  plan rebuild [options]           Rebuild state from results

Internal:
  daemon            Run daemon process
  agent launch      Launch agent in tmux pane
  agent exec        Send message to agent

Utilities:
  worker standby    Show idle workers
  dashboard         Regenerate dashboard.md
  version           Show version
  help              Show this help

`, version)
}
