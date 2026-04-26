package daemon

import (
	"encoding/json"
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/uds"
)

func makeVerifyWriteRequest(t *testing.T, commandID, configData string) *uds.Request {
	t.Helper()
	params, err := json.Marshal(map[string]string{"command_id": commandID, "config_data": configData})
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	return &uds.Request{
		ProtocolVersion: 1,
		Command:         "verify_write",
		CallerRole:      uds.RolePlanner,
		Params:          params,
	}
}

func TestVerifyWrite_WritesConfigThroughDaemon(t *testing.T) {
	t.Parallel()
	d := newTestDaemon(t)

	commandID := "cmd_0000000001_verify1"
	req := makeVerifyWriteRequest(t, commandID, "verify:\n  test:\n    - go test ./...\n")
	resp := d.api.handleVerifyWrite(req)
	if !resp.Success {
		t.Fatalf("handleVerifyWrite: %v", resp.Error)
	}

	cfg, err := model.LoadVerifyConfig(verifySnapshotPath(d.maestroDir, commandID))
	if err != nil {
		t.Fatalf("load verify config: %v", err)
	}
	if got := cfg.Test; len(got) != 1 || got[0] != "go test ./..." {
		t.Fatalf("commands = %#v, want go test ./...", got)
	}
}

func TestVerifyWrite_RejectsEmptyConfig(t *testing.T) {
	t.Parallel()
	d := newTestDaemon(t)

	resp := d.api.handleVerifyWrite(makeVerifyWriteRequest(t, "cmd_0000000002_verify2", "verify: {}\n"))
	if resp.Success {
		t.Fatal("handleVerifyWrite succeeded, want validation error")
	}
	if resp.Error.Code != uds.ErrCodeValidation {
		t.Fatalf("error code = %q, want %q", resp.Error.Code, uds.ErrCodeValidation)
	}
}

func TestVerifyWrite_RejectsWorkerRole(t *testing.T) {
	t.Parallel()
	d := newTestDaemon(t)

	req := makeVerifyWriteRequest(t, "cmd_0000000003_verify3", "verify:\n  test:\n    - go test ./...\n")
	req.CallerRole = uds.RoleWorker
	resp := d.api.handleVerifyWrite(req)
	if resp.Success {
		t.Fatal("handleVerifyWrite succeeded for worker role")
	}
	if resp.Error.Code != uds.ErrCodeValidation {
		t.Fatalf("error code = %q, want %q", resp.Error.Code, uds.ErrCodeValidation)
	}
}
