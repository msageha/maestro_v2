package daemon

import (
	"testing"

	"github.com/msageha/maestro_v2/internal/testutil"
)

// fixTestDirPerms delegates to testutil.FixDirPerms.
func fixTestDirPerms(t *testing.T, root string) {
	t.Helper()
	testutil.FixDirPerms(t, root)
}
