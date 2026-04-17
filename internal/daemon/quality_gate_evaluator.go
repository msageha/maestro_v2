package daemon

// This file re-exports types from internal/daemon/dispatch so that existing code
// referencing daemon.QualityGateEvaluator, daemon.NewQualityGateEvaluator, etc.
// continues to compile.

import (
	"github.com/msageha/maestro_v2/internal/daemon/dispatch"
)

// QualityGateEvaluator is an alias for dispatch.QualityGateEvaluator.
type QualityGateEvaluator = dispatch.QualityGateEvaluator

// NewQualityGateEvaluator is re-exported from dispatch.
var NewQualityGateEvaluator = dispatch.NewQualityGateEvaluator
