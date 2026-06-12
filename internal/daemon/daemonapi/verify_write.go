package daemonapi

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/uds"
	"github.com/msageha/maestro_v2/internal/validate"
)

// VerifyWrite handles per-command verify-config snapshot writes (the
// `maestro verify write` UDS request the planner uses to pin the verify
// commands a specific plan must satisfy).
type VerifyWrite struct {
	maestroDir     string
	lock           func()
	unlock         func()
	logInfof       Logf
	logWarnf       Logf
	verifyEnabledF func() bool
}

// NewVerifyWrite constructs a VerifyWrite handler. lock/unlock guard the
// snapshot file write so concurrent writes from CLI / planner serialize.
// verifyEnabledF reports whether the daemon's verify runner is currently
// enabled — used to warn the operator when a Planner writes a verify
// snapshot the daemon will silently never execute (Report 2026-05-05 P1).
// nil verifyEnabledF disables the warning (legacy callers without config
// access).
func NewVerifyWrite(maestroDir string, lock func(), unlock func(), logInfof, logWarnf Logf, verifyEnabledF func() bool) *VerifyWrite {
	return &VerifyWrite{
		maestroDir:     maestroDir,
		lock:           lock,
		unlock:         unlock,
		logInfof:       logInfof,
		logWarnf:       logWarnf,
		verifyEnabledF: verifyEnabledF,
	}
}

// Handle validates and persists the requested verify config snapshot
// under <maestroDir>/state/verify/<command_id>.yaml.
func (h *VerifyWrite) Handle(req *uds.Request) *uds.Response {
	var params struct {
		ConfigData string `json:"config_data"`
		CommandID  string `json:"command_id"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("invalid params: %v", err))
	}
	switch req.CallerRole {
	case uds.RolePlanner, uds.RoleCLI:
	case "":
		return uds.ErrorResponse(uds.ErrCodeValidation, "verify_write requires caller_role")
	default:
		return uds.ErrorResponse(uds.ErrCodeValidation,
			fmt.Sprintf("verify_write is not permitted for caller role %q", req.CallerRole))
	}
	if err := validate.ID(params.CommandID); err != nil {
		return uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("invalid command_id: %v", err))
	}
	if params.ConfigData == "" {
		return uds.ErrorResponse(uds.ErrCodeValidation, "config_data is required")
	}
	if len(params.ConfigData) > model.DefaultMaxYAMLFileBytes {
		return uds.ErrorResponse(uds.ErrCodeValidation,
			fmt.Sprintf("config_data exceeds maximum size of %d bytes", model.DefaultMaxYAMLFileBytes))
	}

	cfg, err := model.ParseVerifyConfigYAML([]byte(params.ConfigData))
	if err != nil {
		return uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("parse verify config: %v", err))
	}
	if cfg.IsEmpty() {
		return uds.ErrorResponse(uds.ErrCodeValidation, "verify config must contain at least one command")
	}

	if h.lock != nil {
		h.lock()
		defer h.unlock()
	}

	path := filepath.Join(h.maestroDir, "state", "verify", params.CommandID+".yaml")
	if err := model.SaveVerifyConfig(path, cfg); err != nil {
		return uds.ErrorResponse(uds.ErrCodeInternal, err.Error())
	}
	if h.logInfof != nil {
		h.logInfof("verify_write command=%s commands=%d", params.CommandID, len(cfg.AllCommands()))
	}
	// When verify.enabled=false the snapshot has just been written but
	// the runner is wired with NewSkipVerifyRunner, so the daemon will
	// silently never execute the commands. Warn so the operator sees
	// the misconfiguration before publish (Report 2026-05-05 P1: Planner
	// wrote `go vet ./...` etc. but daemon log only showed
	// `verify_runner_disabled`).
	if h.verifyEnabledF != nil && h.logWarnf != nil && !h.verifyEnabledF() {
		h.logWarnf("verify_write_with_runner_disabled command=%s commands=%d "+
			"(verify.enabled=false in config.yaml — snapshot stored but the daemon verify runner is no-op; "+
			"the snapshot IS still consulted by A/B candidate selection (ab_test.enabled) and by "+
			"RunOnIntegration / RunOnMain tasks once verify.enabled is true)",
			params.CommandID, len(cfg.AllCommands()))
	}
	return uds.SuccessResponse(map[string]string{"status": "written", "command_id": params.CommandID})
}
