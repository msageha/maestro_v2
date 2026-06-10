package tmux

import (
	"testing"
	"time"
)

func TestPasteDelayFor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		n    int
		want time.Duration
	}{
		{"small message keeps base delay", 200, 500 * time.Millisecond},
		{"sub-KB stays at base", 1023, 500 * time.Millisecond},
		{"8KB adds scaled headroom", 8 * 1024, 500*time.Millisecond + 8*15*time.Millisecond},
		{"128KB envelope scales below cap", 128 * 1024, 500*time.Millisecond + 128*15*time.Millisecond},
		{"256KB hits the cap", 256 * 1024, 3 * time.Second},
		{"1MB max message stays capped", 1 << 20, 3 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := pasteDelayFor(tt.n); got != tt.want {
				t.Errorf("pasteDelayFor(%d) = %v, want %v", tt.n, got, tt.want)
			}
		})
	}
}
