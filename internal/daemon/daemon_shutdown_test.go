package daemon

import (
	"testing"
	"time"
)

func TestShutdownOpTimeout_Constant(t *testing.T) {
	if shutdownOpTimeout != 10*time.Second {
		t.Errorf("expected shutdownOpTimeout=10s, got %s", shutdownOpTimeout)
	}
}

func TestResultHandler_ReducedRetryConstants(t *testing.T) {
	if maxNotifyAttempts != 3 {
		t.Errorf("expected maxNotifyAttempts=3, got %d", maxNotifyAttempts)
	}
	if notifyBackoffMax != 5*time.Second {
		t.Errorf("expected notifyBackoffMax=5s, got %s", notifyBackoffMax)
	}
}
