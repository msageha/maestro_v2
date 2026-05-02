package daemon

import "testing"

// TestShouldRunAfterVerificationSync pins the sync-path AfterVerification
// guard. The two downstream side effects (maybeAutoRecoverAfterResolution
// + dispatchAdvisoryReview) MUST share a single gate so that adding a
// third side effect later cannot accidentally invoke one path and skip
// the other.
func TestShouldRunAfterVerificationSync(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name               string
		duplicate          bool
		verifyWillRunAsync bool
		want               bool
	}{
		{"fresh sync write — both side effects must run", false, false, true},
		{"duplicate — both side effects must skip", true, false, false},
		{"async verify — sync path defers to async completion", false, true, false},
		{"duplicate AND async — definitely skip", true, true, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := shouldRunAfterVerificationSync(resultPostFinalizeInput{
				duplicate:          tc.duplicate,
				verifyWillRunAsync: tc.verifyWillRunAsync,
			})
			if got != tc.want {
				t.Errorf("shouldRunAfterVerificationSync(duplicate=%v, async=%v) = %v, want %v",
					tc.duplicate, tc.verifyWillRunAsync, got, tc.want)
			}
		})
	}
}
