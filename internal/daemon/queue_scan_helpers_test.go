package daemon

import (
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
)

func TestIsFenceStale(t *testing.T) {
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
			leaseExpiresAt:    strPtr("2025-01-01T00:20:00Z"),
			expectedEpoch:     3,
			expectedExpiresAt: expires,
			want:              true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isFenceStale(tt.status, tt.leaseEpoch, tt.leaseExpiresAt, tt.expectedEpoch, tt.expectedExpiresAt)
			if got != tt.want {
				t.Errorf("isFenceStale() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsMaxInProgressTimeout(t *testing.T) {
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
			got := isMaxInProgressTimeout(now, tt.timestamp, tt.maxMin)
			if got != tt.want {
				t.Errorf("isMaxInProgressTimeout() = %v, want %v", got, tt.want)
			}
		})
	}
}

func strPtr(s string) *string { return &s }
