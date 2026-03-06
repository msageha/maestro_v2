package daemon

import (
	"github.com/msageha/maestro_v2/internal/daemon/lease"
)

// LeaseManager is an alias for lease.Manager.
type LeaseManager = lease.Manager

// LeaseInfo is an alias for lease.Info.
type LeaseInfo = lease.Info

// NewLeaseManager delegates to lease.NewManager.
var NewLeaseManager = lease.NewManager

// NewLeaseManagerWithDeps delegates to lease.NewManagerWithDeps.
var NewLeaseManagerWithDeps = lease.NewManagerWithDeps

func ptrStr(s *string) string {
	return lease.PtrStr(s)
}
