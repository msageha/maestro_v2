package model

import (
	"testing"

	"github.com/msageha/maestro_v2/internal/ptr"
)

func testGroup() *CandidateGroup {
	return &CandidateGroup{
		Status:          ABGroupRacing,
		CanonicalTaskID: "task_A",
		Candidates: []ABCandidate{
			{TaskID: "task_A", WorkerID: "worker1", Model: "opus", BloomLevel: 5},
			{TaskID: "task_B", WorkerID: "worker3", Model: "codex", BloomLevel: 5},
		},
	}
}

func TestABGroupStatus_IsUnresolved(t *testing.T) {
	cases := map[ABGroupStatus]bool{
		ABGroupRacing:    true,
		ABGroupSelecting: true,
		ABGroupResolved:  false,
		ABGroupDegraded:  false,
	}
	for status, want := range cases {
		if got := status.IsUnresolved(); got != want {
			t.Errorf("IsUnresolved(%s) = %v, want %v", status, got, want)
		}
	}
}

func TestCandidateGroup_Lookups(t *testing.T) {
	g := testGroup()

	if c := g.CandidateByTask("task_B"); c == nil || c.WorkerID != "worker3" {
		t.Errorf("CandidateByTask(task_B) = %+v, want worker3 entry", c)
	}
	if c := g.CandidateByTask("task_X"); c != nil {
		t.Errorf("CandidateByTask(unknown) = %+v, want nil", c)
	}
	if c := g.OtherCandidate("task_A"); c == nil || c.TaskID != "task_B" {
		t.Errorf("OtherCandidate(task_A) = %+v, want task_B", c)
	}

	var nilGroup *CandidateGroup
	if nilGroup.CandidateByTask("x") != nil || nilGroup.OtherCandidate("x") != nil {
		t.Error("nil group lookups must return nil")
	}
}

func TestCommandState_FindCandidateGroupByTask(t *testing.T) {
	cs := &CommandState{
		CandidateGroups: map[string]*CandidateGroup{"grp_1": testGroup()},
	}

	id, g := cs.FindCandidateGroupByTask("task_B")
	if id != "grp_1" || g == nil {
		t.Errorf("FindCandidateGroupByTask(task_B) = (%q, %v), want (grp_1, group)", id, g)
	}
	if id, g := cs.FindCandidateGroupByTask("task_X"); id != "" || g != nil {
		t.Errorf("FindCandidateGroupByTask(unknown) = (%q, %v), want empty", id, g)
	}

	var nilState *CommandState
	if id, g := nilState.FindCandidateGroupByTask("task_A"); id != "" || g != nil {
		t.Error("nil state must return empty")
	}
}

func TestCommandState_ABBarrierActive(t *testing.T) {
	cs := &CommandState{
		CandidateGroups: map[string]*CandidateGroup{"grp_1": testGroup()},
	}

	if !cs.ABBarrierActive("task_A") || !cs.ABBarrierActive("task_B") {
		t.Error("barrier must be active for racing group members")
	}
	if cs.ABBarrierActive("task_X") {
		t.Error("barrier must be inactive for non-candidates")
	}

	cs.CandidateGroups["grp_1"].Status = ABGroupResolved
	if cs.ABBarrierActive("task_A") {
		t.Error("barrier must lift once the group is resolved")
	}
	cs.CandidateGroups["grp_1"].Status = ABGroupDegraded
	if cs.ABBarrierActive("task_A") {
		t.Error("barrier must lift once the group is degraded")
	}
}

func TestABTestConfig_Defaults(t *testing.T) {
	var a ABTestConfig
	if a.EffectiveEnabled() {
		t.Error("default Enabled should be false")
	}
	if v := a.EffectiveMinBloomLevel(); v != DefaultABMinBloomLevel {
		t.Errorf("EffectiveMinBloomLevel() = %d, want %d", v, DefaultABMinBloomLevel)
	}
	if v := a.EffectiveTimeoutSec(); v != 0 {
		t.Errorf("EffectiveTimeoutSec() = %d, want 0", v)
	}
	if v := a.EffectiveSelectionTimeoutSec(); v != DefaultABSelectionTimeoutSec {
		t.Errorf("EffectiveSelectionTimeoutSec() = %d, want %d", v, DefaultABSelectionTimeoutSec)
	}
}

func TestABTestConfig_Configured(t *testing.T) {
	a := ABTestConfig{
		Enabled:             ptr.Bool(true),
		MinBloomLevel:       ptr.Int(5),
		TimeoutSec:          ptr.Int(600),
		SelectionTimeoutSec: ptr.Int(900),
	}
	if !a.EffectiveEnabled() || a.EffectiveMinBloomLevel() != 5 ||
		a.EffectiveTimeoutSec() != 600 || a.EffectiveSelectionTimeoutSec() != 900 {
		t.Errorf("configured values not honored: %+v", a)
	}
}

func TestABTestConfig_CrossTestPatterns(t *testing.T) {
	cfg := ABTestConfig{CrossTestPatterns: []string{"*.mytest", "*_test.go"}}
	pats := cfg.EffectiveCrossTestPatterns()
	seen := map[string]bool{}
	for _, p := range pats {
		if seen[p] {
			t.Errorf("duplicate pattern %q", p)
		}
		seen[p] = true
	}
	if !seen["*.mytest"] || !seen["*_test.go"] {
		t.Errorf("patterns missing additions: %v", pats)
	}
}

func TestValidate_CrossTestPatterns(t *testing.T) {
	check := func(pattern string) error {
		var cfg Config
		cfg.ABTest.CrossTestPatterns = []string{pattern}
		var errs []error
		cfg.validateExperimental(&errs)
		if len(errs) > 0 {
			return errs[0]
		}
		return nil
	}
	if err := check("dir/*.go"); err == nil {
		t.Error("pattern with '/' must be rejected")
	}
	if err := check("[invalid"); err == nil {
		t.Error("invalid glob must be rejected")
	}
	if err := check("*.mytest"); err != nil {
		t.Errorf("valid pattern rejected: %v", err)
	}
}
