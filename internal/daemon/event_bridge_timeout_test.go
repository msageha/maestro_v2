package daemon

import (
	"bytes"
	"context"
	"log"
	"testing"
	"time"
)

func TestEventBridge_RunWithTimeout_FastCallback(t *testing.T) {
	t.Parallel()

	var logBuf bytes.Buffer
	logger := log.New(&logBuf, "", 0)

	d := &Daemon{
		logLevel: LogLevelDebug,
		logger:   logger,
		clock:    RealClock{},
	}
	d.ctx, d.cancel = context.WithCancel(context.Background())
	defer d.cancel()

	eb := &EventBridge{d: d}

	ok := eb.runWithTimeout("test_fast", func(_ context.Context) {
		// completes instantly
	})
	if !ok {
		t.Error("expected fast callback to complete within timeout")
	}
}

func TestEventBridge_CallbackTimeoutConstant(t *testing.T) {
	if eventBridgeCallbackTimeout != 10*time.Second {
		t.Errorf("expected eventBridgeCallbackTimeout=10s, got %s", eventBridgeCallbackTimeout)
	}
}
