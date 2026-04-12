package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"golang.org/x/sync/errgroup"

	"github.com/msageha/maestro_v2/internal/uds"
)

// setupDaemonForStartRuntime creates a daemon with the minimum fields required
// to call startRuntime() in tests. It wires a real fsnotify watcher and an
// errgroup so that fsnotifyLoop / tickerLoop can start without panicking.
//
// The UDS server is created with a short socket path to stay within macOS's
// 104-byte Unix socket path limit (t.TempDir() paths can be very long).
//
// Returns the daemon and the path to its Unix socket.
func setupDaemonForStartRuntime(t *testing.T) (*Daemon, string) {
	t.Helper()

	d := newTestDaemon(t)

	// Replace the server with one that uses a shorter socket path.
	// macOS limits Unix socket paths to 104 bytes; t.TempDir() paths can
	// easily exceed this when combined with the socket filename.
	sockDir, err := os.MkdirTemp("", "mds")
	if err != nil {
		t.Fatalf("create socket dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	sockPath := filepath.Join(sockDir, "d.sock")
	d.server = uds.NewServer(sockPath)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatalf("create watcher: %v", err)
	}
	t.Cleanup(func() { _ = watcher.Close() })
	d.watcher = watcher

	d.eg, d.egCtx = errgroup.WithContext(d.ctx)

	// Stop the server at test end so the acceptLoop goroutine does not leak.
	t.Cleanup(func() {
		if err := d.server.Stop(); err != nil {
			t.Logf("server.Stop: %v", err)
		}
	})

	return d, sockPath
}

// TestStartRuntime_ServerAvailableWithoutWaitingForReconcile verifies that the
// UDS server starts accepting ping requests immediately when startRuntime() is
// called, even if the startup reconcile is slow (e.g. many orphaned worktrees).
//
// Regression test for the bug where d.worktreeManager.Reconcile() was called
// synchronously inside initComponents() before server.Start(), causing
// waitDaemonReady to time out when git operations were slow.
//
// How to read a failure:
//   - If ping does not succeed within 1s, Reconcile is blocking server startup.
//   - Ensure that startRuntime() starts Reconcile in a goroutine AFTER
//     server.Start() returns.
func TestStartRuntime_ServerAvailableWithoutWaitingForReconcile(t *testing.T) {
	t.Parallel()

	d, sockPath := setupDaemonForStartRuntime(t)

	// Inject a slow reconcile that takes 3 seconds to complete.
	// In the real daemon this delay comes from "git worktree list/remove".
	// The test uses context cancellation so cleanup is instant.
	d.startupReconcileHook = func() {
		select {
		case <-time.After(3 * time.Second):
		case <-d.ctx.Done():
		}
	}

	// Run startRuntime() concurrently so we can poll the socket while it runs.
	// If Reconcile blocks server.Start(), startRuntime() will not return for 3s
	// and the socket will not be created — the ping below will fail.
	runtimeDone := make(chan error, 1)
	go func() { runtimeDone <- d.startRuntime() }()

	// The socket must become available well within 1 second (3× shorter than
	// the simulated reconcile delay). Poll every 20 ms.
	client := uds.NewClient(sockPath)
	client.SetTimeout(200 * time.Millisecond)

	deadline := time.Now().Add(1 * time.Second)
	var pingOK bool
	for time.Now().Before(deadline) {
		resp, err := client.SendCommand("ping", nil)
		if err == nil && resp.Success {
			pingOK = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if !pingOK {
		t.Error("server did not accept pings within 1s — " +
			"Reconcile must run in a goroutine after server.Start(), not before it")
	}

	// Cancel context to unblock the slow reconcile hook and stop loops.
	d.cancel()
	if err := <-runtimeDone; err != nil {
		t.Errorf("startRuntime error: %v", err)
	}
	_ = d.eg.Wait()
}

// TestStartRuntime_PingSucceedsWhileReconcileIsRunning confirms that the UDS
// server continues to serve ping requests while a concurrent Reconcile runs.
// This is a companion to the above test: it verifies that the goroutinized
// Reconcile does not block the server's accept loop.
func TestStartRuntime_PingSucceedsWhileReconcileIsRunning(t *testing.T) {
	t.Parallel()

	d, sockPath := setupDaemonForStartRuntime(t)

	// Block reconcile until the test signals it.
	reconcileStarted := make(chan struct{})
	reconcileUnblock := make(chan struct{})
	d.startupReconcileHook = func() {
		close(reconcileStarted)
		select {
		case <-reconcileUnblock:
		case <-d.ctx.Done():
		}
	}

	if err := d.startRuntime(); err != nil {
		t.Fatalf("startRuntime: %v", err)
	}

	// Wait until reconcile is actively running.
	select {
	case <-reconcileStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("reconcile hook did not start within 2s")
	}

	// Ping the server while reconcile is still holding the goroutine.
	client := uds.NewClient(sockPath)
	client.SetTimeout(time.Second)

	resp, err := client.SendCommand("ping", nil)
	if err != nil {
		t.Errorf("ping while reconcile running: %v", err)
	} else if !resp.Success {
		t.Errorf("ping returned failure while reconcile running: %v", resp.Error)
	}

	// Unblock reconcile and clean up.
	close(reconcileUnblock)
	d.cancel()
	_ = d.eg.Wait()
}
