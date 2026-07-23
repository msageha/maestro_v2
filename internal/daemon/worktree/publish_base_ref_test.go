package worktree

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/testutil"
)

// resolveBaseRef must map an existing local branch to refs/heads/<branch> and
// anything else (a detached-HEAD commit SHA, or a name with no branch) to HEAD,
// so publish advances the detached checkout in place instead of creating a
// spurious refs/heads/<sha>.
func TestResolveBaseRef(t *testing.T) {
	projectRoot := testutil.InitTestGitRepo(t) // on branch "main" with one commit
	wm := newTestWorktreeManager(t, projectRoot)

	headSHA := gitHeadSHA(t, projectRoot)

	cases := []struct {
		name       string
		baseBranch string
		want       string
	}{
		{"existing branch", "main", "refs/heads/main"},
		{"detached commit sha", headSHA, "HEAD"},
		{"nonexistent branch", "no-such-branch", "HEAD"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := wm.resolveBaseRef(tc.baseBranch); got != tc.want {
				t.Fatalf("resolveBaseRef(%q) = %q, want %q", tc.baseBranch, got, tc.want)
			}
		})
	}
}

func gitHeadSHA(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "rev-parse", "HEAD")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}
