package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/uds"
)

func TestRunVerifyWrite_WritesValidatedConfig(t *testing.T) {
	dir := t.TempDir()
	maestroDir := filepath.Join(dir, ".maestro")
	if err := os.MkdirAll(maestroDir, 0o755); err != nil {
		t.Fatal(err)
	}
	inputPath := filepath.Join(dir, "verify-input.yaml")
	if err := os.WriteFile(inputPath, []byte(`verify:
  build:
    - go test ./...
`), 0o644); err != nil {
		t.Fatal(err)
	}
	oldwd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldwd) })

	var gotCommand string
	var gotParams map[string]any
	app := newTestApp(&mockUDSClient{
		sendCommandFunc: func(command string, params any) (*uds.Response, error) {
			gotCommand = command
			raw, _ := json.Marshal(params)
			if err := json.Unmarshal(raw, &gotParams); err != nil {
				t.Fatalf("unmarshal params: %v", err)
			}
			return successResponse(map[string]string{"status": "written"}), nil
		},
	})
	err := app.runVerify([]string{"write", "--command-id", "cmd_0000000001_verify1", "--config-file", inputPath})
	if err != nil {
		t.Fatalf("runVerify write: %v", err)
	}
	if gotCommand != "verify_write" {
		t.Fatalf("command = %q, want verify_write", gotCommand)
	}
	if !strings.Contains(gotParams["config_data"].(string), "go test ./...") {
		t.Fatalf("config_data missing verify command: %v", gotParams["config_data"])
	}
	if gotParams["command_id"] != "cmd_0000000001_verify1" {
		t.Fatalf("command_id = %v, want cmd_0000000001_verify1", gotParams["command_id"])
	}
}

func TestRunVerifyWrite_RejectsEmptyConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".maestro"), 0o755); err != nil {
		t.Fatal(err)
	}
	inputPath := filepath.Join(dir, "verify-empty.yaml")
	if err := os.WriteFile(inputPath, []byte("verify: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldwd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldwd) })

	err := newCLIApp().runVerify([]string{"write", "--command-id", "cmd_0000000002_verify2", "--config-file", inputPath})
	if err == nil {
		t.Fatal("expected error for empty verify config")
	}
	if !strings.Contains(err.Error(), "at least one command") {
		t.Fatalf("expected empty config error, got %v", err)
	}
}
