package model

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestWorktreeStateMarshalUnmarshal(t *testing.T) {
	ws := WorktreeState{
		CommandID: "cmd_1771722000_a3f2b7c1",
		WorkerID:  "worker1",
		Path:      "/tmp/worktrees/worker1",
		Branch:    "maestro/worker1/cmd_a3f2b7c1",
		BaseSHA:   "abc123def456",
		Status:    WorktreeStatusActive,
		CreatedAt: "2026-02-23T10:00:00+09:00",
		UpdatedAt: "2026-02-23T10:05:00+09:00",
	}

	data, err := yaml.Marshal(&ws)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded WorktreeState
	if err := yaml.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if decoded.CommandID != ws.CommandID {
		t.Errorf("command_id: got %q, want %q", decoded.CommandID, ws.CommandID)
	}
	if decoded.WorkerID != ws.WorkerID {
		t.Errorf("worker_id: got %q, want %q", decoded.WorkerID, ws.WorkerID)
	}
	if decoded.Path != ws.Path {
		t.Errorf("path: got %q, want %q", decoded.Path, ws.Path)
	}
	if decoded.Branch != ws.Branch {
		t.Errorf("branch: got %q, want %q", decoded.Branch, ws.Branch)
	}
	if decoded.BaseSHA != ws.BaseSHA {
		t.Errorf("base_sha: got %q, want %q", decoded.BaseSHA, ws.BaseSHA)
	}
	if decoded.Status != ws.Status {
		t.Errorf("status: got %q, want %q", decoded.Status, ws.Status)
	}
}

func TestWorktreeStatusConstants(t *testing.T) {
	tests := []struct {
		status WorktreeStatus
		want   string
	}{
		{WorktreeStatusCreated, "created"},
		{WorktreeStatusActive, "active"},
		{WorktreeStatusCommitted, "committed"},
		{WorktreeStatusIntegrated, "integrated"},
		{WorktreeStatusPublished, "published"},
		{WorktreeStatusCleanupDone, "cleanup_done"},
		{WorktreeStatusConflict, "conflict"},
		{WorktreeStatusFailed, "failed"},
		{WorktreeStatusCleanupFailed, "cleanup_failed"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if string(tt.status) != tt.want {
				t.Errorf("got %q, want %q", tt.status, tt.want)
			}
		})
	}
}

func TestIntegrationStateMarshalUnmarshal(t *testing.T) {
	is := IntegrationState{
		CommandID: "cmd_1771722000_a3f2b7c1",
		Branch:    "maestro/integrate/cmd_a3f2b7c1",
		BaseSHA:   "def456abc789",
		Status:    IntegrationStatusMerged,
		CreatedAt: "2026-02-23T10:00:00+09:00",
		UpdatedAt: "2026-02-23T10:10:00+09:00",
	}

	data, err := yaml.Marshal(&is)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded IntegrationState
	if err := yaml.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if decoded.CommandID != is.CommandID {
		t.Errorf("command_id: got %q, want %q", decoded.CommandID, is.CommandID)
	}
	if decoded.Branch != is.Branch {
		t.Errorf("branch: got %q, want %q", decoded.Branch, is.Branch)
	}
	if decoded.Status != is.Status {
		t.Errorf("status: got %q, want %q", decoded.Status, is.Status)
	}
}

func TestIntegrationStatusConstants(t *testing.T) {
	tests := []struct {
		status IntegrationStatus
		want   string
	}{
		{IntegrationStatusCreated, "created"},
		{IntegrationStatusMerging, "merging"},
		{IntegrationStatusMerged, "merged"},
		{IntegrationStatusPublishing, "publishing"},
		{IntegrationStatusPublished, "published"},
		{IntegrationStatusConflict, "conflict"},
		{IntegrationStatusFailed, "failed"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if string(tt.status) != tt.want {
				t.Errorf("got %q, want %q", tt.status, tt.want)
			}
		})
	}
}

func TestMergeConflictMarshalUnmarshal(t *testing.T) {
	mc := MergeConflict{
		WorkerID:      "worker2",
		ConflictFiles: []string{"src/main.go", "src/util.go"},
		Message:       "conflict in main.go lines 10-20",
	}

	data, err := yaml.Marshal(&mc)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded MergeConflict
	if err := yaml.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if decoded.WorkerID != mc.WorkerID {
		t.Errorf("worker_id: got %q, want %q", decoded.WorkerID, mc.WorkerID)
	}
	if len(decoded.ConflictFiles) != 2 {
		t.Fatalf("conflict_files: expected 2, got %d", len(decoded.ConflictFiles))
	}
	if decoded.ConflictFiles[0] != "src/main.go" {
		t.Errorf("conflict_files[0]: got %q", decoded.ConflictFiles[0])
	}
	if decoded.Message != mc.Message {
		t.Errorf("message: got %q, want %q", decoded.Message, mc.Message)
	}
}

func TestWorktreeCommandStateMarshalUnmarshal(t *testing.T) {
	wcs := WorktreeCommandState{
		SchemaVersion: 1,
		FileType:      "state_worktree",
		CommandID:     "cmd_1771722000_a3f2b7c1",
		Integration: IntegrationState{
			CommandID: "cmd_1771722000_a3f2b7c1",
			Branch:    "maestro/integrate/cmd_a3f2b7c1",
			BaseSHA:   "abc123",
			Status:    IntegrationStatusCreated,
			CreatedAt: "2026-02-23T10:00:00+09:00",
			UpdatedAt: "2026-02-23T10:00:00+09:00",
		},
		Workers: []WorktreeState{
			{
				CommandID: "cmd_1771722000_a3f2b7c1",
				WorkerID:  "worker1",
				Path:      "/tmp/worktrees/worker1",
				Branch:    "maestro/worker1/cmd_a3f2b7c1",
				BaseSHA:   "abc123",
				Status:    WorktreeStatusActive,
				CreatedAt: "2026-02-23T10:00:00+09:00",
				UpdatedAt: "2026-02-23T10:00:00+09:00",
			},
			{
				CommandID: "cmd_1771722000_a3f2b7c1",
				WorkerID:  "worker2",
				Path:      "/tmp/worktrees/worker2",
				Branch:    "maestro/worker2/cmd_a3f2b7c1",
				BaseSHA:   "abc123",
				Status:    WorktreeStatusCreated,
				CreatedAt: "2026-02-23T10:00:00+09:00",
				UpdatedAt: "2026-02-23T10:00:00+09:00",
			},
		},
		MergedPhases: map[string]string{
			"phase_001": "2026-02-23T10:15:00+09:00",
		},
		CreatedAt: "2026-02-23T10:00:00+09:00",
		UpdatedAt: "2026-02-23T10:15:00+09:00",
	}

	data, err := yaml.Marshal(&wcs)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded WorktreeCommandState
	if err := yaml.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if decoded.SchemaVersion != 1 {
		t.Errorf("schema_version: got %d, want 1", decoded.SchemaVersion)
	}
	if decoded.CommandID != wcs.CommandID {
		t.Errorf("command_id: got %q, want %q", decoded.CommandID, wcs.CommandID)
	}
	if decoded.Integration.Status != IntegrationStatusCreated {
		t.Errorf("integration.status: got %q, want %q", decoded.Integration.Status, IntegrationStatusCreated)
	}
	if len(decoded.Workers) != 2 {
		t.Fatalf("workers: expected 2, got %d", len(decoded.Workers))
	}
	if decoded.Workers[0].WorkerID != "worker1" {
		t.Errorf("workers[0].worker_id: got %q", decoded.Workers[0].WorkerID)
	}
	if decoded.Workers[1].Status != WorktreeStatusCreated {
		t.Errorf("workers[1].status: got %q", decoded.Workers[1].Status)
	}
	if decoded.MergedPhases["phase_001"] != "2026-02-23T10:15:00+09:00" {
		t.Errorf("merged_phases[phase_001]: got %q", decoded.MergedPhases["phase_001"])
	}
}

func TestWorktreeCommandState_EmptyOptionals(t *testing.T) {
	wcs := WorktreeCommandState{
		SchemaVersion: 1,
		FileType:      "state_worktree",
		CommandID:     "cmd_001",
		Integration: IntegrationState{
			CommandID: "cmd_001",
			Branch:    "branch",
			BaseSHA:   "sha",
			Status:    IntegrationStatusCreated,
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
		},
		Workers:   []WorktreeState{},
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
	}

	data, err := yaml.Marshal(&wcs)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded WorktreeCommandState
	if err := yaml.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if len(decoded.Workers) != 0 {
		t.Errorf("workers: expected empty, got %d", len(decoded.Workers))
	}
	if decoded.MergedPhases != nil && len(decoded.MergedPhases) != 0 {
		t.Errorf("merged_phases: expected nil/empty, got %v", decoded.MergedPhases)
	}
}
