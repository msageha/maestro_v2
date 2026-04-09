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

// createSkillFile creates a SKILL.md file under skillsDir/worker/<name>/SKILL.md with the given content.
func createSkillFile(t *testing.T, skillsDir, name, content string) {
	t.Helper()
	dir := filepath.Join(skillsDir, "worker", name)
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
	ep := newTestExecutorProvider(maestroDir, cfg)
	d := NewDispatcher(maestroDir, cfg, nil, log.New(&bytes.Buffer{}, "", 0), LogLevelDebug, ep, RealClock{})
	mock := &mockExecutor{result: agent.ExecResult{Success: true}}
	ep.SetFactory(func(string, model.WatcherConfig, string) (AgentExecutor, error) {
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

func TestDispatchTask_SkillRefs_Empty_NoShared_NoInjection(t *testing.T) {
	maestroDir := t.TempDir()
	skillsDir := filepath.Join(maestroDir, "skills")
	// Only create a worker-specific skill (no shared skills)
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
		t.Error("envelope should NOT contain skill section when no shared skills and no skill_refs")
	}
}

func TestDispatchTask_SharedSkills_AutoInjected(t *testing.T) {
	maestroDir := t.TempDir()
	skillsDir := filepath.Join(maestroDir, "skills")
	// Create shared skill only (no skill_refs needed)
	createSkillFileForRole(t, skillsDir, "share", "error-patterns", "---\nname: Error Patterns\n---\nDiagnose errors systematically.\n")

	cfg := model.Config{
		Skills: model.SkillsConfig{Enabled: true},
	}
	d, mock := newSkillTestDispatcher(t, maestroDir, cfg)

	task := &model.Task{
		ID:        "task_001",
		CommandID: "cmd_001",
		Purpose:   "test",
		Content:   "original content",
		SkillRefs: nil, // no explicit skill_refs
	}

	if err := d.DispatchTask(task, "worker1"); err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}

	envelope := mock.calls[0].Message
	if !strings.Contains(envelope, "スキル: Error Patterns") {
		t.Error("envelope should contain auto-injected shared skill")
	}
	if !strings.Contains(envelope, "Diagnose errors systematically.") {
		t.Error("envelope should contain shared skill body")
	}
}

func TestDispatchTask_SharedSkills_DeduplicatedWithSkillRefs(t *testing.T) {
	maestroDir := t.TempDir()
	skillsDir := filepath.Join(maestroDir, "skills")
	// Create a shared skill and a worker-specific override with the same name
	createSkillFileForRole(t, skillsDir, "share", "my-skill", "---\nname: Shared Version\n---\nShared body\n")
	createSkillFile(t, skillsDir, "my-skill", "---\nname: Worker Version\n---\nWorker body\n")

	cfg := model.Config{
		Skills: model.SkillsConfig{Enabled: true},
	}
	d, mock := newSkillTestDispatcher(t, maestroDir, cfg)

	task := &model.Task{
		ID:        "task_001",
		CommandID: "cmd_001",
		Purpose:   "test",
		Content:   "original content",
		SkillRefs: []string{"my-skill"}, // explicitly references the skill
	}

	if err := d.DispatchTask(task, "worker1"); err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}

	envelope := mock.calls[0].Message
	// Worker-specific version should be used (via ReadSkillWithRole fallback)
	if !strings.Contains(envelope, "Worker Version") {
		t.Error("envelope should contain worker-specific version of the skill")
	}
	// Should NOT contain the shared version as a duplicate
	if strings.Contains(envelope, "Shared Version") {
		t.Error("envelope should NOT contain shared version when worker-specific exists in skill_refs")
	}
}

func TestDispatchTask_SharedSkills_MixedWithSkillRefs(t *testing.T) {
	maestroDir := t.TempDir()
	skillsDir := filepath.Join(maestroDir, "skills")
	// Worker-specific skill
	createSkillFile(t, skillsDir, "impl-skill", "---\nname: Impl Skill\n---\nImplementation body\n")
	// Shared skill (different name, should be auto-injected)
	createSkillFileForRole(t, skillsDir, "share", "shared-skill", "---\nname: Shared Skill\n---\nShared body\n")

	cfg := model.Config{
		Skills: model.SkillsConfig{Enabled: true},
	}
	d, mock := newSkillTestDispatcher(t, maestroDir, cfg)

	task := &model.Task{
		ID:        "task_001",
		CommandID: "cmd_001",
		Purpose:   "test",
		Content:   "original content",
		SkillRefs: []string{"impl-skill"},
	}

	if err := d.DispatchTask(task, "worker1"); err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}

	envelope := mock.calls[0].Message
	if !strings.Contains(envelope, "スキル: Impl Skill") {
		t.Error("envelope should contain explicitly referenced skill")
	}
	if !strings.Contains(envelope, "スキル: Shared Skill") {
		t.Error("envelope should contain auto-injected shared skill")
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

// ---------------------------------------------------------------------------
// Planner skill injection tests
// ---------------------------------------------------------------------------

// createSkillFileForRole creates a SKILL.md file under skillsDir/<role>/<name>/SKILL.md.
func createSkillFileForRole(t *testing.T, skillsDir, role, name, content string) {
	t.Helper()
	dir := filepath.Join(skillsDir, role, name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("create skill dir %s/%s: %v", role, name, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0644); err != nil {
		t.Fatalf("write SKILL.md for %s/%s: %v", role, name, err)
	}
}

func TestDispatchCommand_PlannerSkillInjection(t *testing.T) {
	maestroDir := t.TempDir()
	skillsDir := filepath.Join(maestroDir, "skills")
	createSkillFileForRole(t, skillsDir, "planner", "plan-skill", "---\nname: Plan Skill\n---\nPlanner skill body\n")
	createSkillFileForRole(t, skillsDir, "share", "shared-skill", "---\nname: Shared Skill\n---\nShared skill body\n")

	cfg := model.Config{
		Skills: model.SkillsConfig{Enabled: true},
	}
	d, mock := newSkillTestDispatcher(t, maestroDir, cfg)

	cmd := &model.Command{
		ID:        "cmd_001",
		Content:   "implement feature X",
		SkillRefs: []string{"plan-skill"}, // Orchestrator selects planner skills
	}

	if err := d.DispatchCommand(cmd); err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}

	if len(mock.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(mock.calls))
	}

	envelope := mock.calls[0].Message
	if !strings.Contains(envelope, "implement feature X") {
		t.Error("envelope should contain original content")
	}
	if !strings.Contains(envelope, "スキル: Plan Skill") {
		t.Error("envelope should contain planner-specific skill from skill_refs")
	}
	if !strings.Contains(envelope, "Planner skill body") {
		t.Error("envelope should contain planner skill body")
	}
	if !strings.Contains(envelope, "スキル: Shared Skill") {
		t.Error("envelope should contain auto-injected shared skill")
	}
	if !strings.Contains(envelope, "Shared skill body") {
		t.Error("envelope should contain shared skill body")
	}
}

func TestDispatchCommand_NoSkillRefs_OnlySharedInjected(t *testing.T) {
	maestroDir := t.TempDir()
	skillsDir := filepath.Join(maestroDir, "skills")
	createSkillFileForRole(t, skillsDir, "planner", "plan-skill", "---\nname: Plan Skill\n---\nPlanner body\n")
	createSkillFileForRole(t, skillsDir, "share", "shared-skill", "---\nname: Shared Skill\n---\nShared body\n")

	cfg := model.Config{
		Skills: model.SkillsConfig{Enabled: true},
	}
	d, mock := newSkillTestDispatcher(t, maestroDir, cfg)

	cmd := &model.Command{
		ID:      "cmd_001",
		Content: "simple fix",
		// No SkillRefs — only shared skills should be injected
	}

	if err := d.DispatchCommand(cmd); err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}

	envelope := mock.calls[0].Message
	if strings.Contains(envelope, "Plan Skill") {
		t.Error("envelope should NOT contain planner skill when not in skill_refs")
	}
	if !strings.Contains(envelope, "Shared Skill") {
		t.Error("envelope should contain auto-injected shared skill")
	}
}

func TestDispatchCommand_SkillsDisabled_NoInjection(t *testing.T) {
	maestroDir := t.TempDir()
	skillsDir := filepath.Join(maestroDir, "skills")
	createSkillFileForRole(t, skillsDir, "planner", "plan-skill", "---\nname: Plan Skill\n---\nBody\n")

	cfg := model.Config{
		Skills: model.SkillsConfig{Enabled: false},
	}
	d, mock := newSkillTestDispatcher(t, maestroDir, cfg)

	cmd := &model.Command{
		ID:      "cmd_001",
		Content: "implement feature X",
	}

	if err := d.DispatchCommand(cmd); err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}

	envelope := mock.calls[0].Message
	if strings.Contains(envelope, "スキル:") {
		t.Error("envelope should NOT contain skill section when skills are disabled")
	}
}

func TestDispatchCommand_NoSkillsDir_NoError(t *testing.T) {
	maestroDir := t.TempDir()
	// No skills directory

	cfg := model.Config{
		Skills: model.SkillsConfig{Enabled: true},
	}
	d, mock := newSkillTestDispatcher(t, maestroDir, cfg)

	cmd := &model.Command{
		ID:      "cmd_001",
		Content: "implement feature X",
	}

	if err := d.DispatchCommand(cmd); err != nil {
		t.Fatalf("dispatch should succeed even without skills dir: %v", err)
	}

	envelope := mock.calls[0].Message
	if strings.Contains(envelope, "スキル:") {
		t.Error("envelope should NOT contain skill section when no skills exist")
	}
}

func TestDispatchCommand_OnlyPlannerSkills_NotWorkerSkills(t *testing.T) {
	maestroDir := t.TempDir()
	skillsDir := filepath.Join(maestroDir, "skills")
	createSkillFileForRole(t, skillsDir, "planner", "plan-skill", "---\nname: Plan Skill\n---\nPlanner body\n")
	createSkillFileForRole(t, skillsDir, "worker", "worker-skill", "---\nname: Worker Skill\n---\nWorker body\n")

	cfg := model.Config{
		Skills: model.SkillsConfig{Enabled: true},
	}
	d, mock := newSkillTestDispatcher(t, maestroDir, cfg)

	cmd := &model.Command{
		ID:        "cmd_001",
		Content:   "implement feature X",
		SkillRefs: []string{"plan-skill"},
	}

	if err := d.DispatchCommand(cmd); err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}

	envelope := mock.calls[0].Message
	if !strings.Contains(envelope, "スキル: Plan Skill") {
		t.Error("envelope should contain planner skill")
	}
	if strings.Contains(envelope, "Worker Skill") {
		t.Error("envelope should NOT contain worker-specific skill")
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
