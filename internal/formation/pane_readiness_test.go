package formation

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
)

func TestPreparePanes_AllSuccess(t *testing.T) {
	t.Parallel()
	waitFn := func(ctx context.Context, pane string) error { return nil }
	var killed []string
	killFn := func(pane string) { killed = append(killed, pane) }

	required := []string{"orch", "planner"}
	optional := []string{"w1", "w2"}
	ready, err := preparePanes(required, optional, time.Second, waitFn, killFn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ready) != 4 {
		t.Errorf("ready panes = %d, want 4", len(ready))
	}
	if len(killed) != 0 {
		t.Errorf("killed = %v, want none", killed)
	}
}

func TestPreparePanes_RequiredPaneFails(t *testing.T) {
	t.Parallel()
	waitFn := func(ctx context.Context, pane string) error {
		if pane == "planner" {
			return fmt.Errorf("timeout")
		}
		return nil
	}
	var killed []string
	killFn := func(pane string) { killed = append(killed, pane) }

	required := []string{"orch", "planner"}
	optional := []string{"w1"}
	_, err := preparePanes(required, optional, time.Second, waitFn, killFn)
	if err == nil {
		t.Fatal("expected error when required pane fails")
	}
	if len(killed) != 0 {
		t.Errorf("killed = %v, want none (workers not yet checked)", killed)
	}
}

func TestPreparePanes_OptionalPaneFails(t *testing.T) {
	t.Parallel()
	waitFn := func(ctx context.Context, pane string) error {
		if pane == "w2" {
			return fmt.Errorf("timeout")
		}
		return nil
	}
	var killed []string
	killFn := func(pane string) { killed = append(killed, pane) }

	required := []string{"orch", "planner"}
	optional := []string{"w1", "w2", "w3"}
	ready, err := preparePanes(required, optional, time.Second, waitFn, killFn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// orch + planner + w1 + w3 = 4
	if len(ready) != 4 {
		t.Errorf("ready panes = %d, want 4; got %v", len(ready), ready)
	}
	if len(killed) != 1 || killed[0] != "w2" {
		t.Errorf("killed = %v, want [w2]", killed)
	}
}

func TestPreparePanes_AllOptionalFail(t *testing.T) {
	t.Parallel()
	waitFn := func(ctx context.Context, pane string) error {
		if pane == "w1" || pane == "w2" {
			return fmt.Errorf("timeout")
		}
		return nil
	}
	var killed []string
	killFn := func(pane string) { killed = append(killed, pane) }

	required := []string{"orch", "planner"}
	optional := []string{"w1", "w2"}
	ready, err := preparePanes(required, optional, time.Second, waitFn, killFn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Only orchestrator + planner
	if len(ready) != 2 {
		t.Errorf("ready panes = %d, want 2", len(ready))
	}
	if len(killed) != 2 {
		t.Errorf("killed = %v, want [w1, w2]", killed)
	}
}

func TestPreparePanes_NoOptionalPanes(t *testing.T) {
	t.Parallel()
	waitFn := func(ctx context.Context, pane string) error { return nil }
	killFn := func(pane string) { t.Errorf("killFn should not be called") }

	required := []string{"orch", "planner"}
	ready, err := preparePanes(required, nil, time.Second, waitFn, killFn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ready) != 2 {
		t.Errorf("ready panes = %d, want 2", len(ready))
	}
}

func TestPreparePanes_MultipleOptionalFail(t *testing.T) {
	t.Parallel()
	waitFn := func(ctx context.Context, pane string) error {
		if pane == "w1" || pane == "w3" {
			return fmt.Errorf("timeout")
		}
		return nil
	}
	var killed []string
	killFn := func(pane string) { killed = append(killed, pane) }

	required := []string{"orch", "planner"}
	optional := []string{"w1", "w2", "w3", "w4"}
	ready, err := preparePanes(required, optional, time.Second, waitFn, killFn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// orch + planner + w2 + w4 = 4
	if len(ready) != 4 {
		t.Errorf("ready panes = %d, want 4; got %v", len(ready), ready)
	}
	if len(killed) != 2 {
		t.Errorf("killed count = %d, want 2; killed = %v", len(killed), killed)
	}
}

func TestPreparePanes_PreservesOrder(t *testing.T) {
	t.Parallel()
	waitFn := func(ctx context.Context, pane string) error { return nil }
	killFn := func(pane string) {}

	required := []string{"orch", "planner"}
	optional := []string{"w1", "w2"}
	ready, err := preparePanes(required, optional, time.Second, waitFn, killFn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := []string{"orch", "planner", "w1", "w2"}
	if len(ready) != len(expected) {
		t.Fatalf("ready = %v, want %v", ready, expected)
	}
	for i, p := range ready {
		if p != expected[i] {
			t.Errorf("ready[%d] = %s, want %s", i, p, expected[i])
		}
	}
}

func TestShellReadyTimeout_Default(t *testing.T) {
	t.Parallel()
	cfg := model.Config{} // ShellReadyTimeoutSec = 0 → default
	got := shellReadyTimeout(cfg)
	if got != 10*time.Second {
		t.Errorf("shellReadyTimeout() = %v, want 10s", got)
	}
}

func TestShellReadyTimeout_Configured(t *testing.T) {
	t.Parallel()
	cfg := model.Config{
		Watcher: model.WatcherConfig{ShellReadyTimeoutSec: 30},
	}
	got := shellReadyTimeout(cfg)
	if got != 30*time.Second {
		t.Errorf("shellReadyTimeout() = %v, want 30s", got)
	}
}
