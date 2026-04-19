package daemon

import (
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/ptr"
)

func TestIsFenceStale(t *testing.T) {
	t.Parallel()
	expires := "2025-01-01T00:10:00Z"
	tests := []struct {
		name              string
		status            model.Status
		leaseEpoch        int
		leaseExpiresAt    *string
		expectedEpoch     int
		expectedExpiresAt string
		want              bool
	}{
		{
			name:              "matching_fields_not_stale",
			status:            model.StatusInProgress,
			leaseEpoch:        3,
			leaseExpiresAt:    &expires,
			expectedEpoch:     3,
			expectedExpiresAt: expires,
			want:              false,
		},
		{
			name:              "epoch_mismatch",
			status:            model.StatusInProgress,
			leaseEpoch:        4,
			leaseExpiresAt:    &expires,
			expectedEpoch:     3,
			expectedExpiresAt: expires,
			want:              true,
		},
		{
			name:              "status_not_in_progress",
			status:            model.StatusPending,
			leaseEpoch:        3,
			leaseExpiresAt:    &expires,
			expectedEpoch:     3,
			expectedExpiresAt: expires,
			want:              true,
		},
		{
			name:              "nil_lease_expires_at",
			status:            model.StatusInProgress,
			leaseEpoch:        3,
			leaseExpiresAt:    nil,
			expectedEpoch:     3,
			expectedExpiresAt: expires,
			want:              true,
		},
		{
			name:              "expires_at_mismatch",
			status:            model.StatusInProgress,
			leaseEpoch:        3,
			leaseExpiresAt:    ptr.String("2025-01-01T00:20:00Z"),
			expectedEpoch:     3,
			expectedExpiresAt: expires,
			want:              true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := isFenceStale(tt.status, tt.leaseEpoch, tt.leaseExpiresAt, tt.expectedEpoch, tt.expectedExpiresAt)
			if got != tt.want {
				t.Errorf("isFenceStale() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCheckResultFencing(t *testing.T) {
	t.Parallel()
	expires := "2025-01-01T00:10:00Z"
	tests := []struct {
		name              string
		status            model.Status
		leaseEpoch        int
		leaseExpiresAt    *string
		expectedEpoch     int
		expectedExpiresAt string
		wantStale         bool
		wantReason        string
	}{
		{
			name:              "all_match_valid",
			status:            model.StatusInProgress,
			leaseEpoch:        3,
			leaseExpiresAt:    &expires,
			expectedEpoch:     3,
			expectedExpiresAt: expires,
			wantStale:         false,
			wantReason:        "",
		},
		{
			name:              "status_changed",
			status:            model.StatusPending,
			leaseEpoch:        3,
			leaseExpiresAt:    &expires,
			expectedEpoch:     3,
			expectedExpiresAt: expires,
			wantStale:         true,
			wantReason:        "status",
		},
		{
			name:              "epoch_mismatch",
			status:            model.StatusInProgress,
			leaseEpoch:        4,
			leaseExpiresAt:    &expires,
			expectedEpoch:     3,
			expectedExpiresAt: expires,
			wantStale:         true,
			wantReason:        "epoch",
		},
		{
			name:              "nil_lease_expires_at",
			status:            model.StatusInProgress,
			leaseEpoch:        3,
			leaseExpiresAt:    nil,
			expectedEpoch:     3,
			expectedExpiresAt: expires,
			wantStale:         true,
			wantReason:        "expiry",
		},
		{
			name:              "expires_at_mismatch",
			status:            model.StatusInProgress,
			leaseEpoch:        3,
			leaseExpiresAt:    ptr.String("2025-01-01T00:20:00Z"),
			expectedEpoch:     3,
			expectedExpiresAt: expires,
			wantStale:         true,
			wantReason:        "expiry",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rej := checkResultFencing(tt.status, tt.leaseEpoch, tt.leaseExpiresAt, tt.expectedEpoch, tt.expectedExpiresAt)
			if rej.Stale() != tt.wantStale {
				t.Errorf("checkResultFencing().Stale() = %v, want %v", rej.Stale(), tt.wantStale)
			}
			if rej.Reason != tt.wantReason {
				t.Errorf("checkResultFencing().Reason = %q, want %q", rej.Reason, tt.wantReason)
			}
		})
	}
}

func TestCheckResultFencingString(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		rej  FenceRejection
		want string
	}{
		{name: "valid", rej: FenceRejection{}, want: "valid"},
		{name: "status", rej: FenceRejection{Reason: "status"}, want: "status"},
		{name: "epoch", rej: FenceRejection{Reason: "epoch"}, want: "epoch"},
		{name: "expiry", rej: FenceRejection{Reason: "expiry"}, want: "expiry"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.rej.String(); got != tt.want {
				t.Errorf("FenceRejection.String() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIsEpochStale(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		leaseEpoch    int
		expectedEpoch int
		want          bool
	}{
		{name: "matching_epochs", leaseEpoch: 5, expectedEpoch: 5, want: false},
		{name: "lease_ahead", leaseEpoch: 6, expectedEpoch: 5, want: true},
		{name: "lease_behind", leaseEpoch: 4, expectedEpoch: 5, want: true},
		{name: "zero_epochs", leaseEpoch: 0, expectedEpoch: 0, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isEpochStale(tt.leaseEpoch, tt.expectedEpoch); got != tt.want {
				t.Errorf("isEpochStale(%d, %d) = %v, want %v", tt.leaseEpoch, tt.expectedEpoch, got, tt.want)
			}
		})
	}
}

func TestCheckResultFencingConsistentWithIsFenceStale(t *testing.T) {
	t.Parallel()
	expires := "2025-01-01T00:10:00Z"
	otherExpires := "2025-01-01T00:20:00Z"
	cases := []struct {
		status            model.Status
		leaseEpoch        int
		leaseExpiresAt    *string
		expectedEpoch     int
		expectedExpiresAt string
	}{
		{model.StatusInProgress, 3, &expires, 3, expires},
		{model.StatusPending, 3, &expires, 3, expires},
		{model.StatusInProgress, 4, &expires, 3, expires},
		{model.StatusInProgress, 3, nil, 3, expires},
		{model.StatusInProgress, 3, &otherExpires, 3, expires},
		{model.StatusCompleted, 1, nil, 2, expires},
	}
	for _, c := range cases {
		old := isFenceStale(c.status, c.leaseEpoch, c.leaseExpiresAt, c.expectedEpoch, c.expectedExpiresAt)
		rej := checkResultFencing(c.status, c.leaseEpoch, c.leaseExpiresAt, c.expectedEpoch, c.expectedExpiresAt)
		if old != rej.Stale() {
			t.Errorf("isFenceStale and checkResultFencing disagree for status=%s epoch=%d/%d: isFenceStale=%v Stale=%v",
				c.status, c.leaseEpoch, c.expectedEpoch, old, rej.Stale())
		}
	}
}

func TestIsMaxInProgressTimeout(t *testing.T) {
	t.Parallel()
	now := time.Date(2025, 1, 1, 1, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		timestamp string
		maxMin    int
		want      bool
	}{
		{
			name:      "within_limit",
			timestamp: now.Add(-30 * time.Minute).Format(time.RFC3339),
			maxMin:    60,
			want:      false,
		},
		{
			name:      "at_boundary_exact",
			timestamp: now.Add(-60 * time.Minute).Format(time.RFC3339),
			maxMin:    60,
			want:      true,
		},
		{
			name:      "exceeded",
			timestamp: now.Add(-90 * time.Minute).Format(time.RFC3339),
			maxMin:    60,
			want:      true,
		},
		{
			name:      "malformed_timestamp_returns_false",
			timestamp: "not-a-timestamp",
			maxMin:    60,
			want:      false,
		},
		{
			name:      "empty_timestamp_returns_false",
			timestamp: "",
			maxMin:    60,
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := isMaxInProgressTimeout(now, tt.timestamp, tt.maxMin, nil)
			if got != tt.want {
				t.Errorf("isMaxInProgressTimeout() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMaxGraceLeaseDuration(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name             string
		maxInProgressMin int
		scanIntervalSec  int
		want             time.Duration
	}{
		{
			name:             "normal_case_60min_10sec",
			maxInProgressMin: 60,
			scanIntervalSec:  10,
			want:             20 * time.Minute, // 60/3 = 20min > 30s floor
		},
		{
			name:             "large_max_in_progress",
			maxInProgressMin: 180,
			scanIntervalSec:  10,
			want:             60 * time.Minute, // 180/3 = 60min > 30s floor
		},
		{
			name:             "small_max_in_progress_hits_floor",
			maxInProgressMin: 0,
			scanIntervalSec:  10,
			want:             30 * time.Second, // 0/3 = 0min < 30s floor → 30s
		},
		{
			name:             "very_small_max_hits_floor",
			maxInProgressMin: 1,
			scanIntervalSec:  20,
			want:             60 * time.Second, // 1/3 = 0min < 60s floor → 60s
		},
		{
			name:             "grace_equals_floor",
			maxInProgressMin: 3,
			scanIntervalSec:  20,
			want:             60 * time.Second, // 3/3 = 1min = 60s = floor → equal returns grace
		},
		{
			name:             "zero_scan_interval",
			maxInProgressMin: 60,
			scanIntervalSec:  0,
			want:             20 * time.Minute, // 60/3 = 20min > 0s floor
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := maxGraceLeaseDuration(tt.maxInProgressMin, tt.scanIntervalSec)
			if got != tt.want {
				t.Errorf("maxGraceLeaseDuration(%d, %d) = %v, want %v",
					tt.maxInProgressMin, tt.scanIntervalSec, got, tt.want)
			}
		})
	}
}

func TestIsGraceLeaseExceeded(t *testing.T) {
	t.Parallel()
	now := time.Date(2025, 1, 1, 1, 0, 0, 0, time.UTC)
	dispatchLease := 5 * time.Minute
	graceLimit := 20 * time.Minute

	tests := []struct {
		name          string
		updatedAt     string
		dispatchLease time.Duration
		graceLimit    time.Duration
		want          bool
	}{
		{
			name:          "within_grace_limit",
			updatedAt:     now.Add(-10 * time.Minute).Format(time.RFC3339), // graceStart = now-10m+5m = now-5m, elapsed=5m < 20m
			dispatchLease: dispatchLease,
			graceLimit:    graceLimit,
			want:          false,
		},
		{
			name:          "at_grace_limit_boundary",
			updatedAt:     now.Add(-25 * time.Minute).Format(time.RFC3339), // graceStart = now-25m+5m = now-20m, elapsed=20m >= 20m
			dispatchLease: dispatchLease,
			graceLimit:    graceLimit,
			want:          true,
		},
		{
			name:          "exceeded_grace_limit",
			updatedAt:     now.Add(-30 * time.Minute).Format(time.RFC3339), // graceStart = now-30m+5m = now-25m, elapsed=25m >= 20m
			dispatchLease: dispatchLease,
			graceLimit:    graceLimit,
			want:          true,
		},
		{
			name:          "dispatch_lease_not_yet_expired",
			updatedAt:     now.Add(-3 * time.Minute).Format(time.RFC3339), // graceStart = now-3m+5m = now+2m, elapsed=-2m < 20m
			dispatchLease: dispatchLease,
			graceLimit:    graceLimit,
			want:          false,
		},
		{
			name:          "malformed_timestamp_returns_false",
			updatedAt:     "not-a-timestamp",
			dispatchLease: dispatchLease,
			graceLimit:    graceLimit,
			want:          false,
		},
		{
			name:          "empty_timestamp_returns_false",
			updatedAt:     "",
			dispatchLease: dispatchLease,
			graceLimit:    graceLimit,
			want:          false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := isGraceLeaseExceeded(now, tt.updatedAt, tt.dispatchLease, tt.graceLimit, nil)
			if got != tt.want {
				t.Errorf("isGraceLeaseExceeded() = %v, want %v", got, tt.want)
			}
		})
	}
}

