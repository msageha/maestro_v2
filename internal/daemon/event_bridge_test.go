package daemon

import (
	"testing"
)

func TestExtractStringFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		data    map[string]interface{}
		keys    []string
		wantOK  bool
		wantLen int
	}{
		{
			name:    "all_keys_present",
			data:    map[string]interface{}{"a": "v1", "b": "v2", "c": "v3"},
			keys:    []string{"a", "b", "c"},
			wantOK:  true,
			wantLen: 3,
		},
		{
			name:   "missing_key",
			data:   map[string]interface{}{"a": "v1"},
			keys:   []string{"a", "b"},
			wantOK: false,
		},
		{
			name:   "wrong_type",
			data:   map[string]interface{}{"a": "v1", "b": 42},
			keys:   []string{"a", "b"},
			wantOK: false,
		},
		{
			name:   "nil_value",
			data:   map[string]interface{}{"a": "v1", "b": nil},
			keys:   []string{"a", "b"},
			wantOK: false,
		},
		{
			name:    "empty_keys",
			data:    map[string]interface{}{"a": "v1"},
			keys:    []string{},
			wantOK:  true,
			wantLen: 0,
		},
		{
			name:    "empty_data_empty_keys",
			data:    map[string]interface{}{},
			keys:    []string{},
			wantOK:  true,
			wantLen: 0,
		},
		{
			name:   "empty_data_with_keys",
			data:   map[string]interface{}{},
			keys:   []string{"a"},
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			vals, ok := extractStringFields(tt.data, tt.keys)
			if ok != tt.wantOK {
				t.Errorf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && len(vals) != tt.wantLen {
				t.Errorf("len(vals) = %d, want %d", len(vals), tt.wantLen)
			}
		})
	}
}

func TestExtractStringFields_ValueOrder(t *testing.T) {
	t.Parallel()
	data := map[string]interface{}{
		"task_id":    "t1",
		"command_id": "c1",
		"worker_id":  "w1",
	}
	vals, ok := extractStringFields(data, []string{"task_id", "command_id", "worker_id"})
	if !ok {
		t.Fatal("expected ok=true")
	}
	if vals[0] != "t1" || vals[1] != "c1" || vals[2] != "w1" {
		t.Errorf("values = %v, want [t1 c1 w1]", vals)
	}
}

func TestUnsubscribeAll(t *testing.T) {
	t.Parallel()

	callCount := 0
	eb := &EventBridge{
		eventUnsubscribers: []func(){
			func() { callCount++ },
			nil, // nil should be skipped
			func() { callCount++ },
		},
	}

	eb.unsubscribeAll()

	if callCount != 2 {
		t.Errorf("expected 2 unsubscribe calls, got %d", callCount)
	}
	if eb.eventUnsubscribers != nil {
		t.Errorf("expected eventUnsubscribers to be nil after unsubscribeAll")
	}
}

func TestUnsubscribeAll_Empty(t *testing.T) {
	t.Parallel()
	eb := &EventBridge{}
	// Must not panic on nil slice.
	eb.unsubscribeAll()
	if eb.eventUnsubscribers != nil {
		t.Errorf("expected nil after unsubscribe on empty bridge")
	}
}

func TestMaxDrainGoroutinesConstant(t *testing.T) {
	if maxDrainGoroutines != 10 {
		t.Errorf("maxDrainGoroutines = %d, want 10", maxDrainGoroutines)
	}
}
