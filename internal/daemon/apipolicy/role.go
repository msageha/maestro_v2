// Package apipolicy enforces role-based access on UDS API requests.
// All daemon handlers must run their request through one of these helpers
// before mutating durable state so unauthenticated callers cannot reach
// privileged endpoints.
package apipolicy

import (
	"fmt"
	"strings"

	"github.com/msageha/maestro_v2/internal/uds"
)

// NormalizedCallerRole extracts and validates the caller role from req.
// On a missing or invalid role it returns an empty string and a populated
// *uds.Response that the caller must surface verbatim. A nil response
// signals the role is well-formed and present.
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

// RequireCallerRole returns nil when req's caller role is one of allowed,
// or a *uds.Response with a permission-denied error otherwise. operation
// is included in the error message to aid log triage.
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
