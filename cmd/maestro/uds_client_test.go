package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/msageha/maestro_v2/internal/uds"
)

type mockUDSClient struct {
	sendCommandFunc        func(command string, params any) (*uds.Response, error)
	sendCommandContextFunc func(ctx context.Context, command string, params any) (*uds.Response, error)
}

func (m *mockUDSClient) SendCommand(command string, params any) (*uds.Response, error) {
	return m.sendCommandFunc(command, params)
}

func (m *mockUDSClient) SendCommandContext(ctx context.Context, command string, params any) (*uds.Response, error) {
	if m.sendCommandContextFunc != nil {
		return m.sendCommandContextFunc(ctx, command, params)
	}
	return m.sendCommandFunc(command, params)
}

func withMaestroDir(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	maestroDir := filepath.Join(dir, ".maestro")
	if err := os.MkdirAll(maestroDir, 0o755); err != nil {
		t.Fatal(err)
	}
	origDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origDir) })
}

func withMockUDS(t *testing.T, client udsClientIface) {
	t.Helper()
	orig := newUDSClient
	newUDSClient = func(string) udsClientIface { return client }
	t.Cleanup(func() { newUDSClient = orig })
}

func successResponse(data any) *uds.Response {
	var raw json.RawMessage
	if data != nil {
		raw, _ = json.Marshal(data)
	}
	return &uds.Response{Success: true, Data: raw}
}

func errorResponse(code, message string) *uds.Response {
	return uds.ErrorResponse(code, message)
}
