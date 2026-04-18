package daemon

import (
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
)

func TestHasInProgressTasks(t *testing.T) {
	tests := []struct {
		name  string
		tasks []model.Task
		want  bool
	}{
		{"nil", nil, false},
		{"empty", []model.Task{}, false},
		{"pending only", []model.Task{{Status: model.StatusPending}}, false},
		{"completed only", []model.Task{{Status: model.StatusCompleted}}, false},
		{"in_progress", []model.Task{{Status: model.StatusInProgress}}, true},
		{"mixed with in_progress", []model.Task{
			{Status: model.StatusCompleted},
			{Status: model.StatusInProgress},
		}, true},
		{"mixed without in_progress", []model.Task{
			{Status: model.StatusPending},
			{Status: model.StatusCompleted},
			{Status: model.StatusFailed},
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasInProgressTasks(tt.tasks); got != tt.want {
				t.Errorf("hasInProgressTasks() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHasInProgressCommands(t *testing.T) {
	tests := []struct {
		name     string
		commands []model.Command
		want     bool
	}{
		{"nil", nil, false},
		{"empty", []model.Command{}, false},
		{"pending only", []model.Command{{Status: model.StatusPending}}, false},
		{"in_progress", []model.Command{{Status: model.StatusInProgress}}, true},
		{"completed", []model.Command{{Status: model.StatusCompleted}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasInProgressCommands(tt.commands); got != tt.want {
				t.Errorf("hasInProgressCommands() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHasInProgressNotifications(t *testing.T) {
	tests := []struct {
		name          string
		notifications []model.Notification
		want          bool
	}{
		{"nil", nil, false},
		{"empty", []model.Notification{}, false},
		{"pending only", []model.Notification{{Status: model.StatusPending}}, false},
		{"completed only", []model.Notification{{Status: model.StatusCompleted}}, false},
		{"in_progress", []model.Notification{{Status: model.StatusInProgress}}, true},
		{"completed after dispatch", []model.Notification{
			{Status: model.StatusCompleted},
			{Status: model.StatusPending},
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasInProgressNotifications(tt.notifications); got != tt.want {
				t.Errorf("hasInProgressNotifications() = %v, want %v", got, tt.want)
			}
		})
	}
}
