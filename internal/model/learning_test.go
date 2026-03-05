package model

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestLearningMarshalUnmarshal(t *testing.T) {
	l := Learning{
		ResultID:  "res_1771722300_e5f0c3d8",
		CommandID: "cmd_1771722000_a3f2b7c1",
		Content:   "Always run go vet before committing",
		CreatedAt: "2026-02-23T10:05:00+09:00",
	}

	data, err := yaml.Marshal(&l)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded Learning
	if err := yaml.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if decoded.ResultID != l.ResultID {
		t.Errorf("result_id: got %q, want %q", decoded.ResultID, l.ResultID)
	}
	if decoded.CommandID != l.CommandID {
		t.Errorf("command_id: got %q, want %q", decoded.CommandID, l.CommandID)
	}
	if decoded.Content != l.Content {
		t.Errorf("content: got %q, want %q", decoded.Content, l.Content)
	}
	if decoded.CreatedAt != l.CreatedAt {
		t.Errorf("created_at: got %q, want %q", decoded.CreatedAt, l.CreatedAt)
	}
}

func TestLearningsFileMarshalUnmarshal(t *testing.T) {
	f := LearningsFile{
		SchemaVersion: 1,
		FileType:      "state_learnings",
		Learnings: []Learning{
			{
				ResultID:  "res_001",
				CommandID: "cmd_001",
				Content:   "First learning",
				CreatedAt: "2026-02-23T10:00:00+09:00",
			},
			{
				ResultID:  "res_002",
				CommandID: "cmd_001",
				Content:   "Second learning",
				CreatedAt: "2026-02-23T10:05:00+09:00",
			},
		},
	}

	data, err := yaml.Marshal(&f)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded LearningsFile
	if err := yaml.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if decoded.SchemaVersion != 1 {
		t.Errorf("schema_version: got %d, want 1", decoded.SchemaVersion)
	}
	if decoded.FileType != "state_learnings" {
		t.Errorf("file_type: got %q, want %q", decoded.FileType, "state_learnings")
	}
	if len(decoded.Learnings) != 2 {
		t.Fatalf("learnings: expected 2, got %d", len(decoded.Learnings))
	}
	if decoded.Learnings[0].Content != "First learning" {
		t.Errorf("learnings[0].content: got %q", decoded.Learnings[0].Content)
	}
	if decoded.Learnings[1].ResultID != "res_002" {
		t.Errorf("learnings[1].result_id: got %q", decoded.Learnings[1].ResultID)
	}
}

func TestLearningsFile_Empty(t *testing.T) {
	f := LearningsFile{
		SchemaVersion: 1,
		FileType:      "state_learnings",
		Learnings:     []Learning{},
	}

	data, err := yaml.Marshal(&f)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded LearningsFile
	if err := yaml.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if len(decoded.Learnings) != 0 {
		t.Errorf("learnings: expected empty, got %d", len(decoded.Learnings))
	}
}
