package daemonapi

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/uds"
	"github.com/msageha/maestro_v2/internal/validate"
	yamlv3 "gopkg.in/yaml.v3"
)

type VerifyWrite struct {
	maestroDir string
	lock       func()
	unlock     func()
	logInfof   Logf
}

func NewVerifyWrite(maestroDir string, lock func(), unlock func(), logInfof Logf) *VerifyWrite {
	return &VerifyWrite{
		maestroDir: maestroDir,
		lock:       lock,
		unlock:     unlock,
		logInfof:   logInfof,
	}
}

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

	var wrapper struct {
		Verify model.VerifyConfig `yaml:"verify"`
	}
	if err := yamlv3.Unmarshal([]byte(params.ConfigData), &wrapper); err != nil {
		return uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("parse verify config: %v", err))
	}
	if wrapper.Verify.IsEmpty() {
		return uds.ErrorResponse(uds.ErrCodeValidation, "verify config must contain at least one command")
	}
	if err := wrapper.Verify.Validate(); err != nil {
		return uds.ErrorResponse(uds.ErrCodeValidation, err.Error())
	}

	if h.lock != nil {
		h.lock()
		defer h.unlock()
	}

	path := filepath.Join(h.maestroDir, "state", "verify", params.CommandID+".yaml")
	if err := model.SaveVerifyConfig(path, &wrapper.Verify); err != nil {
		return uds.ErrorResponse(uds.ErrCodeInternal, err.Error())
	}
	if h.logInfof != nil {
		h.logInfof("verify_write command=%s commands=%d", params.CommandID, len(wrapper.Verify.AllCommands()))
	}
	return uds.SuccessResponse(map[string]string{"status": "written", "command_id": params.CommandID})
}
