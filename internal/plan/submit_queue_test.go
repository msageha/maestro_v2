package plan

import (
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
)

func TestApplyTaskDefaults_NilExpectedPaths(t *testing.T) {
	tasks := []TaskInput{
		{
			Name:               "t1",
			Purpose:            "p",
			Content:            "c",
			AcceptanceCriteria: "ac",
			BloomLevel:         1,
		},
	}

	ApplyTaskDefaults(tasks)

	if tasks[0].ExpectedPaths == nil {
		t.Fatal("ExpectedPaths should not be nil after ApplyTaskDefaults")
	}
	if len(tasks[0].ExpectedPaths) != 0 {
		t.Errorf("ExpectedPaths should be empty, got %v", tasks[0].ExpectedPaths)
	}
}

func TestApplyTaskDefaults_NilDefinitionOfAbort(t *testing.T) {
	tasks := []TaskInput{
		{
			Name:               "t1",
			Purpose:            "p",
			Content:            "c",
			AcceptanceCriteria: "ac",
			BloomLevel:         1,
		},
	}

	ApplyTaskDefaults(tasks)

	if tasks[0].DefinitionOfAbort == nil {
		t.Fatal("DefinitionOfAbort should not be nil after ApplyTaskDefaults")
	}
	expected := model.DefaultDefinitionOfAbort()
	if tasks[0].DefinitionOfAbort.MaxRepairCount != expected.MaxRepairCount {
		t.Errorf("MaxRepairCount = %d, want %d", tasks[0].DefinitionOfAbort.MaxRepairCount, expected.MaxRepairCount)
	}
	if tasks[0].DefinitionOfAbort.MaxWallClockSec != expected.MaxWallClockSec {
		t.Errorf("MaxWallClockSec = %d, want %d", tasks[0].DefinitionOfAbort.MaxWallClockSec, expected.MaxWallClockSec)
	}
}

func TestApplyTaskDefaults_PreservesExistingValues(t *testing.T) {
	customAbort := &model.DefinitionOfAbort{
		MaxRepairCount:  10,
		MaxWallClockSec: 7200,
	}
	tasks := []TaskInput{
		{
			Name:               "t1",
			Purpose:            "p",
			Content:            "c",
			AcceptanceCriteria: "ac",
			BloomLevel:         1,
			ExpectedPaths:      []string{"src/main.go"},
			DefinitionOfAbort:  customAbort,
		},
	}

	ApplyTaskDefaults(tasks)

	if len(tasks[0].ExpectedPaths) != 1 || tasks[0].ExpectedPaths[0] != "src/main.go" {
		t.Errorf("ExpectedPaths should be preserved, got %v", tasks[0].ExpectedPaths)
	}
	if tasks[0].DefinitionOfAbort.MaxRepairCount != 10 {
		t.Errorf("MaxRepairCount should be preserved, got %d", tasks[0].DefinitionOfAbort.MaxRepairCount)
	}
	if tasks[0].DefinitionOfAbort.MaxWallClockSec != 7200 {
		t.Errorf("MaxWallClockSec should be preserved, got %d", tasks[0].DefinitionOfAbort.MaxWallClockSec)
	}
}

func TestApplyTaskDefaults_MultipleTasks(t *testing.T) {
	tasks := []TaskInput{
		{Name: "t1", Purpose: "p", Content: "c", AcceptanceCriteria: "ac", BloomLevel: 1},
		{Name: "t2", Purpose: "p", Content: "c", AcceptanceCriteria: "ac", BloomLevel: 2,
			ExpectedPaths: []string{"existing.go"}},
	}

	ApplyTaskDefaults(tasks)

	for i, task := range tasks {
		if task.ExpectedPaths == nil {
			t.Errorf("tasks[%d].ExpectedPaths should not be nil", i)
		}
		if task.DefinitionOfAbort == nil {
			t.Errorf("tasks[%d].DefinitionOfAbort should not be nil", i)
		}
	}
	if len(tasks[1].ExpectedPaths) != 1 {
		t.Errorf("tasks[1].ExpectedPaths should be preserved with 1 item, got %d", len(tasks[1].ExpectedPaths))
	}
}
