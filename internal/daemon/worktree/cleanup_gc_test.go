package worktree

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestGCBakFiles_OrphanRemoved verifies that a .bak file with no matching
// .yaml is removed by gcBakFiles.
func TestGCBakFiles_OrphanRemoved(t *testing.T) {
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	stateDir := filepath.Join(wm.maestroDir, "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(stateDir, "orphan.yaml.bak")
	if err := os.WriteFile(orphan, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	wm.gcBakFiles()

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan .bak should have been removed, stat err=%v", err)
	}
}

// TestGCBakFiles_ExpiredRemoved verifies that a .bak file older than bakTTL
// is removed even when its companion .yaml still exists.
func TestGCBakFiles_ExpiredRemoved(t *testing.T) {
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	queuesDir := filepath.Join(wm.maestroDir, "queues")
	if err := os.MkdirAll(queuesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	yamlPath := filepath.Join(queuesDir, "q.yaml")
	bakPath := yamlPath + ".bak"
	if err := os.WriteFile(yamlPath, []byte("k: v\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bakPath, []byte("k: old\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	old := time.Now().Add(-2 * bakTTL)
	if err := os.Chtimes(bakPath, old, old); err != nil {
		t.Fatal(err)
	}

	wm.gcBakFiles()

	if _, err := os.Stat(bakPath); !os.IsNotExist(err) {
		t.Fatalf("expired .bak should have been removed, stat err=%v", err)
	}
	if _, err := os.Stat(yamlPath); err != nil {
		t.Fatalf(".yaml companion must remain: %v", err)
	}
}

// TestGCBakFiles_FreshRetained verifies that a recent .bak with a matching
// .yaml is preserved.
func TestGCBakFiles_FreshRetained(t *testing.T) {
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	resultsDir := filepath.Join(wm.maestroDir, "results")
	if err := os.MkdirAll(resultsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	yamlPath := filepath.Join(resultsDir, "r.yaml")
	bakPath := yamlPath + ".bak"
	if err := os.WriteFile(yamlPath, []byte("k: v\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bakPath, []byte("k: prev\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	wm.gcBakFiles()

	if _, err := os.Stat(bakPath); err != nil {
		t.Fatalf("fresh .bak with matching .yaml must be retained: %v", err)
	}
}
