package model

import (
	"crypto/sha256"
	"encoding/hex"
)

// ComputeConflictGeneration returns a deterministic 16-character hex token
// derived from sha256(integrationHEAD|workerBranchSHA|phaseID|workerID). The
// token is used as a CAS guard for the conflict-resolution lifecycle: a
// resolver must echo back the same generation it observed, otherwise the
// daemon refuses to apply the resolution because the underlying refs have
// shifted under it.
func ComputeConflictGeneration(integrationHEAD, workerBranchSHA, phaseID, workerID string) string {
	h := sha256.Sum256([]byte(integrationHEAD + "|" + workerBranchSHA + "|" + phaseID + "|" + workerID))
	return hex.EncodeToString(h[:])[:16]
}
