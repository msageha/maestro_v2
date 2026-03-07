package plan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
)

func TestSubmit_SkillRefs_PropagatedToQueue(t *testing.T) {
	maestroDir := setupMaestroDir(t)
	cfg := testConfig()
	commandID := "cmd_0000000001_aabbccdd"

	writePlannerQueue(t, maestroDir, commandID, model.StatusInProgress)

	tasksFile := writeTasksFile(t, []TaskInput{
		{
			Name:               "task_with_skills",
			Purpose:            "test skill_refs propagation",
			Content:            "implement something",
			AcceptanceCriteria: "done",
			BloomLevel:         2,
			Required:           true,
			SkillRefs:          []string{"go-testing", "code-review"},
		},
	})

	result, err := Submit(SubmitOptions{
		CommandID:  commandID,
		TasksFile:  tasksFile,
		MaestroDir: maestroDir,
		Config:     cfg,
		LockMap:    lock.NewMutexMap(),
	})
	if err != nil {
		t.Fatalf("Submit returned error: %v", err)
	}

	if len(result.Tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(result.Tasks))
	}

	// Find the queue file for the assigned worker
	workerID := result.Tasks[0].Worker
	queueFile := filepath.Join(maestroDir, "queue", workerID+".yaml")
	data, err := os.ReadFile(queueFile)
	if err != nil {
		t.Fatalf("read queue file: %v", err)
	}

	var tq model.TaskQueue
	if err := yamlv3.Unmarshal(data, &tq); err != nil {
		t.Fatalf("parse queue file: %v", err)
	}

	// Find the task in the queue
	var found *model.Task
	for i := range tq.Tasks {
		if tq.Tasks[i].ID == result.Tasks[0].TaskID {
			found = &tq.Tasks[i]
			break
		}
	}
	if found == nil {
		t.Fatal("task not found in queue file")
	}

	if len(found.SkillRefs) != 2 {
		t.Fatalf("expected 2 skill_refs in queue, got %d", len(found.SkillRefs))
	}
	if found.SkillRefs[0] != "go-testing" || found.SkillRefs[1] != "code-review" {
		t.Errorf("skill_refs mismatch: got %v", found.SkillRefs)
	}
}

func TestSubmit_SkillRefs_EmptyNotPropagated(t *testing.T) {
	maestroDir := setupMaestroDir(t)
	cfg := testConfig()
	commandID := "cmd_0000000002_aabbccdd"

	writePlannerQueue(t, maestroDir, commandID, model.StatusInProgress)

	tasksFile := writeTasksFile(t, []TaskInput{
		{
			Name:               "task_no_skills",
			Purpose:            "test no skill_refs",
			Content:            "implement something",
			AcceptanceCriteria: "done",
			BloomLevel:         2,
			Required:           true,
			// No SkillRefs
		},
	})

	result, err := Submit(SubmitOptions{
		CommandID:  commandID,
		TasksFile:  tasksFile,
		MaestroDir: maestroDir,
		Config:     cfg,
		LockMap:    lock.NewMutexMap(),
	})
	if err != nil {
		t.Fatalf("Submit returned error: %v", err)
	}

	workerID := result.Tasks[0].Worker
	queueFile := filepath.Join(maestroDir, "queue", workerID+".yaml")
	data, err := os.ReadFile(queueFile)
	if err != nil {
		t.Fatalf("read queue file: %v", err)
	}

	var tq model.TaskQueue
	if err := yamlv3.Unmarshal(data, &tq); err != nil {
		t.Fatalf("parse queue file: %v", err)
	}

	var found *model.Task
	for i := range tq.Tasks {
		if tq.Tasks[i].ID == result.Tasks[0].TaskID {
			found = &tq.Tasks[i]
			break
		}
	}
	if found == nil {
		t.Fatal("task not found in queue file")
	}

	if len(found.SkillRefs) != 0 {
		t.Errorf("expected empty skill_refs, got %v", found.SkillRefs)
	}
}

func TestBuildWorkerEnvelope_SkillRefs_CommaSeparated(t *testing.T) {
	task := model.Task{
		ID:                 "task_001",
		CommandID:          "cmd_001",
		Purpose:            "test",
		Content:            "do something",
		AcceptanceCriteria: "done",
		SkillRefs:          []string{"go-testing", "code-review"},
	}

	envelope := agent.BuildWorkerEnvelope(task, "worker1", 1, 1)

	if !strings.Contains(envelope, "skill_refs: go-testing, code-review") {
		t.Errorf("envelope should contain comma-separated skill_refs, got envelope:\n%s", envelope)
	}
}

func TestBuildWorkerEnvelope_SkillRefs_Empty_ShowsNashi(t *testing.T) {
	task := model.Task{
		ID:                 "task_001",
		CommandID:          "cmd_001",
		Purpose:            "test",
		Content:            "do something",
		AcceptanceCriteria: "done",
	}

	envelope := agent.BuildWorkerEnvelope(task, "worker1", 1, 1)

	if !strings.Contains(envelope, "skill_refs: なし") {
		t.Errorf("envelope should show 'なし' for empty skill_refs, got envelope:\n%s", envelope)
	}
}
