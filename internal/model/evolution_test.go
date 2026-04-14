package model

import "testing"

func TestValidateMutationStrategy(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"diff", true},
		{"full", true},
		{"cross", true},
		{"unknown", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := validateMutationStrategy(tt.input); got != tt.want {
				t.Errorf("validateMutationStrategy(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}
