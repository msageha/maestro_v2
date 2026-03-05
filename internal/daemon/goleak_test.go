package daemon

import (
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		// fsnotify spawns platform-specific background goroutines for
		// watching directories. These vary by OS backend (kqueue on macOS,
		// inotify on Linux, ReadDirectoryChangesW on Windows).
		goleak.IgnoreTopFunction("github.com/fsnotify/fsnotify.(*Watcher).readEvents"),
		goleak.IgnoreTopFunction("github.com/fsnotify/fsnotify.(*kqueue).read"),
		goleak.IgnoreTopFunction("github.com/fsnotify/fsnotify.(*inotify).readEvents"),
	)
}
