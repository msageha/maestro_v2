package model

import (
	"testing"
)

func TestGenerateID(t *testing.T) {
	types := []IDType{IDTypeCommand, IDTypeTask, IDTypePhase, IDTypeNotification, IDTypeResult}
	prefixes := []string{"cmd", "task", "phase", "ntf", "res"}

	for i, idType := range types {
		t.Run(string(idType), func(t *testing.T) {
			id, err := GenerateID(idType)
			if err != nil {
				t.Fatalf("GenerateID(%s) returned error: %v", idType, err)
			}
			if !ValidateID(id) {
				t.Errorf("generated ID %q does not match regex", id)
			}
			if id[:len(prefixes[i])] != prefixes[i] {
				t.Errorf("expected prefix %q, got %q", prefixes[i], id[:len(prefixes[i])])
			}
		})
	}
}

func TestGenerateID_InvalidType(t *testing.T) {
	_, err := GenerateID("invalid")
	if err == nil {
		t.Error("expected error for invalid ID type")
	}
}

func TestGenerateID_Uniqueness(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id, err := GenerateID(IDTypeTask)
		if err != nil {
			t.Fatalf("GenerateID returned error: %v", err)
		}
		if seen[id] {
			t.Fatalf("duplicate ID generated: %s", id)
		}
		seen[id] = true
	}
}

func TestValidateID(t *testing.T) {
	tests := []struct {
		name  string
		id    string
		valid bool
	}{
		{"valid command", "cmd_1771722000_a3f2b7c1", true},
		{"valid task", "task_1771722060_b7c1d4e9", true},
		{"valid phase", "phase_1771722000_c3d4e5f6", true},
		{"valid notification", "ntf_1771722600_d4e9f0a2", true},
		{"valid result", "res_1771722300_e5f0c3d8", true},
		{"invalid prefix", "xxx_1771722000_a3f2b7c1", false},
		{"short timestamp", "cmd_177172200_a3f2b7c1", false},
		{"long timestamp", "cmd_17717220001_a3f2b7c1", false},
		{"uppercase hex", "cmd_1771722000_A3F2B7C1", false},
		{"short hex", "cmd_1771722000_a3f2b7c", false},
		{"long hex", "cmd_1771722000_a3f2b7c10", false},
		{"empty", "", false},
		{"no separators", "cmd1771722000a3f2b7c1", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ValidateID(tt.id); got != tt.valid {
				t.Errorf("ValidateID(%q) = %v, want %v", tt.id, got, tt.valid)
			}
		})
	}
}

func TestParseIDType(t *testing.T) {
	tests := []struct {
		id       string
		expected IDType
	}{
		{"cmd_1771722000_a3f2b7c1", IDTypeCommand},
		{"task_1771722060_b7c1d4e9", IDTypeTask},
		{"phase_1771722000_c3d4e5f6", IDTypePhase},
		{"ntf_1771722600_d4e9f0a2", IDTypeNotification},
		{"res_1771722300_e5f0c3d8", IDTypeResult},
	}
	for _, tt := range tests {
		t.Run(string(tt.expected), func(t *testing.T) {
			got, err := ParseIDType(tt.id)
			if err != nil {
				t.Fatalf("ParseIDType(%q) returned error: %v", tt.id, err)
			}
			if got != tt.expected {
				t.Errorf("ParseIDType(%q) = %q, want %q", tt.id, got, tt.expected)
			}
		})
	}
}

func TestParseIDType_Invalid(t *testing.T) {
	_, err := ParseIDType("invalid")
	if err == nil {
		t.Error("expected error for invalid ID")
	}
}

func TestParseIDTimestamp(t *testing.T) {
	ts, err := ParseIDTimestamp("cmd_1771722000_a3f2b7c1")
	if err != nil {
		t.Fatalf("ParseIDTimestamp returned error: %v", err)
	}
	if ts.Unix() != 1771722000 {
		t.Errorf("expected timestamp 1771722000, got %d", ts.Unix())
	}
}
