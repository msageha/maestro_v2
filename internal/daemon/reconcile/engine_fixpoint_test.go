package reconcile

import (
	"testing"

	"github.com/msageha/maestro_v2/internal/testutil"
)

// convergingPattern produces repairs on the first N calls then converges.
type convergingPattern struct {
	maxPasses int
	callCount int
}

func (p *convergingPattern) Apply(*Run) Outcome {
	p.callCount++
	if p.callCount <= p.maxPasses {
		return Outcome{
			Repairs: []Repair{{Pattern: "converging", Detail: "pass"}},
		}
	}
	return Outcome{}
}

func TestEngine_Reconcile_FixpointConvergence(t *testing.T) {
	t.Parallel()
	deps := newTestDeps(t, testutil.SetupDir(t))

	// Pattern converges after 2 passes
	p := &convergingPattern{maxPasses: 2}
	engine := NewEngine(deps, p)
	repairs, _ := engine.Reconcile()

	// Should have repairs from 2 passes
	if len(repairs) != 2 {
		t.Errorf("expected 2 repairs (2 passes before convergence), got %d", len(repairs))
	}
	// Pattern should have been called 3 times (2 with repairs + 1 empty)
	if p.callCount != 3 {
		t.Errorf("expected 3 calls (converges on 3rd), got %d", p.callCount)
	}
}

func TestEngine_Reconcile_BoundedFixpoint(t *testing.T) {
	t.Parallel()
	deps := newTestDeps(t, testutil.SetupDir(t))

	// Pattern never converges (always returns repairs)
	neverConverges := &convergingPattern{maxPasses: 100}
	engine := NewEngine(deps, neverConverges)
	repairs, _ := engine.Reconcile()

	// Should be bounded by maxReconcilePasses (3)
	if len(repairs) != maxReconcilePasses {
		t.Errorf("expected %d repairs (bounded), got %d", maxReconcilePasses, len(repairs))
	}
}

func TestEngine_Reconcile_SinglePassConvergence(t *testing.T) {
	t.Parallel()
	deps := newTestDeps(t, testutil.SetupDir(t))

	// Pattern converges immediately (no repairs)
	noRepairs := &convergingPattern{maxPasses: 0}
	engine := NewEngine(deps, noRepairs)
	repairs, _ := engine.Reconcile()

	if len(repairs) != 0 {
		t.Errorf("expected 0 repairs, got %d", len(repairs))
	}
	// Should be called exactly once
	if noRepairs.callCount != 1 {
		t.Errorf("expected 1 call, got %d", noRepairs.callCount)
	}
}

func TestEngine_SetClock(t *testing.T) {
	t.Parallel()
	deps := newTestDeps(t, testutil.SetupDir(t))
	engine := NewEngine(deps)

	fc := &testutil.FakeClock{}
	engine.SetClock(fc)

	if engine.deps.Clock != fc {
		t.Error("expected clock to be replaced")
	}
}

func TestDeps_Validate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		modify  func(*Deps)
		wantErr bool
	}{
		{
			name:    "valid deps",
			modify:  func(*Deps) {},
			wantErr: false,
		},
		{
			name:    "missing MaestroDir",
			modify:  func(d *Deps) { d.MaestroDir = "" },
			wantErr: true,
		},
		{
			name:    "missing LockMap",
			modify:  func(d *Deps) { d.LockMap = nil },
			wantErr: true,
		},
		{
			name:    "missing DL",
			modify:  func(d *Deps) { d.DL = nil },
			wantErr: true,
		},
		{
			name:    "missing Clock",
			modify:  func(d *Deps) { d.Clock = nil },
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			deps := newTestDeps(t, testutil.SetupDir(t))
			tt.modify(&deps)
			err := deps.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}
