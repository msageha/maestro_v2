package ptr

import (
	"math"
	"testing"
)

func TestBool(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   bool
	}{
		{name: "true", in: true},
		{name: "false", in: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Bool(tt.in)
			if got == nil {
				t.Fatal("Bool() returned nil")
			}
			if *got != tt.in {
				t.Errorf("*Bool(%v) = %v, want %v", tt.in, *got, tt.in)
			}
		})
	}
}

func TestString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
	}{
		{name: "empty string", in: ""},
		{name: "normal string", in: "hello"},
		{name: "multibyte string", in: "日本語テスト"},
		{name: "string with spaces", in: "hello world"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := String(tt.in)
			if got == nil {
				t.Fatal("String() returned nil")
			}
			if *got != tt.in {
				t.Errorf("*String(%q) = %q, want %q", tt.in, *got, tt.in)
			}
		})
	}
}

func TestInt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   int
	}{
		{name: "zero", in: 0},
		{name: "positive", in: 42},
		{name: "negative", in: -1},
		{name: "max int", in: math.MaxInt},
		{name: "min int", in: math.MinInt},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Int(tt.in)
			if got == nil {
				t.Fatal("Int() returned nil")
			}
			if *got != tt.in {
				t.Errorf("*Int(%d) = %d, want %d", tt.in, *got, tt.in)
			}
		})
	}
}

func TestFloat64(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      float64
		wantNaN bool
	}{
		{name: "zero", in: 0.0},
		{name: "positive", in: 3.14},
		{name: "negative", in: -2.71},
		{name: "positive infinity", in: math.Inf(1)},
		{name: "negative infinity", in: math.Inf(-1)},
		{name: "NaN", in: math.NaN(), wantNaN: true},
		{name: "max float64", in: math.MaxFloat64},
		{name: "smallest positive", in: math.SmallestNonzeroFloat64},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Float64(tt.in)
			if got == nil {
				t.Fatal("Float64() returned nil")
			}
			if tt.wantNaN {
				if !math.IsNaN(*got) {
					t.Errorf("*Float64(NaN) = %v, want NaN", *got)
				}
			} else if *got != tt.in {
				t.Errorf("*Float64(%v) = %v, want %v", tt.in, *got, tt.in)
			}
		})
	}
}

// TestPointerIndependence verifies that each call returns a distinct pointer.
func TestPointerIndependence(t *testing.T) {
	t.Parallel()

	p1 := Int(42)
	p2 := Int(42)
	if p1 == p2 {
		t.Error("Int(42) returned the same pointer twice")
	}

	s1 := String("x")
	s2 := String("x")
	if s1 == s2 {
		t.Error("String(\"x\") returned the same pointer twice")
	}
}
