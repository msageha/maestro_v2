package reconcile

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	yamlutil "github.com/msageha/maestro_v2/internal/yaml"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
)

// fakeClock is a deterministic Clock for testing.
type fakeClock struct {
	now time.Time
}

func (c *fakeClock) Now() time.Time { return c.now }

// harness provides test infrastructure for reconcile pattern tests.
type harness struct {
	t          *testing.T
	maestroDir string
	engine     *Engine
	clock      *fakeClock
}

func newHarness(t *testing.T, now time.Time, patterns ...Pattern) *harness {
	t.Helper()

	maestroDir := t.TempDir()

	// Create required directories.
	for _, sub := range []string{
		"queue",
		"results",
		filepath.Join("state", "commands"),
		"quarantine",
	} {
		if err := os.MkdirAll(filepath.Join(maestroDir, sub), 0755); err != nil {
			t.Fatalf("mkdir %s: %v", sub, err)
		}
	}

	clk := &fakeClock{now: now}
	dl := core.NewDaemonLogger("test", slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	deps := Deps{
		MaestroDir: maestroDir,
		Config: model.Config{
			Watcher: model.WatcherConfig{DispatchLeaseSec: 60},
		},
		LockMap: lock.NewMutexMap(),
		DL:      dl,
		Clock:   clk,
	}

	engine := NewEngine(deps, patterns...)

	return &harness{
		t:          t,
		maestroDir: maestroDir,
		engine:     engine,
		clock:      clk,
	}
}

func (h *harness) statePath(commandID string) string {
	return filepath.Join(h.maestroDir, "state", "commands", commandID+".yaml")
}

func (h *harness) plannerQueuePath() string {
	return filepath.Join(h.maestroDir, "queue", "planner.yaml")
}

func (h *harness) workerQueuePath(workerID string) string {
	return filepath.Join(h.maestroDir, "queue", workerID+".yaml")
}

func (h *harness) workerResultPath(workerID string) string {
	return filepath.Join(h.maestroDir, "results", workerID+".yaml")
}

func (h *harness) plannerResultPath() string {
	return filepath.Join(h.maestroDir, "results", "planner.yaml")
}

func (h *harness) orchestratorQueuePath() string {
	return filepath.Join(h.maestroDir, "queue", "orchestrator.yaml")
}

func (h *harness) mustWriteYAML(path string, v any) {
	h.t.Helper()
	if err := yamlutil.AtomicWrite(path, v); err != nil {
		h.t.Fatalf("mustWriteYAML %s: %v", path, err)
	}
}

func (h *harness) mustNotExist(path string) {
	h.t.Helper()
	_, err := os.Stat(path)
	if err == nil {
		h.t.Fatalf("expected %s to not exist, but it does", path)
	}
	if !os.IsNotExist(err) {
		h.t.Fatalf("unexpected error checking %s: %v", path, err)
	}
}

func (h *harness) mustExist(path string) {
	h.t.Helper()
	if _, err := os.Stat(path); err != nil {
		h.t.Fatalf("expected %s to exist: %v", path, err)
	}
}

// ts returns a time.Time offset from the harness clock's "now".
func (h *harness) ts(offset time.Duration) string {
	return h.clock.now.Add(offset).UTC().Format(time.RFC3339)
}
