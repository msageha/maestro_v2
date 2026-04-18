package model

import "testing"

func TestComputeTaskStats(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		tasks []CommandResultTask
		want  TaskStats
	}{
		{
			name:  "empty",
			tasks: nil,
			want:  TaskStats{},
		},
		{
			name: "all completed",
			tasks: []CommandResultTask{
				{TaskID: "t1", Status: StatusCompleted},
				{TaskID: "t2", Status: StatusCompleted},
				{TaskID: "t3", Status: StatusCompleted},
			},
			want: TaskStats{Total: 3, Completed: 3},
		},
		{
			name: "mixed statuses",
			tasks: []CommandResultTask{
				{TaskID: "t1", Status: StatusCompleted},
				{TaskID: "t2", Status: StatusFailed},
				{TaskID: "t3", Status: StatusCancelled},
				{TaskID: "t4", Status: StatusCompleted},
			},
			want: TaskStats{Total: 4, Completed: 2, Failed: 1, Cancelled: 1},
		},
		{
			name: "all failed",
			tasks: []CommandResultTask{
				{TaskID: "t1", Status: StatusFailed},
				{TaskID: "t2", Status: StatusFailed},
			},
			want: TaskStats{Total: 2, Failed: 2},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ComputeTaskStats(tt.tasks)
			if got != tt.want {
				t.Errorf("ComputeTaskStats() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
