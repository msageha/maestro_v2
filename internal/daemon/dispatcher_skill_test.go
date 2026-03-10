package daemon

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/model"
)

// createSkillFile creates a SKILL.md file under skillsDir/<name>/SKILL.md with the given content.
func createSkillFile(t *testing.T, skillsDir, name, content string) {
	t.Helper()
	dir := filepath.Join(skillsDir, name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("create skill dir %s: %v", name, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0644); err != nil {
		t.Fatalf("write SKILL.md for %s: %v", name, err)
	}
}

// newSkillTestDispatcher creates a Dispatcher with a mock executor that captures the dispatched envelope.
func newSkillTestDispatcher(t *testing.T, maestroDir string, cfg model.Config) (*Dispatcher, *mockExecutor) {
	t.Helper()
	d := NewDispatcher(maestroDir, cfg, nil, log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)
	mock := &mockExecutor{result: agent.ExecResult{Success: true}}
	d.SetExecutorFactory(func(string, model.WatcherConfig, string) (AgentExecutor, error) {
		return mock, nil
	})
	return d, mock
}

func TestDispatchTask_SkillInjection_Success(t *testing.T) {
	maestroDir := t.TempDir()
	skillsDir := filepath.Join(maestroDir, "skills")
	createSkillFile(t, skillsDir, "go-testing", "---\nname: Go Testing\n---\nAlways use t.Helper() in test helpers.\n")

	cfg := model.Config{
		Skills: model.SkillsConfig{Enabled: true},
	}
	d, mock := newSkillTestDispatcher(t, maestroDir, cfg)

	task := &model.Task{
		ID:        "task_001",
		CommandID: "cmd_001",
		Purpose:   "test",
		Content:   "original content",
		SkillRefs: []string{"go-testing"},
	}

	if err := d.DispatchTask(task, "worker1"); err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}

	if len(mock.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(mock.calls))
	}

	envelope := mock.calls[0].Message
	if !strings.Contains(envelope, "original content") {
		t.Error("envelope should contain original content")
	}
	if !strings.Contains(envelope, "スキル: Go Testing") {
		t.Error("envelope should contain injected skill section")
	}
	if !strings.Contains(envelope, "Always use t.Helper()") {
		t.Error("envelope should contain skill body")
	}
}

func TestDispatchTask_SkillRefs_Empty_NoInjection(t *testing.T) {
	maestroDir := t.TempDir()
	skillsDir := filepath.Join(maestroDir, "skills")
	createSkillFile(t, skillsDir, "go-testing", "---\nname: Go Testing\n---\nBody\n")

	cfg := model.Config{
		Skills: model.SkillsConfig{Enabled: true},
	}
	d, mock := newSkillTestDispatcher(t, maestroDir, cfg)

	task := &model.Task{
		ID:        "task_001",
		CommandID: "cmd_001",
		Purpose:   "test",
		Content:   "original content",
		SkillRefs: nil, // empty
	}

	if err := d.DispatchTask(task, "worker1"); err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}

	envelope := mock.calls[0].Message
	if strings.Contains(envelope, "スキル:") {
		t.Error("envelope should NOT contain skill section when SkillRefs is empty")
	}
}

func TestDispatchTask_SkillsDisabled_NoInjection(t *testing.T) {
	maestroDir := t.TempDir()
	skillsDir := filepath.Join(maestroDir, "skills")
	createSkillFile(t, skillsDir, "go-testing", "---\nname: Go Testing\n---\nBody\n")

	cfg := model.Config{
		Skills: model.SkillsConfig{Enabled: false},
	}
	d, mock := newSkillTestDispatcher(t, maestroDir, cfg)

	task := &model.Task{
		ID:        "task_001",
		CommandID: "cmd_001",
		Purpose:   "test",
		Content:   "original content",
		SkillRefs: []string{"go-testing"},
	}

	if err := d.DispatchTask(task, "worker1"); err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}

	envelope := mock.calls[0].Message
	if strings.Contains(envelope, "スキル:") {
		t.Error("envelope should NOT contain skill section when skills are disabled")
	}
}

func TestDispatchTask_MissingRefPolicy_Warn(t *testing.T) {
	maestroDir := t.TempDir()
	// No skills directory created — refs will be missing

	cfg := model.Config{
		Skills: model.SkillsConfig{
			Enabled:          true,
			MissingRefPolicy: "warn",
		},
	}
	d, mock := newSkillTestDispatcher(t, maestroDir, cfg)

	task := &model.Task{
		ID:        "task_001",
		CommandID: "cmd_001",
		Purpose:   "test",
		Content:   "original content",
		SkillRefs: []string{"nonexistent-skill"},
	}

	// Should succeed (warn policy skips missing refs)
	if err := d.DispatchTask(task, "worker1"); err != nil {
		t.Fatalf("dispatch should succeed with warn policy, got: %v", err)
	}

	envelope := mock.calls[0].Message
	if strings.Contains(envelope, "スキル:") {
		t.Error("envelope should NOT contain skill section when all refs are missing")
	}
}

func TestDispatchTask_MissingRefPolicy_Error(t *testing.T) {
	maestroDir := t.TempDir()
	// No skills directory created — refs will be missing

	cfg := model.Config{
		Skills: model.SkillsConfig{
			Enabled:          true,
			MissingRefPolicy: "error",
		},
	}
	d, _ := newSkillTestDispatcher(t, maestroDir, cfg)

	task := &model.Task{
		ID:        "task_001",
		CommandID: "cmd_001",
		Purpose:   "test",
		Content:   "original content",
		SkillRefs: []string{"nonexistent-skill"},
	}

	// Should fail with error policy
	err := d.DispatchTask(task, "worker1")
	if err == nil {
		t.Fatal("dispatch should fail with error policy for missing skill ref")
	}
	if !strings.Contains(err.Error(), "nonexistent-skill") {
		t.Errorf("error should mention the missing ref, got: %v", err)
	}
}

func TestDispatchTask_MaxRefsPerTask_Truncation(t *testing.T) {
	maestroDir := t.TempDir()
	skillsDir := filepath.Join(maestroDir, "skills")
	createSkillFile(t, skillsDir, "skill-a", "---\nname: Skill A\n---\nBody A\n")
	createSkillFile(t, skillsDir, "skill-b", "---\nname: Skill B\n---\nBody B\n")
	createSkillFile(t, skillsDir, "skill-c", "---\nname: Skill C\n---\nBody C\n")

	cfg := model.Config{
		Skills: model.SkillsConfig{
			Enabled:        true,
			MaxRefsPerTask: model.IntPtr(2), // Only allow 2, but task has 3
		},
	}
	d, mock := newSkillTestDispatcher(t, maestroDir, cfg)

	task := &model.Task{
		ID:        "task_001",
		CommandID: "cmd_001",
		Purpose:   "test",
		Content:   "original content",
		SkillRefs: []string{"skill-a", "skill-b", "skill-c"},
	}

	if err := d.DispatchTask(task, "worker1"); err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}

	envelope := mock.calls[0].Message
	// Should contain skill-a and skill-b (first 2), but NOT skill-c
	if !strings.Contains(envelope, "Skill A") {
		t.Error("envelope should contain Skill A")
	}
	if !strings.Contains(envelope, "Skill B") {
		t.Error("envelope should contain Skill B")
	}
	if strings.Contains(envelope, "Skill C") {
		t.Error("envelope should NOT contain Skill C (truncated)")
	}
}

func TestDispatchTask_SkillInjection_MultipleSkills(t *testing.T) {
	maestroDir := t.TempDir()
	skillsDir := filepath.Join(maestroDir, "skills")
	createSkillFile(t, skillsDir, "skill-a", "---\nname: Skill A\n---\nBody A content\n")
	createSkillFile(t, skillsDir, "skill-b", "---\nname: Skill B\n---\nBody B content\n")

	cfg := model.Config{
		Skills: model.SkillsConfig{Enabled: true},
	}
	d, mock := newSkillTestDispatcher(t, maestroDir, cfg)

	task := &model.Task{
		ID:        "task_001",
		CommandID: "cmd_001",
		Purpose:   "test",
		Content:   "original content",
		SkillRefs: []string{"skill-a", "skill-b"},
	}

	if err := d.DispatchTask(task, "worker1"); err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}

	envelope := mock.calls[0].Message
	if !strings.Contains(envelope, "Skill A") || !strings.Contains(envelope, "Skill B") {
		t.Error("envelope should contain both skills")
	}
	if !strings.Contains(envelope, "Body A content") || !strings.Contains(envelope, "Body B content") {
		t.Error("envelope should contain both skill bodies")
	}
}

func TestDispatchTask_MissingRefPolicy_Warn_PartialSkills(t *testing.T) {
	maestroDir := t.TempDir()
	skillsDir := filepath.Join(maestroDir, "skills")
	createSkillFile(t, skillsDir, "existing-skill", "---\nname: Existing Skill\n---\nExisting body\n")

	cfg := model.Config{
		Skills: model.SkillsConfig{
			Enabled:          true,
			MissingRefPolicy: "warn",
		},
	}
	d, mock := newSkillTestDispatcher(t, maestroDir, cfg)

	task := &model.Task{
		ID:        "task_001",
		CommandID: "cmd_001",
		Purpose:   "test",
		Content:   "original content",
		SkillRefs: []string{"existing-skill", "nonexistent-skill"},
	}

	if err := d.DispatchTask(task, "worker1"); err != nil {
		t.Fatalf("dispatch should succeed with warn policy, got: %v", err)
	}

	envelope := mock.calls[0].Message
	if !strings.Contains(envelope, "Existing Skill") {
		t.Error("envelope should contain the existing skill")
	}
}
