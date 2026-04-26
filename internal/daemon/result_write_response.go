package daemon

import "github.com/msageha/maestro_v2/internal/uds"

type resultWriteResponse struct {
	ResultID      string
	Duplicate     bool
	VerifyPending bool
	LeaseRejectID string
}

func newResultWriteResponse(data resultWriteResponse) *uds.Response {
	payload := map[string]string{"result_id": data.ResultID}
	if data.VerifyPending {
		payload["verification"] = "pending"
	}
	if data.Duplicate {
		payload["duplicate"] = "true"
	}
	if data.LeaseRejectID != "" {
		payload["lease_rejection_id"] = data.LeaseRejectID
		payload["lease_rejection_warning"] =
			"learnings/skill_candidates rejected: lease revoked; recorded as " + data.LeaseRejectID
	}
	return uds.SuccessResponse(payload)
}
