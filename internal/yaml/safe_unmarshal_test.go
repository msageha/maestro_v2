package yaml

import (
	"fmt"
	"strings"
	"testing"
)

func TestSafeUnmarshal_ValidYAML(t *testing.T) {
	input := []byte("key: value\nlist:\n  - one\n  - two\n")
	var out map[string]any
	if err := SafeUnmarshal(input, &out); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out["key"] != "value" {
		t.Errorf("expected key=value, got %v", out["key"])
	}
}

func TestSafeUnmarshal_ValidAnchorsAndAliases(t *testing.T) {
	input := []byte("anchor: &a hello\nalias: *a\n")
	var out map[string]string
	if err := SafeUnmarshal(input, &out); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out["alias"] != "hello" {
		t.Errorf("expected alias=hello, got %v", out["alias"])
	}
}

func TestSafeUnmarshal_TooManyAnchors(t *testing.T) {
	var sb strings.Builder
	for i := 0; i <= MaxAnchors; i++ {
		fmt.Fprintf(&sb, "key%d: &anchor%d value%d\n", i, i, i)
	}

	var out map[string]string
	err := SafeUnmarshal([]byte(sb.String()), &out)
	if err == nil {
		t.Fatal("expected error for too many anchors")
	}
	if !strings.Contains(err.Error(), "anchor count") {
		t.Errorf("expected 'anchor count' in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("exceeds maximum of %d", MaxAnchors)) {
		t.Errorf("expected max limit in error, got: %v", err)
	}
}

func TestSafeUnmarshal_ExactlyMaxAnchorsAllowed(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < MaxAnchors; i++ {
		fmt.Fprintf(&sb, "key%d: &anchor%d value%d\n", i, i, i)
	}

	var out map[string]string
	if err := SafeUnmarshal([]byte(sb.String()), &out); err != nil {
		t.Fatalf("exactly %d anchors should be allowed, got error: %v", MaxAnchors, err)
	}
}

func TestSafeUnmarshal_AliasDepthExceeded(t *testing.T) {
	// Build a chain of aliases that exceeds MaxAliasDepth.
	// Each level references the previous anchor.
	var sb strings.Builder
	sb.WriteString("l0: &a0\n")
	sb.WriteString("  val: base\n")
	for i := 1; i <= MaxAliasDepth+1; i++ {
		fmt.Fprintf(&sb, "l%d: &a%d\n  ref: *a%d\n", i, i, i-1)
	}

	var out any
	err := SafeUnmarshal([]byte(sb.String()), &out)
	if err == nil {
		t.Fatal("expected error for alias depth exceeded")
	}
	if !strings.Contains(err.Error(), "alias depth") {
		t.Errorf("expected 'alias depth' in error, got: %v", err)
	}
}

func TestSafeUnmarshal_AliasDepthWithinLimit(t *testing.T) {
	// Build a chain of aliases exactly at MaxAliasDepth.
	var sb strings.Builder
	sb.WriteString("l0: &a0\n")
	sb.WriteString("  val: base\n")
	for i := 1; i <= MaxAliasDepth; i++ {
		fmt.Fprintf(&sb, "l%d: &a%d\n  ref: *a%d\n", i, i, i-1)
	}

	var out any
	if err := SafeUnmarshal([]byte(sb.String()), &out); err != nil {
		t.Fatalf("alias depth within limit should be allowed, got error: %v", err)
	}
}

func TestSafeUnmarshal_InvalidYAML(t *testing.T) {
	input := []byte(":\n  bad: [unclosed")
	var out any
	if err := SafeUnmarshal(input, &out); err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

func TestSafeUnmarshal_EmptyInput(t *testing.T) {
	var out any
	if err := SafeUnmarshal([]byte(""), &out); err != nil {
		t.Fatalf("empty input should not error: %v", err)
	}
}

func TestSafeUnmarshal_NoAnchorsOrAliases(t *testing.T) {
	input := []byte("a: 1\nb: 2\nc:\n  d: 3\n")
	var out map[string]any
	if err := SafeUnmarshal(input, &out); err != nil {
		t.Fatalf("YAML without anchors/aliases should work: %v", err)
	}
}
