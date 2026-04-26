package apipolicy

import (
	"fmt"
	"strings"

	"github.com/msageha/maestro_v2/internal/uds"
)

func NormalizedCallerRole(req *uds.Request) (string, *uds.Response) {
	role := ""
	if req != nil {
		role = req.CallerRole
	}
	if err := uds.ValidateCallerRole(role); err != nil {
		return "", uds.ErrorResponse(uds.ErrCodeValidation, err.Error())
	}
	if role == "" {
		return "", uds.ErrorResponse(uds.ErrCodeValidation, "caller_role is required")
	}
	return role, nil
}

func RequireCallerRole(req *uds.Request, operation string, allowed ...string) *uds.Response {
	role, resp := NormalizedCallerRole(req)
	if resp != nil {
		return resp
	}
	for _, a := range allowed {
		if role == a {
			return nil
		}
	}
	return uds.ErrorResponse(uds.ErrCodeValidation,
		fmt.Sprintf("operation %q is not permitted for caller role %q; allowed roles: %s",
			operation, role, strings.Join(allowed, ", ")))
}
