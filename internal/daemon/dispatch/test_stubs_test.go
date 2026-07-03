package dispatch

import "github.com/msageha/maestro_v2/internal/model"

// stubResolver is a no-op WorktreeResolver fixture used by dispatch tests.
// Each method returns the zero value; tests embed it and override only the
// methods they care about. Kept in a shared *_test.go file so multiple test
// files can reuse the embedding without duplicating the surface.
type stubResolver struct{}

func (stubResolver) GetWorkerPath(string, string) (string, error) { return "", nil }
func (stubResolver) EnsureWorkerWorktree(string, string) error    { return nil }
func (stubResolver) EnsureCandidateWorktree(string, string) (string, string, error) {
	return "", "", nil
}
func (stubResolver) GetIntegrationPath(string) (string, error)      { return "", nil }
func (stubResolver) EnsureIntegrationBranchCheckedOut(string) error { return nil }
func (stubResolver) GetIntegrationStatus(string) (model.IntegrationStatus, error) {
	return model.IntegrationStatusCreated, nil
}
func (stubResolver) RefreshWorkerWorktreeToIntegrationHead(string, string) error {
	return nil
}
