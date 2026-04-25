package metrics

import "testing"

func TestScanCounters_Merge(t *testing.T) {
	t.Parallel()

	t.Run("adds all fields", func(t *testing.T) {
		t.Parallel()
		base := ScanCounters{
			CommandsDispatched:         1,
			TasksDispatched:            2,
			TasksCompleted:             3,
			TasksFailed:                4,
			TasksCancelled:             5,
			DeadLetters:                6,
			ReconciliationRepairs:      7,
			NotificationRetries:        8,
			SignalDeliveries:           9,
			SignalRetries:              10,
			SignalDeadLetters:          11,
			SignalInlineRetrySuccesses: 12,
			LeaseRenewals:              13,
			LeaseExtensions:            14,
			LeaseReleases:              15,
		}
		other := ScanCounters{
			CommandsDispatched:         10,
			TasksDispatched:            20,
			TasksCompleted:             30,
			TasksFailed:                40,
			TasksCancelled:             50,
			DeadLetters:                60,
			ReconciliationRepairs:      70,
			NotificationRetries:        80,
			SignalDeliveries:           90,
			SignalRetries:              100,
			SignalDeadLetters:          110,
			SignalInlineRetrySuccesses: 120,
			LeaseRenewals:              130,
			LeaseExtensions:            140,
			LeaseReleases:              150,
		}

		base.Merge(other)

		if base.CommandsDispatched != 11 {
			t.Errorf("CommandsDispatched = %d, want 11", base.CommandsDispatched)
		}
		if base.TasksDispatched != 22 {
			t.Errorf("TasksDispatched = %d, want 22", base.TasksDispatched)
		}
		if base.TasksCompleted != 33 {
			t.Errorf("TasksCompleted = %d, want 33", base.TasksCompleted)
		}
		if base.TasksFailed != 44 {
			t.Errorf("TasksFailed = %d, want 44", base.TasksFailed)
		}
		if base.TasksCancelled != 55 {
			t.Errorf("TasksCancelled = %d, want 55", base.TasksCancelled)
		}
		if base.DeadLetters != 66 {
			t.Errorf("DeadLetters = %d, want 66", base.DeadLetters)
		}
		if base.ReconciliationRepairs != 77 {
			t.Errorf("ReconciliationRepairs = %d, want 77", base.ReconciliationRepairs)
		}
		if base.NotificationRetries != 88 {
			t.Errorf("NotificationRetries = %d, want 88", base.NotificationRetries)
		}
		if base.SignalDeliveries != 99 {
			t.Errorf("SignalDeliveries = %d, want 99", base.SignalDeliveries)
		}
		if base.SignalRetries != 110 {
			t.Errorf("SignalRetries = %d, want 110", base.SignalRetries)
		}
		if base.SignalDeadLetters != 121 {
			t.Errorf("SignalDeadLetters = %d, want 121", base.SignalDeadLetters)
		}
		if base.SignalInlineRetrySuccesses != 132 {
			t.Errorf("SignalInlineRetrySuccesses = %d, want 132", base.SignalInlineRetrySuccesses)
		}
		if base.LeaseRenewals != 143 {
			t.Errorf("LeaseRenewals = %d, want 143", base.LeaseRenewals)
		}
		if base.LeaseExtensions != 154 {
			t.Errorf("LeaseExtensions = %d, want 154", base.LeaseExtensions)
		}
		if base.LeaseReleases != 165 {
			t.Errorf("LeaseReleases = %d, want 165", base.LeaseReleases)
		}
	})

	t.Run("merge zero is no-op", func(t *testing.T) {
		t.Parallel()
		c := ScanCounters{SignalInlineRetrySuccesses: 5, TasksDispatched: 3}
		c.Merge(ScanCounters{})
		if c.SignalInlineRetrySuccesses != 5 {
			t.Errorf("SignalInlineRetrySuccesses = %d, want 5", c.SignalInlineRetrySuccesses)
		}
		if c.TasksDispatched != 3 {
			t.Errorf("TasksDispatched = %d, want 3", c.TasksDispatched)
		}
	})
}
