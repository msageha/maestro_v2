package admission

import (
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/stretchr/testify/assert"
)

func defaultCfg() model.AdmissionControl {
	return model.AdmissionControl{
		MaxConcurrentVerify: 2,
		MaxConcurrentRepair: 1,
	}
}

func TestTryAcquire_RespectsLimits(t *testing.T) {
	t.Parallel()
	c := NewController(defaultCfg())

	// Can acquire up to the limit (verify=2).
	assert.True(t, c.TryAcquire(OpVerify), "first verify slot should succeed")
	assert.True(t, c.TryAcquire(OpVerify), "second verify slot should succeed")

	// Third acquisition must fail.
	assert.False(t, c.TryAcquire(OpVerify), "third verify slot should be denied")
}

func TestRelease_AllowsReAcquire(t *testing.T) {
	t.Parallel()
	c := NewController(defaultCfg())

	assert.True(t, c.TryAcquire(OpRepair))
	assert.False(t, c.TryAcquire(OpRepair), "repair limit is 1")

	c.Release(OpRepair)
	assert.True(t, c.TryAcquire(OpRepair), "should succeed after release")
}

func TestRelease_DoesNotGoBelowZero(t *testing.T) {
	t.Parallel()
	c := NewController(defaultCfg())

	// Release without prior acquire should not panic or go negative.
	c.Release(OpVerify)
	assert.Equal(t, 0, c.ActiveCount(OpVerify))
}

func TestOpType_Independence(t *testing.T) {
	t.Parallel()
	c := NewController(defaultCfg())

	// Fill verify slots.
	assert.True(t, c.TryAcquire(OpVerify))
	assert.True(t, c.TryAcquire(OpVerify))
	assert.False(t, c.TryAcquire(OpVerify))

	// Repair should still be available.
	assert.True(t, c.TryAcquire(OpRepair), "repair should not be blocked by full verify")
}

func TestSnapshot_Accuracy(t *testing.T) {
	t.Parallel()
	c := NewController(defaultCfg())

	c.TryAcquire(OpVerify)
	c.TryAcquire(OpVerify)
	c.TryAcquire(OpRepair)

	snap := c.Snapshot()
	assert.Equal(t, 2, snap[OpVerify])
	assert.Equal(t, 1, snap[OpRepair])

	// Mutating the snapshot should not affect the controller.
	snap[OpVerify] = 99
	assert.Equal(t, 2, c.ActiveCount(OpVerify))
}

// TestClassifyTask_PurposeIgnored locks in the deterministic-classification
// invariant: even when Purpose contains verify/repair keywords, an unset
// OperationType MUST classify as OpUnknown. This guards against the pre-2026
// Purpose-substring behaviour that misclassified normal user tasks (e.g.
// "Repair broken auth flow") into the admission-controlled bucket.
func TestClassifyTask_PurposeIgnored(t *testing.T) {
	t.Parallel()
	c := NewController(defaultCfg())

	tests := []struct {
		name    string
		purpose string
	}{
		{name: "verify lowercase", purpose: "verify implementation"},
		{name: "verify uppercase", purpose: "VERIFY results"},
		{name: "verification", purpose: "run verification suite"},
		{name: "repair", purpose: "repair broken tests"},
		{name: "unknown generic", purpose: "implement feature X"},
		{name: "empty purpose", purpose: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			task := &model.Task{Purpose: tt.purpose}
			assert.Equal(t, OpUnknown, c.ClassifyTask(task),
				"empty OperationType must yield OpUnknown regardless of Purpose")
		})
	}
}

func TestClassifyTask_OperationTypeTakesPrecedence(t *testing.T) {
	t.Parallel()
	c := NewController(defaultCfg())

	tests := []struct {
		name          string
		operationType string
		purpose       string
		want          OpType
	}{
		{
			name:          "structured field overrides purpose",
			operationType: model.OperationTypeRepair,
			purpose:       "verify implementation", // would otherwise classify as verify
			want:          OpRepair,
		},
		{
			name:          "verify structured field",
			operationType: model.OperationTypeVerify,
			purpose:       "implement feature",
			want:          OpVerify,
		},
		{
			name:          "empty structured field classifies as unknown regardless of purpose",
			operationType: "",
			purpose:       "repair broken tests",
			want:          OpUnknown,
		},
		{
			name:          "unknown structured field classifies as unknown",
			operationType: "bogus",
			purpose:       "v2 deploy",
			want:          OpUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			task := &model.Task{
				OperationType: tt.operationType,
				Purpose:       tt.purpose,
			}
			got := c.ClassifyTask(task)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestOpUnknown_AlwaysPassesTryAcquire(t *testing.T) {
	t.Parallel()
	c := NewController(defaultCfg())

	// OpUnknown should always succeed regardless of how many times we call it.
	for i := 0; i < 100; i++ {
		assert.True(t, c.TryAcquire(OpUnknown), "OpUnknown should always succeed (iteration %d)", i)
	}
}

func TestReset_ClearsSlots(t *testing.T) {
	t.Parallel()
	c := NewController(defaultCfg())

	c.TryAcquire(OpVerify)
	c.TryAcquire(OpVerify)
	c.TryAcquire(OpRepair)

	c.Reset()

	assert.Equal(t, 0, c.ActiveCount(OpVerify))
	assert.Equal(t, 0, c.ActiveCount(OpRepair))

	// After reset we should be able to acquire again.
	assert.True(t, c.TryAcquire(OpVerify))
	assert.True(t, c.TryAcquire(OpRepair))
}

func TestRecordInFlight(t *testing.T) {
	t.Parallel()
	c := NewController(defaultCfg())

	// Pre-populate a slot that should be cleared by RecordInFlight.
	c.TryAcquire(OpRepair)

	// Tasks must declare OperationType structurally; Purpose is only a
	// human-readable hint and MUST NOT influence admission classification.
	tasks := []*model.Task{
		{OperationType: model.OperationTypeVerify, Purpose: "verify unit tests"},
		{OperationType: model.OperationTypeVerify, Purpose: "Verification of integration"},
		{OperationType: model.OperationTypeRepair, Purpose: "repair linting errors"},
		{Purpose: "implement new feature"}, // OperationType empty → OpUnknown, not counted
	}

	c.RecordInFlight(tasks)

	assert.Equal(t, 2, c.ActiveCount(OpVerify))
	assert.Equal(t, 1, c.ActiveCount(OpRepair), "repair active count should reflect only the in-flight task after reset")
}

func TestRecordInFlight_PreservesBackgroundSlots(t *testing.T) {
	t.Parallel()
	c := NewController(defaultCfg())

	assert.True(t, c.TryAcquireBackground(OpVerify))
	c.RecordInFlight([]*model.Task{
		{OperationType: model.OperationTypeVerify},
		{OperationType: model.OperationTypeRepair},
	})

	assert.Equal(t, 2, c.ActiveCount(OpVerify), "background verify + queue in-flight verify should both count")
	assert.Equal(t, 1, c.ActiveCount(OpRepair))
	assert.False(t, c.TryAcquire(OpVerify), "verify limit should include background work")

	c.ReleaseBackground(OpVerify)
	assert.Equal(t, 1, c.ActiveCount(OpVerify), "releasing background slot should leave queue in-flight slot")
	assert.True(t, c.TryAcquire(OpVerify), "one verify slot should be available after background release")
}

func TestOpType_String(t *testing.T) {
	t.Parallel()
	tests := []struct {
		op   OpType
		want string
	}{
		{OpVerify, "verify"},
		{OpRepair, "repair"},
		{OpUnknown, "unknown"},
		{OpType(99), "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, tt.op.String())
		})
	}
}
